// Copyright 2026 fxTunnel contributors. Licensed under Apache-2.0.
package receipts

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
)

type YooKassa struct {
	client          *http.Client
	base, shop, key string
}

func NewYooKassa(c Config) *YooKassa {
	return &YooKassa{newHTTPClient(c.RequestTimeout, c.YooSourceIP), "https://api.yookassa.ru/v3", c.ShopID, c.APIKey}
}
func (y *YooKassa) get(ctx context.Context, path string, q url.Values, out any) error {
	address := y.base + path
	if len(q) > 0 {
		address += "?" + q.Encode()
	}
	r, err := jsonRequest(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	r.SetBasicAuth(y.shop, y.key)
	return execute(y.client, r, "yookassa", false, out)
}
func (y *YooKassa) Payments(ctx context.Context, from, to time.Time, visit func(Payment) error) error {
	q := url.Values{"captured_at.gte": {from.UTC().Format(time.RFC3339Nano)}, "captured_at.lte": {to.UTC().Format(time.RFC3339Nano)}, "status": {"succeeded"}, "limit": {"100"}}
	seen := map[string]bool{}
	for page := 0; page < 10000; page++ {
		var res struct {
			Items *[]Payment `json:"items"`
			Next  string     `json:"next_cursor"`
		}
		if err := y.get(ctx, "/payments", q, &res); err != nil {
			return err
		}
		if res.Items == nil {
			return errors.New("yookassa payment list lacks items")
		}
		for _, p := range *res.Items {
			if err := visit(p); err != nil {
				return err
			}
		}
		if res.Next == "" {
			return nil
		}
		if seen[res.Next] {
			return errors.New("yookassa repeated payment cursor")
		}
		seen[res.Next] = true
		q.Set("cursor", res.Next)
	}
	return errors.New("yookassa payment pagination limit")
}
func (y *YooKassa) Refunds(ctx context.Context, from, to time.Time, visit func(Refund) error) error {
	// Do not filter by status: a pending refund is persisted and polled by ID
	// after the scan checkpoint passes its creation time.
	q := url.Values{"created_at.gte": {from.UTC().Format(time.RFC3339Nano)}, "created_at.lte": {to.UTC().Format(time.RFC3339Nano)}, "limit": {"100"}}
	seen := map[string]bool{}
	for page := 0; page < 10000; page++ {
		var res struct {
			Items *[]Refund `json:"items"`
			Next  string    `json:"next_cursor"`
		}
		if err := y.get(ctx, "/refunds", q, &res); err != nil {
			return err
		}
		if res.Items == nil {
			return errors.New("yookassa refund list lacks items")
		}
		for _, r := range *res.Items {
			if err := visit(r); err != nil {
				return err
			}
		}
		if res.Next == "" {
			return nil
		}
		if seen[res.Next] {
			return errors.New("yookassa repeated refund cursor")
		}
		seen[res.Next] = true
		q.Set("cursor", res.Next)
	}
	return errors.New("yookassa refund pagination limit")
}
func (y *YooKassa) Refund(ctx context.Context, id string) (Refund, error) {
	var r Refund
	err := y.get(ctx, "/refunds/"+url.PathEscape(id), nil, &r)
	if err == nil && r.ID != id {
		return r, errors.New("yookassa refund ID mismatch")
	}
	return r, err
}

// Payment fetches a single payment without enumerating shop history.
func (y *YooKassa) Payment(ctx context.Context, id string) (Payment, error) {
	var p Payment
	if id == "" {
		return p, errors.New("payment ID is required")
	}
	err := y.get(ctx, "/payments/"+url.PathEscape(id), nil, &p)
	if err == nil && p.ID != id {
		return p, errors.New("yookassa payment ID mismatch")
	}
	return p, err
}
