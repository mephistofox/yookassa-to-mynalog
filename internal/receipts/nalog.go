// Copyright 2026 fxTunnel contributors. Licensed under Apache-2.0.
// API payloads adapted from upstream app/nalog_api.py (Apache-2.0).
package receipts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type Nalog struct {
	client         *http.Client
	base           string
	config         Config
	store          *Store
	token, refresh string
	authNext       time.Time
	lastRequest    time.Time
}
type tokenState struct{ Device, SeedHash, Token string }

func seedHash(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func NewNalog(c Config, s *Store) (*Nalog, error) {
	n := &Nalog{client: newHTTPClient(c.RequestTimeout, c.NalogSourceIP), base: "https://lknpd.nalog.ru/api/v1", config: c, store: s, refresh: c.RefreshToken}
	v, err := s.Meta("auth.refresh")
	if err != nil {
		return nil, err
	}
	if v != "" {
		var saved tokenState
		if err = json.Unmarshal([]byte(v), &saved); err != nil {
			return nil, errors.New("corrupt saved refresh token")
		}
		if saved.Device == c.DeviceID && saved.SeedHash == seedHash(c.RefreshToken) && saved.Token != "" {
			n.refresh = saved.Token
		}
	}
	return n, nil
}
func (n *Nalog) Authenticate(ctx context.Context) error {
	if time.Now().Before(n.authNext) {
		return &APIError{Service: "nalog auth cooling down", Retryable: true}
	}
	device := map[string]any{"sourceDeviceId": n.config.DeviceID, "sourceType": "WEB", "appVersion": "1.0.0", "metaDetails": map[string]string{"userAgent": "Mozilla/5.0"}}
	if err := n.pace(ctx); err != nil {
		return &APIError{Service: "nalog auth pacing cancelled", Retryable: true}
	}
	payload := map[string]any{"deviceInfo": device}
	path := "/auth/token"
	if n.config.AuthMethod == "password" {
		path = "/auth/lkfl"
		payload["username"] = n.config.Login
		payload["password"] = n.config.Password
	} else {
		payload["refreshToken"] = n.refresh
	}
	r, err := jsonRequest(ctx, http.MethodPost, n.base+path, payload)
	if err != nil {
		return err
	}
	r.Header.Set("Origin", "https://lknpd.nalog.ru")
	r.Header.Set("Referer", "https://lknpd.nalog.ru/")
	var response struct {
		Token   string `json:"token"`
		Refresh string `json:"refreshToken"`
		Profile struct {
			INN string `json:"inn"`
		} `json:"profile"`
	}
	if err = execute(n.client, r, "nalog auth", false, &response); err != nil {
		n.authNext = time.Now().Add(time.Minute)
		return err
	}
	if response.Profile.INN != "" && response.Profile.INN != n.config.INN {
		n.authNext = time.Now().Add(5 * time.Minute)
		return errors.New("nalog account INN does not match configured INN")
	}
	if response.Refresh != "" && response.Refresh != n.refresh {
		b, _ := json.Marshal(tokenState{n.config.DeviceID, seedHash(n.config.RefreshToken), response.Refresh})
		// Fail closed if a rotated credential cannot be persisted durably.
		if err = n.store.SetMeta("auth.refresh", string(b)); err != nil {
			return errors.New("cannot persist rotated refresh token")
		}
		n.refresh = response.Refresh
	}
	if response.Token == "" {
		return &APIError{Service: "nalog auth", Status: 200, Retryable: true}
	}
	n.token = response.Token
	n.authNext = time.Time{}
	return nil
}
func (n *Nalog) request(ctx context.Context, method, path string, value any, write bool, out any) error {
	if n.token == "" {
		if err := n.Authenticate(ctx); err != nil {
			return err
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := n.pace(ctx); err != nil {
			return &APIError{Service: "nalog pacing cancelled", Retryable: true}
		}
		r, err := jsonRequest(ctx, method, n.base+path, value)
		if err != nil {
			return err
		}
		r.Header.Set("Authorization", "Bearer "+n.token)
		r.Header.Set("Origin", "https://lknpd.nalog.ru")
		r.Header.Set("Referer", "https://lknpd.nalog.ru/")
		err = execute(n.client, r, "nalog", write, out)
		var api *APIError
		if errors.As(err, &api) && api.Status == 401 && attempt == 0 {
			n.token = ""
			if authErr := n.Authenticate(ctx); authErr != nil {
				return authErr
			}
			continue
		}
		return err
	}
	return errors.New("nalog authentication failed")
}
func (n *Nalog) Create(ctx context.Context, name string, amount Money, date time.Time) (string, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	payload := map[string]any{
		"operationTime": date.UTC().Format(time.RFC3339), "requestTime": now,
		"services": []map[string]any{{"name": name, "amount": amount.String(), "quantity": 1}}, "totalAmount": amount.String(),
		"client":      map[string]any{"contactPhone": nil, "displayName": nil, "inn": nil, "incomeType": "FROM_INDIVIDUAL"},
		"paymentType": "CASH", "ignoreMaxTotalIncomeRestriction": false,
	}
	var response struct {
		ID string `json:"approvedReceiptUuid"`
	}
	if err := n.request(ctx, http.MethodPost, "/income", payload, true, &response); err != nil {
		return "", err
	}
	if response.ID == "" {
		return "", &APIError{Service: "nalog missing receipt ID", Status: 200, Retryable: true, Uncertain: true}
	}
	return response.ID, nil
}

func (n *Nalog) pace(ctx context.Context) error {
	delay := time.Until(n.lastRequest.Add(n.config.FNSInterval))
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	n.lastRequest = time.Now()
	return nil
}
func (n *Nalog) Cancel(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	payload := map[string]any{"operationTime": now, "requestTime": now, "comment": "Возврат средств", "receiptUuid": id}
	var response json.RawMessage
	return n.request(ctx, http.MethodPost, "/cancel", payload, true, &response)
}
func (n *Nalog) incomes(ctx context.Context, date time.Time, visit func(Income) error) error {
	q := url.Values{"from": {date.Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)}, "to": {date.Add(24 * time.Hour).UTC().Format(time.RFC3339Nano)}, "sortBy": {"operation_time:desc"}, "limit": {"50"}}
	seen := map[string]bool{}
	for page := 0; page < 1000; page++ {
		q.Set("offset", strconv.Itoa(page*50))
		var response struct {
			Content *[]Income `json:"content"`
		}
		if err := n.request(ctx, http.MethodGet, "/incomes?"+q.Encode(), nil, false, &response); err != nil {
			return err
		}
		if response.Content == nil {
			return errors.New("nalog income list lacks content")
		}
		ids := ""
		for _, i := range *response.Content {
			ids += i.UUID() + ";"
		}
		if len(*response.Content) > 0 && seen[ids] {
			return errors.New("nalog repeated income page")
		}
		seen[ids] = true
		for _, i := range *response.Content {
			if err := visit(i); err != nil {
				return err
			}
		}
		if len(*response.Content) < 50 {
			return nil
		}
	}
	return errors.New("nalog income pagination limit")
}
func (n *Nalog) Find(ctx context.Context, name string, amount Money, date time.Time) (string, error) {
	var found string
	err := n.incomes(ctx, date, func(i Income) error {
		if i.Cancelled() || i.Name != name {
			return nil
		}
		m, err := i.Money()
		if err != nil {
			return fmt.Errorf("nalog income amount invalid")
		}
		if m == amount && i.UUID() != "" {
			if found != "" && found != i.UUID() {
				return errors.New("multiple matching nalog receipts require manual review")
			}
			found = i.UUID()
		}
		return nil
	})
	return found, err
}
func (n *Nalog) Status(ctx context.Context, id string, date time.Time) (string, error) {
	status := "missing"
	err := n.incomes(ctx, date, func(i Income) error {
		if i.UUID() == id {
			status = "active"
			if i.Cancelled() {
				status = "cancelled"
			}
		}
		return nil
	})
	return status, err
}
