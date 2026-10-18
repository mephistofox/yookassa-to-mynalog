package receipts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestYooKassaUsesCapturedTimeAndAllPages(t *testing.T) {
	from := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		user, pass, ok := r.BasicAuth()
		if !ok || user != "shop" || pass != "secret" {
			t.Error("invalid basic auth")
		}
		q := r.URL.Query()
		if q.Get("captured_at.gte") != from.Format(time.RFC3339) || q.Get("created_at.gte") != "" || q.Get("status") != "succeeded" {
			t.Error("late capture would be lost")
		}
		if q.Get("cursor") == "" {
			writeJSON(w, map[string]any{"items": []Payment{paymentAt("one", from)}, "next_cursor": "next"})
		} else {
			writeJSON(w, map[string]any{"items": []Payment{paymentAt("two", to)}})
		}
	}))
	defer server.Close()
	y := &YooKassa{server.Client(), server.URL, "shop", "secret"}
	var ids []string
	err := y.Payments(context.Background(), from, to, func(p Payment) error { ids = append(ids, p.ID); return nil })
	if err != nil || requests != 2 || len(ids) != 2 {
		t.Fatalf("pagination failed %v", err)
	}
}
func TestYooKassaRefusesRepeatedCursorAndMissingItems(t *testing.T) {
	for _, mode := range []string{"cursor", "missing"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "missing" {
					writeJSON(w, map[string]any{})
					return
				}
				writeJSON(w, map[string]any{"items": []Payment{}, "next_cursor": "same"})
			}))
			defer server.Close()
			y := &YooKassa{server.Client(), server.URL, "shop", "key"}
			if err := y.Payments(context.Background(), time.Now(), time.Now(), func(Payment) error { return nil }); err == nil {
				t.Fatal("incomplete pagination accepted")
			}
		})
	}
}
func TestYooKassaRefundScanIncludesPending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") != "" {
			t.Error("pending refund would be lost")
		}
		if r.URL.Query().Get("created_at.gte") == "" {
			t.Error("missing refund window")
		}
		writeJSON(w, map[string]any{"items": []Refund{{ID: "r", Status: "pending"}}})
	}))
	defer server.Close()
	y := &YooKassa{server.Client(), server.URL, "shop", "key"}
	count := 0
	if err := y.Refunds(context.Background(), time.Now(), time.Now(), func(r Refund) error { count++; return nil }); err != nil || count != 1 {
		t.Fatal(err)
	}
}
func TestHTTPDoesNotFollowRedirectWithCredentials(t *testing.T) {
	hit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer server.Close()
	y := &YooKassa{newHTTPClient(time.Second, ""), server.URL, "shop", "secret"}
	if err := y.Payments(context.Background(), time.Now(), time.Now(), func(Payment) error { return nil }); err == nil || hit {
		t.Fatal("redirect followed")
	}
}
