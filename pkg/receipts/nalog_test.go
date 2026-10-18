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
			writeJSON(w, map[string]any{"token": "fresh", "profile": map[string]string{"inn": "123456789012"}})
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
			content = []map[string]any{{"approvedReceiptUuid": "found", "name": "target", "totalAmount": json.Number("10.10"), "operationTime": time.Now().UTC().Format(time.RFC3339)}}
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
					items[i] = map[string]string{"approvedReceiptUuid": fmt.Sprint(i), "name": "other", "totalAmount": "1.00", "operationTime": time.Now().UTC().Format(time.RFC3339)}
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
		writeJSON(w, map[string]any{"content": []map[string]string{{"approvedReceiptUuid": "a", "name": "target", "totalAmount": "1.00", "operationTime": time.Now().UTC().Format(time.RFC3339)}, {"approvedReceiptUuid": "b", "name": "target", "totalAmount": "1.00", "operationTime": time.Now().UTC().Format(time.RFC3339)}}})
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
func TestNalogReceiptVerifiesExplicitUUIDAmountDayAndTitle(t *testing.T) {
	date := time.Date(2026, 10, 17, 22, 30, 0, 0, time.UTC)
	for _, mode := range []string{"active", "cancelled", "missing", "amount", "day", "name", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			row := map[string]any{"approvedReceiptUuid": "chosen", "name": "Manual receipt title", "totalAmount": "10.00", "operationTime": "2026-10-18T01:30:00+03:00"}
			switch mode {
			case "cancelled":
				row["cancellationInfo"] = map[string]string{"reason": "refund"}
			case "missing":
				row["approvedReceiptUuid"] = "other"
			case "amount":
				row["totalAmount"] = "10.01"
			case "day":
				row["operationTime"] = "2026-10-17T23:59:00+03:00"
			case "name":
				row["name"] = ""
			}
			rows := []map[string]any{row}
			if mode == "duplicate" {
				rows = append(rows, row)
			}
			n, _, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{"content": rows}) })
			n.token = "access"
			i, err := n.Receipt(context.Background(), "chosen", 1000, date)
			if mode != "active" && mode != "cancelled" {
				if err == nil {
					t.Fatal("unverified receipt accepted")
				}
				return
			}
			if err != nil || i.UUID() != "chosen" || i.Name != "Manual receipt title" || i.Cancelled() != (mode == "cancelled") {
				t.Fatalf("bad receipt %+v %v", i, err)
			}
		})
	}
}
func TestInspectIgnoresOnlySecurelyBoundOtherReceipt(t *testing.T) {
	date := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	for _, mode := range []string{"unknown marker", "known marker", "stale marker", "unknown manual", "known manual", "stale manual"} {
		t.Run(mode, func(t *testing.T) {
			title := "Service [yookassa:other]"
			if strings.Contains(mode, "manual") {
				title = "Earlier manual receipt"
			}
			n, store, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, map[string]any{"content": []map[string]any{{"approvedReceiptUuid": "other-receipt", "name": title, "totalAmount": "10.00", "operationTime": date.Format(time.RFC3339)}}})
			})
			n.token = "access"
			if strings.HasPrefix(mode, "known") || strings.HasPrefix(mode, "stale") {
				job := PaymentJob{ID: "other", Phase: "done", Receipt: "other-receipt", Amount: 1000, Remaining: 1000, Date: date, Description: "Service [yookassa:other]", Email: "other@example.com"}
				binding := job
				binding.Description = title
				if strings.HasPrefix(mode, "stale") {
					job.Receipt = "new-receipt"
				}
				records := []Record{paymentRecord(&job)}
				if strings.Contains(mode, "manual") {
					records = append(records, Record{Kind: "receipt_binding", ID: "other-receipt", Phase: "verified", Value: binding})
				}
				if err := store.Save(records...); err != nil {
					t.Fatal(err)
				}
			}
			id, conflict, err := n.Inspect(context.Background(), "Service [yookassa:chosen]", 1000, date)
			wantConflict := !strings.HasPrefix(mode, "known")
			if err != nil || id != "" || conflict != wantConflict {
				t.Fatalf("id=%q conflict=%v want=%v err=%v", id, conflict, wantConflict, err)
			}
		})
	}
}

func TestNalogAuthWithoutEmbeddedProfileVerifiesAuthenticatedUserBeforeIncome(t *testing.T) {
	auths, profiles, posts := 0, 0, 0
	n, _, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token":
			auths++
			writeJSON(w, map[string]any{"token": "unverified-access", "tokenExpireIn": 3600, "refreshToken": "new-refresh", "refreshTokenExpiresIn": 86400})
		case "/user":
			profiles++
			if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer unverified-access" {
				t.Error("identity probe did not use new bearer token")
			}
			writeJSON(w, map[string]string{"inn": "123456789012"})
		case "/income":
			posts++
			if profiles != 1 {
				t.Error("income preceded identity proof")
			}
			writeJSON(w, map[string]string{"approvedReceiptUuid": "verified-receipt"})
		default:
			t.Error("unexpected endpoint")
			w.WriteHeader(404)
		}
	})
	id, err := n.Create(context.Background(), "payment", 100, time.Now())
	if err != nil || id != "verified-receipt" || auths != 1 || profiles != 1 || posts != 1 {
		t.Fatalf("identity fallback failed auth=%d profiles=%d posts=%d err=%v", auths, profiles, posts, err)
	}
}
func TestNalogProfileFallbackFailsClosedWithoutRecursiveAuthentication(t *testing.T) {
	for _, mode := range []string{"unauthorized", "unavailable", "missing", "wrong", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			auths, profiles, posts := 0, 0, 0
			n, _, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/auth/token":
					auths++
					writeJSON(w, map[string]string{"token": "candidate", "refreshToken": "rotated"})
				case "/user":
					profiles++
					switch mode {
					case "unauthorized":
						w.WriteHeader(401)
					case "unavailable":
						w.WriteHeader(503)
					case "missing":
						writeJSON(w, map[string]string{})
					case "wrong":
						writeJSON(w, map[string]string{"inn": "999999999999"})
					case "malformed":
						writeJSON(w, map[string]int{"inn": 123})
					}
				case "/income":
					posts++
					writeJSON(w, map[string]string{"approvedReceiptUuid": "must-not-exist"})
				}
			})
			if _, err := n.Create(context.Background(), "payment", 100, time.Now()); err == nil || uncertain(err) {
				t.Fatalf("identity failure accepted/misclassified: %v", err)
			}
			if auths != 1 || profiles != 1 || posts != 0 || n.token != "" {
				t.Fatalf("unsafe fallback auth=%d profile=%d financial=%d", auths, profiles, posts)
			}
		})
	}
}
func TestNalogPendingRefreshSurvivesProbeFailureAndRequiresIdentityOnRestart(t *testing.T) {
	profileOK := false
	auths, posts := 0, 0
	n, s, server := testNalog(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token":
			var payload map[string]any
			if json.NewDecoder(r.Body).Decode(&payload) != nil {
				t.Error("invalid auth payload")
			}
			want := "seed-secret"
			if auths > 0 {
				want = "pending-refresh"
			}
			if payload["refreshToken"] != want {
				t.Error("rotated credential was lost")
			}
			auths++
			writeJSON(w, map[string]string{"token": "candidate", "refreshToken": "pending-refresh"})
		case "/user":
			if !profileOK {
				w.WriteHeader(503)
				return
			}
			writeJSON(w, map[string]string{"inn": "123456789012"})
		case "/income":
			posts++
			writeJSON(w, map[string]string{"approvedReceiptUuid": "receipt"})
		}
	})
	if err := n.Authenticate(context.Background()); err == nil || n.token != "" {
		t.Fatal("failed identity probe accepted access token")
	}
	raw, err := s.Meta("auth.refresh")
	if err != nil {
		t.Fatal(err)
	}
	var staged map[string]string
	if json.Unmarshal([]byte(raw), &staged) != nil || staged["PendingToken"] != "pending-refresh" || staged["Token"] != "" {
		t.Fatal("unverified rotation was not staged separately")
	}
	restarted, err := NewNalog(n.config, s)
	if err != nil {
		t.Fatal(err)
	}
	restarted.base = server.URL
	restarted.client = server.Client()
	profileOK = true
	if _, err = restarted.Create(context.Background(), "payment", 100, time.Now()); err != nil || posts != 1 {
		t.Fatal("restart could not verify pending rotation", err)
	}
	raw, err = s.Meta("auth.refresh")
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal([]byte(raw), &staged) != nil || staged["Token"] != "pending-refresh" || staged["PendingToken"] != "" {
		t.Fatal("verified rotation was not promoted")
	}
	for _, field := range []string{"seed", "device"} {
		c := n.config
		if field == "seed" {
			c.RefreshToken = "new-seed"
		} else {
			c.DeviceID = "other-device"
		}
		other, err := NewNalog(c, s)
		if err != nil || other.refresh != c.RefreshToken {
			t.Fatal("pending credential crossed seed/device binding")
		}
	}
}
func TestNalogWrongProfileNeverPromotesPendingCredential(t *testing.T) {
	identity := "123456789012"
	rotation := "verified-refresh"
	n, s, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/token":
			writeJSON(w, map[string]string{"token": "candidate", "refreshToken": rotation})
		case "/user":
			writeJSON(w, map[string]string{"inn": identity})
		}
	})
	if err := n.Authenticate(context.Background()); err != nil {
		t.Fatal(err)
	}
	identity = "999999999999"
	rotation = "wrong-account-refresh"
	if err := n.Authenticate(context.Background()); err == nil || n.token != "" {
		t.Fatal("wrong account accepted")
	}
	raw, err := s.Meta("auth.refresh")
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]string
	json.Unmarshal([]byte(raw), &saved)
	if saved["Token"] != "verified-refresh" || saved["PendingToken"] != "" {
		t.Fatal("wrong-account rotation poisoned verified credential")
	}
}

type failRefreshSave struct {
	StateStore
	calls, failAt int
}

func (s *failRefreshSave) SetMeta(k, v string) error {
	if k == "auth.refresh" {
		s.calls++
		if s.calls == s.failAt {
			return errors.New("injected credential commit failure")
		}
	}
	return s.StateStore.SetMeta(k, v)
}
func TestNalogPendingRefreshPersistenceFailureBlocksFinancialWrite(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			profiles, posts := 0, 0
			n, s, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/auth/token":
					writeJSON(w, map[string]string{"token": "candidate", "refreshToken": "rotation"})
				case "/user":
					profiles++
					writeJSON(w, map[string]string{"inn": "123456789012"})
				case "/income":
					posts++
					writeJSON(w, map[string]string{"approvedReceiptUuid": "bad"})
				}
			})
			n.store = &failRefreshSave{StateStore: s, failAt: failAt}
			if _, err := n.Create(context.Background(), "payment", 100, time.Now()); err == nil || posts != 0 || n.token != "" {
				t.Fatal("credential durability failure allowed income")
			}
			if failAt == 1 && profiles != 0 {
				t.Fatal("used undurable candidate for identity probe")
			}
		})
	}
}
func TestNalogEmbeddedWrongIdentityDiscardsPreviouslyPendingRotation(t *testing.T) {
	embedded := false
	n, s, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			response := map[string]any{"token": "candidate", "refreshToken": "pending"}
			if embedded {
				response["profile"] = map[string]string{"inn": "999999999999"}
			}
			writeJSON(w, response)
			return
		}
		w.WriteHeader(503)
	})
	if err := n.Authenticate(context.Background()); err == nil {
		t.Fatal("expected profile failure")
	}
	n.authNext = time.Time{}
	embedded = true
	if err := n.Authenticate(context.Background()); err == nil {
		t.Fatal("wrong inline identity accepted")
	}
	raw, err := s.Meta("auth.refresh")
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]string
	json.Unmarshal([]byte(raw), &saved)
	if saved["PendingToken"] != "" {
		t.Fatal("known wrong-account pending credential retained")
	}
}
func TestNalogVerifiedCredentialPromotionClearsStalePendingCandidate(t *testing.T) {
	step := 0
	n, s, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			candidate := "verified"
			if step == 1 {
				candidate = "pending"
			}
			writeJSON(w, map[string]string{"token": "access", "refreshToken": candidate})
			return
		}
		if step == 1 {
			w.WriteHeader(503)
			return
		}
		writeJSON(w, map[string]string{"inn": "123456789012"})
	})
	if err := n.Authenticate(context.Background()); err != nil {
		t.Fatal(err)
	}
	step = 1
	if err := n.Authenticate(context.Background()); err == nil {
		t.Fatal("expected profile failure")
	}
	n.authNext = time.Time{}
	step = 2
	if err := n.Authenticate(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := s.Meta("auth.refresh")
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]string
	json.Unmarshal([]byte(raw), &saved)
	if saved["PendingToken"] != "" || saved["Token"] != "verified" || n.refresh != "verified" {
		t.Fatal("stale pending credential survived successful identity proof")
	}
}
