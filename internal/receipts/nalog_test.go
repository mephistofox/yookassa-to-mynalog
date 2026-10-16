package receipts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testNalog(t *testing.T, handler http.HandlerFunc) (*Nalog, *Store, *httptest.Server) {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c := Config{AuthMethod: "refresh", RefreshToken: "seed-secret", DeviceID: "browser-device", INN: "123456789012", RequestTimeout: time.Second}
	n, err := NewNalog(c, s)
	if err != nil {
		t.Fatal(err)
	}
	n.base = server.URL
	n.client = server.Client()
	return n, s, server
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
func TestNalogRefreshRotationAndCreateContract(t *testing.T) {
	posts := 0
	n, s, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/auth/token":
			var refresh string
			json.Unmarshal(body["refreshToken"], &refresh)
			if refresh != "seed-secret" {
				t.Error("wrong refresh token")
			}
			var device map[string]any
			json.Unmarshal(body["deviceInfo"], &device)
			if device["sourceDeviceId"] != "browser-device" {
				t.Error("device binding lost")
			}
			writeJSON(w, map[string]any{"token": "access-secret", "refreshToken": "rotated-secret", "profile": map[string]string{"inn": "123456789012"}})
		case "/income":
			posts++
			if r.Header.Get("Authorization") != "Bearer access-secret" {
				t.Error("no access token")
			}
			if string(body["totalAmount"]) != "\"10.10\"" || string(body["paymentType"]) != "\"CASH\"" {
				t.Errorf("unexpected amount/payment type: %s %s", body["totalAmount"], body["paymentType"])
			}
			var date string
			json.Unmarshal(body["operationTime"], &date)
			if date != "2026-10-17T10:00:00Z" {
				t.Error("wrong income date")
			}
			writeJSON(w, map[string]string{"approvedReceiptUuid": "r1"})
		default:
			t.Error("unexpected endpoint")
			w.WriteHeader(404)
		}
	})
	id, err := n.Create(context.Background(), "fxTunnel [yookassa:one]", 1010, time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC))
	if err != nil || id != "r1" || posts != 1 {
		t.Fatalf("%s %v", id, err)
	}
	restarted, err := NewNalog(n.config, s)
	if err != nil || restarted.refresh != "rotated-secret" {
		t.Fatal("rotated token not restored")
	}
	c := n.config
	c.RefreshToken = "manually-replaced-secret"
	reseeded, err := NewNalog(c, s)
	if err != nil || reseeded.refresh != c.RefreshToken {
		t.Fatal("explicit reseed ignored")
	}
	c = n.config
	c.DeviceID = "other-device"
	rebound, err := NewNalog(c, s)
	if err != nil || rebound.refresh != "seed-secret" {
		t.Fatal("rotated token reused with wrong device")
	}
}
func TestNalogWriteFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name           string
		code           int
		body           string
		retry, unknown bool
	}{{"rate limit", 429, "secret body", true, false}, {"validation", 400, "private data", false, false}, {"server", 503, "maintenance", true, true}, {"html success", 200, "<html>maintenance</html>", true, true}, {"missing receipt", 200, "{}", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			n, _, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(tc.code); fmt.Fprint(w, tc.body) })
			n.token = "access"
			_, err := n.Create(context.Background(), "name", 100, time.Now())
			if err == nil || retryable(err) != tc.retry || uncertain(err) != tc.unknown || calls != 1 {
				t.Fatalf("wrong failure %v retry=%v uncertain=%v calls=%d", err, retryable(err), uncertain(err), calls)
			}
			if strings.Contains(err.Error(), tc.body) {
				t.Fatal("remote body leaked into error")
			}
		})
	}
}
func TestNalogReauthOnceOn401(t *testing.T) {
	posts, auths := 0, 0
	n, _, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			auths++
			writeJSON(w, map[string]string{"token": "fresh"})
			return
		}
		posts++
		if r.Header.Get("Authorization") == "Bearer stale" {
			w.WriteHeader(401)
			return
		}
		writeJSON(w, map[string]string{"approvedReceiptUuid": "r1"})
	})
	n.token = "stale"
	id, err := n.Create(context.Background(), "name", 100, time.Now())
	if err != nil || id != "r1" || posts != 2 || auths != 1 {
		t.Fatalf("reauth failed %v %d %d", err, posts, auths)
	}
}
func TestNalogAuthFailureDoesNotMakeIncomeUncertain(t *testing.T) {
	posts := 0
	n, _, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/income" {
			posts++
		}
		w.WriteHeader(503)
	})
	_, err := n.Create(context.Background(), "name", 100, time.Now())
	if err == nil || uncertain(err) || !retryable(err) || posts != 0 {
		t.Fatal("auth failure misclassified as attempted income")
	}
}
func TestNalogFindPaginatesAndExcludesCancelled(t *testing.T) {
	pages := 0
	n, _, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) {
		pages++
		if r.URL.Query().Get("limit") != "50" || r.URL.Query().Get("from") == "" {
			t.Error("missing bounded lookup")
		}
		var content []map[string]any
		if r.URL.Query().Get("offset") == "0" {
			for i := 0; i < 50; i++ {
				content = append(content, map[string]any{"approvedReceiptUuid": fmt.Sprint(i), "name": "target", "totalAmount": "10.10", "cancellationInfo": map[string]string{"reason": "refund"}})
			}
		} else {
			content = []map[string]any{{"approvedReceiptUuid": "found", "name": "target", "totalAmount": json.Number("10.10")}}
		}
		writeJSON(w, map[string]any{"content": content})
	})
	n.token = "access"
	id, err := n.Find(context.Background(), "target", 1010, time.Now())
	if err != nil || id != "found" || pages != 2 {
		t.Fatalf("lookup %s %v pages=%d", id, err, pages)
	}
}
func TestNalogRepeatedPageAndMissingContentAreErrors(t *testing.T) {
	for _, mode := range []string{"repeated", "missing"} {
		t.Run(mode, func(t *testing.T) {
			n, _, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) {
				if mode == "missing" {
					writeJSON(w, map[string]any{})
					return
				}
				items := make([]map[string]string, 50)
				for i := range items {
					items[i] = map[string]string{"approvedReceiptUuid": fmt.Sprint(i), "name": "other", "totalAmount": "1.00"}
				}
				writeJSON(w, map[string]any{"content": items})
			})
			n.token = "access"
			if _, err := n.Find(context.Background(), "target", 100, time.Now()); err == nil {
				t.Fatal("incomplete lookup treated as proof of absence")
			}
		})
	}
}
func TestNalogRejectsDuplicateMatchesAndWrongAccount(t *testing.T) {
	n, _, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			writeJSON(w, map[string]any{"token": "access", "profile": map[string]string{"inn": "999999999999"}})
			return
		}
		writeJSON(w, map[string]any{"content": []map[string]string{{"approvedReceiptUuid": "a", "name": "target", "totalAmount": "1.00"}, {"approvedReceiptUuid": "b", "name": "target", "totalAmount": "1.00"}}})
	})
	if err := n.Authenticate(context.Background()); err == nil {
		t.Fatal("wrong taxpayer account accepted")
	}
	n.token = "access"
	if _, err := n.Find(context.Background(), "target", 100, time.Now()); err == nil {
		t.Fatal("ambiguous matching receipts accepted")
	}
}
func TestNalogLostConnectionAfterWriteIsUncertain(t *testing.T) {
	n, _, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) {
		h := w.(http.Hijacker)
		conn, _, err := h.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	})
	n.token = "access"
	_, err := n.Create(context.Background(), "name", 100, time.Now())
	var api *APIError
	if !errors.As(err, &api) || !api.Uncertain {
		t.Fatal("lost connection treated as safe to repeat")
	}
}
