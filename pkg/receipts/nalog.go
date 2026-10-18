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
	"strings"
	"time"
)

type Nalog struct {
	client          *http.Client
	base            string
	config          Config
	store           StateStore
	token, refresh  string
	verifiedRefresh string
	pendingRefresh  string
	authNext        time.Time
	lastRequest     time.Time
}
type tokenState struct{ Device, SeedHash, Token, PendingToken string }

func seedHash(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func NewNalog(c Config, s StateStore) (*Nalog, error) {
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
		if saved.Device == c.DeviceID && saved.SeedHash == seedHash(c.RefreshToken) {
			n.verifiedRefresh = saved.Token
			n.pendingRefresh = saved.PendingToken
			if saved.PendingToken != "" {
				n.refresh = saved.PendingToken
			} else if saved.Token != "" {
				n.refresh = saved.Token
			}
		}
	}
	return n, nil
}
func (n *Nalog) Authenticate(ctx context.Context) error {
	n.token = ""
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
	if n.config.INN == "" || (response.Profile.INN != "" && response.Profile.INN != n.config.INN) {
		if n.pendingRefresh != "" {
			if err = n.saveRefresh(n.verifiedRefresh, ""); err != nil {
				return err
			}
		}
		n.authNext = time.Now().Add(5 * time.Minute)
		return errors.New("nalog account INN does not match configured INN")
	}
	candidate := n.refresh
	if response.Refresh != "" {
		candidate = response.Refresh
	}
	// Keep rotation recoverable without promoting an account whose identity is unverified.
	if response.Profile.INN == "" || response.Token == "" {
		if candidate != n.verifiedRefresh {
			if err = n.saveRefresh(n.verifiedRefresh, candidate); err != nil {
				return err
			}
		}
	}
	if response.Token == "" {
		return &APIError{Service: "nalog auth", Status: 200, Retryable: true}
	}
	identity := response.Profile.INN
	if identity == "" {
		identity, err = n.userINN(ctx, response.Token)
		if err != nil {
			n.authNext = time.Now().Add(time.Minute)
			return err
		}
		if identity == "" {
			n.authNext = time.Now().Add(time.Minute)
			return errors.New("nalog user profile lacks taxpayer INN")
		}
		if identity != n.config.INN {
			if err = n.saveRefresh(n.verifiedRefresh, ""); err != nil {
				return err
			}
			n.authNext = time.Now().Add(5 * time.Minute)
			return errors.New("nalog account INN does not match configured INN")
		}
	}
	// A failed promotion never grants access, even after successful identity verification.
	if candidate != n.verifiedRefresh || n.pendingRefresh != "" {
		if err = n.saveRefresh(candidate, ""); err != nil {
			return err
		}
	}
	n.token = response.Token
	n.authNext = time.Time{}
	return nil
}
func (n *Nalog) saveRefresh(verified, pending string) error {
	b, err := json.Marshal(tokenState{Device: n.config.DeviceID, SeedHash: seedHash(n.config.RefreshToken), Token: verified, PendingToken: pending})
	if err != nil {
		return errors.New("cannot encode rotated refresh token")
	}
	if err = n.store.SetMeta("auth.refresh", string(b)); err != nil {
		return errors.New("cannot persist rotated refresh token")
	}
	n.verifiedRefresh = verified
	n.pendingRefresh = pending
	n.refresh = pending
	if n.refresh == "" {
		n.refresh = verified
	}
	if n.refresh == "" {
		n.refresh = n.config.RefreshToken
	}
	return nil
}
func (n *Nalog) userINN(ctx context.Context, access string) (string, error) {
	if err := n.pace(ctx); err != nil {
		return "", &APIError{Service: "nalog profile pacing cancelled", Retryable: true}
	}
	r, err := jsonRequest(ctx, http.MethodGet, n.base+"/user", nil)
	if err != nil {
		return "", err
	}
	r.Header.Set("Authorization", "Bearer "+access)
	r.Header.Set("Origin", "https://lknpd.nalog.ru")
	r.Header.Set("Referer", "https://lknpd.nalog.ru/")
	var profile struct {
		INN string `json:"inn"`
	}
	// This probe must not call request: a 401 must not recurse into Authenticate.
	if err = execute(n.client, r, "nalog profile", false, &profile); err != nil {
		return "", err
	}
	return profile.INN, nil
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

// Inspect distinguishes exact payment markers from other same-day income.
// A matching amount alone never identifies an existing receipt.
func (n *Nalog) Inspect(ctx context.Context, name string, amount Money, date time.Time) (string, bool, error) {
	var found string
	conflict := false
	moscow := time.FixedZone("Europe/Moscow", 3*60*60)
	day := date.In(moscow).Format("2006-01-02")
	err := n.incomes(ctx, date, func(i Income) error {
		if i.Cancelled() {
			return nil
		}
		if i.OperationTime.IsZero() {
			return errors.New("nalog income date is missing")
		}
		if i.OperationTime.In(moscow).Format("2006-01-02") != day {
			return nil
		}
		m, err := i.Money()
		if err != nil {
			return fmt.Errorf("nalog income amount invalid")
		}
		if i.Name == name && m == amount && i.UUID() != "" {
			if found != "" && found != i.UUID() {
				return errors.New("multiple matching nalog receipts require manual review")
			}
			found = i.UUID()
		} else if m == amount || i.Name == name {
			known, err := n.knownOtherReceipt(i, m)
			if err != nil {
				return err
			}
			if !known {
				conflict = true
			}
		}
		return nil
	})
	return found, conflict, err
}
func (n *Nalog) Find(ctx context.Context, name string, amount Money, date time.Time) (string, error) {
	id, conflict, err := n.Inspect(ctx, name, amount, date)
	if err == nil && conflict {
		err = errors.New("other matching nalog income requires manual review")
	}
	return id, err
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

// Receipt verifies an explicitly selected document, including a manual title.
// The caller must separately authorize the payment relationship and reject cancellation.
func (n *Nalog) Receipt(ctx context.Context, id string, amount Money, date time.Time) (Income, error) {
	var found Income
	seen := false
	moscow := time.FixedZone("Europe/Moscow", 3*60*60)
	err := n.incomes(ctx, date, func(i Income) error {
		if i.UUID() != id {
			return nil
		}
		if seen {
			return errors.New("multiple records for selected nalog receipt")
		}
		seen = true
		m, err := i.Money()
		if err != nil || m != amount || i.OperationTime.IsZero() || i.OperationTime.In(moscow).Format("2006-01-02") != date.In(moscow).Format("2006-01-02") || strings.TrimSpace(i.Name) == "" {
			return errors.New("selected nalog receipt amount, date or title mismatch")
		}
		found = i
		return nil
	})
	if err == nil && (!seen || id == "") {
		err = errors.New("selected nalog receipt not found")
	}
	return found, err
}
func (n *Nalog) knownOtherReceipt(i Income, m Money) (bool, error) {
	var bound PaymentJob
	explicit, err := n.store.Get("receipt_binding", i.UUID(), &bound)
	if err != nil {
		return false, err
	}
	if !explicit {
		marker := " [yookassa:"
		pos := strings.LastIndex(i.Name, marker)
		if pos < 0 || !strings.HasSuffix(i.Name, "]") {
			return false, nil
		}
		bound.ID = i.Name[pos+len(marker) : len(i.Name)-1]
		if bound.ID == "" || strings.ContainsAny(bound.ID, "[] \r\n") {
			return false, nil
		}
	}
	var actual PaymentJob
	ok, err := n.store.Get("payment", bound.ID, &actual)
	if err != nil {
		return false, err
	}
	moscow := time.FixedZone("Europe/Moscow", 3*60*60)
	if !ok || actual.ID != bound.ID || actual.Phase != "done" || actual.Receipt != i.UUID() || actual.Remaining != m || actual.Date.In(moscow).Format("2006-01-02") != i.OperationTime.In(moscow).Format("2006-01-02") {
		return false, nil
	}
	if !explicit {
		return actual.Description == i.Name, nil
	}
	return bound.Phase == "done" && bound.Receipt == actual.Receipt && bound.Amount == actual.Amount && bound.Remaining == actual.Remaining && bound.Date.Equal(actual.Date) && bound.Email == actual.Email && bound.Description == i.Name, nil
}
