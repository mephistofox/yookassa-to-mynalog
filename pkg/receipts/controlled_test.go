package receipts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestInitialPaymentFindsExistingReceiptBeforeCreate(t *testing.T) {
	f := setup(t)
	p := paymentAt("one", f.now.Add(-time.Minute))
	f.source.payments = []Payment{p}
	name := f.e.Config.Description + " [yookassa:" + p.ID + "]"
	f.tax.receipts[name] = "existing"
	runOK(t, f, true)
	if f.tax.created != 0 || f.tax.finds != 1 || getPayment(t, f.s, p.ID).Receipt != "existing" || len(f.sender.sent) != 1 {
		t.Fatalf("existing receipt ignored: create=%d find=%d", f.tax.created, f.tax.finds)
	}
}

func TestHistoricalRefundBlocksGrossIncome(t *testing.T) {
	for _, value := range []string{"100.10", "30.00", "broken"} {
		t.Run(value, func(t *testing.T) {
			f := setup(t)
			p := paymentAt("one", f.now.Add(-time.Minute))
			raw := []byte(`{"refunded_amount":{"value":"` + value + `","currency":"RUB"}}`)
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatal(err)
			}
			f.source.payments = []Payment{p}
			runOK(t, f, true)
			if f.tax.created != 0 || len(f.sender.sent) != 0 || getPayment(t, f.s, p.ID).Phase != "blocked" {
				t.Fatal("historical refund taxed gross")
			}
			if err := f.e.RetryPayment(context.Background(), p.ID, true); err == nil {
				t.Fatal("operator bypassed refund validation")
			}
		})
	}
}

type failFinalRefundCommit struct {
	StateStore
	failed bool
}

func (s *failFinalRefundCommit) Save(records ...Record) error {
	if !s.failed {
		for _, r := range records {
			if r.Kind == "refund" && r.ID == "r1" && r.Phase == "done" {
				s.failed = true
				return errors.New("final commit failed")
			}
		}
	}
	return s.StateStore.Save(records...)
}
func TestFailedRefundCommitHoldsLaterRefundAndRecoversRemaining(t *testing.T) {
	f := setup(t)
	f.e.Config.RefundsEnabled = true
	f.source.payments = []Payment{paymentAt("one", f.now.Add(-10*time.Minute))}
	runOK(t, f, true)
	f.source.refunds = []Refund{{ID: "r1", PaymentID: "one", Status: "succeeded", Amount: Amount{"30.00", "RUB"}, CreatedAt: f.now.Add(-2 * time.Minute)}, {ID: "r2", PaymentID: "one", Status: "succeeded", Amount: Amount{"20.00", "RUB"}, CreatedAt: f.now.Add(-time.Minute)}}
	f.e.Store = &failFinalRefundCommit{StateStore: f.s}
	if err := f.e.Run(context.Background(), true); err == nil {
		t.Fatal("commit failure hidden")
	}
	if getRefund(t, f.s, "r2").Phase != "ready" || getPayment(t, f.s, "one").Remaining != 10010 || f.tax.created != 2 {
		t.Fatal("later refund escaped failed commit fence")
	}
	f.now = f.now.Add(time.Minute)
	runOK(t, f, false)
	if getPayment(t, f.s, "one").Remaining != 5010 || getRefund(t, f.s, "r2").Phase != "done" || f.tax.cancelled != 2 || f.tax.created != 3 {
		t.Fatal("refund recovery used stale balance")
	}
	var active Money
	for i, name := range f.tax.names {
		if f.tax.states[f.tax.receipts[name]] == "active" {
			active += f.tax.amounts[i]
		}
	}
	if active != 5010 {
		t.Fatalf("active income=%s want 50.10", active)
	}
}
func TestCancellationBlocksOriginalDeliveryBeforeUnknownOrReplacementFailure(t *testing.T) {
	for _, mode := range []string{"lost cancel", "replacement failed"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			f.e.Config.RefundsEnabled = true
			f.e.Config.DeliveryMode = "disabled"
			f.source.payments = []Payment{paymentAt("one", f.now.Add(-10*time.Minute))}
			runOK(t, f, true)
			f.e.Config.DeliveryMode = "smtp"
			f.source.refunds = []Refund{{ID: "r1", PaymentID: "one", Status: "succeeded", Amount: Amount{"30.00", "RUB"}, CreatedAt: f.now.Add(-time.Minute)}}
			if mode == "lost cancel" {
				f.tax.loseCancel = true
			} else {
				f.tax.createErr = &APIError{Service: "nalog", Status: 400}
			}
			if err := f.e.Run(context.Background(), true); err == nil {
				t.Fatal("expected tax failure")
			}
			if len(f.sender.sent) != 0 {
				t.Fatal("obsolete original receipt was sent")
			}
			if err := f.e.SetEmail("one", "fixed@example.com"); err != nil {
				t.Fatal(err)
			}
			f.now = f.now.Add(time.Minute)
			_ = f.e.Run(context.Background(), false)
			for _, d := range f.sender.sent {
				if d.ID == "one" {
					t.Fatal("email update/restart revived obsolete receipt")
				}
			}
		})
	}
}
func TestMissingINNFailsClosedIncludingAfterPreviousAuthentication(t *testing.T) {
	for _, inn := range []string{"", "999999999999"} {
		t.Run(inn, func(t *testing.T) {
			posts := 0
			valid := true
			n, _, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/auth/token" {
					identity := inn
					if valid {
						identity = "123456789012"
					}
					writeJSON(w, map[string]any{"token": "access", "profile": map[string]string{"inn": identity}})
					return
				}
				if r.URL.Path == "/income" {
					posts++
				}
				writeJSON(w, map[string]string{"approvedReceiptUuid": "receipt"})
			})
			if err := n.Authenticate(context.Background()); err != nil {
				t.Fatal(err)
			}
			valid = false
			if err := n.Authenticate(context.Background()); err == nil {
				t.Fatal("unverified INN accepted")
			}
			if _, err := n.Create(context.Background(), "income", 100, time.Now()); err == nil || posts != 0 {
				t.Fatalf("failed auth allowed income posts=%d err=%v", posts, err)
			}
		})
	}
}
func TestProcessPaymentOnlyChosenPaymentAndDelivery(t *testing.T) {
	f := setup(t)
	a := paymentAt("chosen", f.now.Add(-time.Minute))
	b := paymentAt("other", a.CapturedAt)
	f.source.payments = []Payment{b}
	if err := f.e.scanPayments(context.Background()); err != nil {
		t.Fatal(err)
	}
	unrelated := f.e.delivery("unrelated", "other-receipt", "other@example.com", 100, false)
	if err := f.s.Save(deliveryRecord(&unrelated)); err != nil {
		t.Fatal(err)
	}
	f.e.Config.RefundsEnabled = true
	f.source.refunds = []Refund{{ID: "other-refund", PaymentID: "other", Status: "succeeded", Amount: Amount{"100.10", "RUB"}, CreatedAt: a.CapturedAt}}
	before := len(f.source.windows)
	for i := 0; i < 2; i++ {
		if err := f.e.ProcessPayment(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	if f.tax.created != 1 || len(f.sender.sent) != 1 || f.sender.sent[0].ID != "chosen" || getPayment(t, f.s, "other").Phase != "ready" || len(f.source.windows) != before || f.tax.cancelled != 0 {
		t.Fatal("selected command processed unrelated work or duplicated output")
	}
}
func TestProcessPaymentRejectsInvalidSourceBeforeFinancialCalls(t *testing.T) {
	for _, mode := range []string{"test", "refund", "email", "currency", "unpaid", "future", "metadata", "changed"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			p := paymentAt("one", f.now.Add(-time.Minute))
			switch mode {
			case "test":
				p.Test = true
			case "refund":
				p.RefundedAmount = Amount{"1.00", "RUB"}
			case "email":
				p.Metadata["email"] = ""
			case "currency":
				p.Amount.Currency = "USD"
			case "unpaid":
				p.Paid = false
			case "future":
				p.CapturedAt = f.now.Add(time.Minute)
			case "metadata":
				delete(p.Metadata, "invoice_id")
			case "changed":
				f.source.payments = []Payment{p}
				if err := f.e.scanPayments(context.Background()); err != nil {
					t.Fatal(err)
				}
				p.Amount.Value = "100.20"
			}
			if err := f.e.ProcessPayment(context.Background(), p); err == nil || f.tax.created != 0 || f.tax.finds != 0 || f.sender.calls != 0 {
				t.Fatal("invalid chosen payment processed")
			}
		})
	}
}
func TestProcessPaymentUncertainNeverPostsAgain(t *testing.T) {
	f := setup(t)
	p := paymentAt("one", f.now.Add(-time.Minute))
	f.tax.loseCreate = true
	if err := f.e.ProcessPayment(context.Background(), p); err == nil {
		t.Fatal("lost reply hidden")
	}
	f.now = f.now.Add(time.Minute)
	f.tax.receipts = map[string]string{}
	if err := f.e.ProcessPayment(context.Background(), p); err == nil || f.tax.created != 1 || getPayment(t, f.s, p.ID).Phase != "uncertain" {
		t.Fatal("empty reconciliation repeated POST")
	}
}
func TestManualWorkerStartupTicksWakeOnlyExplicitDo(t *testing.T) {
	f := setup(t)
	f.e.Config.Manual = true
	f.e.Config.PollInterval = time.Millisecond
	f.e.Config.RetryInterval = time.Millisecond
	f.source.payments = []Payment{paymentAt("one", f.now.Add(-time.Minute))}
	w := NewWorker(f.e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	for deadline := time.Now().Add(time.Second); !w.started.Load(); {
		if time.Now().After(deadline) {
			t.Fatal("worker not started")
		}
		time.Sleep(time.Millisecond)
	}
	w.Wake()
	time.Sleep(20 * time.Millisecond)
	if err := w.Do(ctx, func(context.Context, *Engine) error {
		if len(f.source.windows) != 0 || f.tax.created != 0 || f.sender.calls != 0 {
			t.Error("manual worker performed background work")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Do(ctx, func(ctx context.Context, e *Engine) error { return e.ProcessPayment(ctx, f.source.payments[0]) }); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if f.tax.created != 1 || len(f.sender.sent) != 1 || len(f.source.windows) != 0 {
		t.Fatal("explicit manual command failed or scanned shop")
	}
}
func TestYooKassaFetchChosenPaymentChecksIdentityAndRefundAmount(t *testing.T) {
	for _, id := range []string{"chosen", "wrong"} {
		t.Run(id, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/payments/chosen" || r.Method != "GET" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				user, pass, ok := r.BasicAuth()
				if !ok || user != "shop" || pass != "key" {
					t.Error("bad auth")
				}
				fmt.Fprintf(w, `{"id":%q,"refunded_amount":{"value":"10.00","currency":"RUB"}}`, id)
			}))
			defer server.Close()
			y := &YooKassa{server.Client(), server.URL, "shop", "key"}
			p, err := y.Payment(context.Background(), "chosen")
			if id == "wrong" {
				if err == nil {
					t.Fatal("wrong payment accepted")
				}
				return
			}
			if err != nil || p.RefundedAmount.Value != "10.00" {
				t.Fatalf("bad response %+v %v", p, err)
			}
		})
	}
}
func TestInspectUsesMoscowDayAndFlagsOtherIncome(t *testing.T) {
	date := time.Date(2026, 10, 17, 22, 30, 0, 0, time.UTC) // October 18 in Moscow.
	for _, tc := range []struct {
		name              string
		items             []map[string]any
		wantID            string
		conflict, wantErr bool
	}{
		{"exact", []map[string]any{{"name": "target", "approvedReceiptUuid": "exact", "totalAmount": "10.00", "operationTime": "2026-10-18T01:30:00+03:00"}}, "exact", false, false},
		{"same day different time", []map[string]any{{"name": "target", "approvedReceiptUuid": "exact", "totalAmount": "10.00", "operationTime": "2026-10-18T20:00:00+03:00"}}, "exact", false, false},
		{"wrong day", []map[string]any{{"name": "target", "approvedReceiptUuid": "wrong", "totalAmount": "10.00", "operationTime": "2026-10-17T23:00:00+03:00"}}, "", false, false},
		{"legacy", []map[string]any{{"name": "manual service", "approvedReceiptUuid": "legacy", "totalAmount": "10.00", "operationTime": "2026-10-18T01:00:00+03:00"}}, "", true, false},
		{"malformed amount", []map[string]any{{"name": "manual service", "approvedReceiptUuid": "legacy", "totalAmount": "broken", "operationTime": "2026-10-18T01:00:00+03:00"}}, "", false, true},
		{"missing date", []map[string]any{{"name": "target", "approvedReceiptUuid": "exact", "totalAmount": "10.00"}}, "", false, true},
		{"duplicates", []map[string]any{{"name": "target", "approvedReceiptUuid": "a", "totalAmount": "10.00", "operationTime": "2026-10-18T01:00:00+03:00"}, {"name": "target", "approvedReceiptUuid": "b", "totalAmount": "10.00", "operationTime": "2026-10-18T01:00:00+03:00"}}, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, _, _ := testNalog(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{"content": tc.items}) })
			n.token = "access"
			id, conflict, err := n.Inspect(context.Background(), "target", 1000, date)
			if (err != nil) != tc.wantErr || (err == nil && (id != tc.wantID || conflict != tc.conflict)) {
				t.Fatalf("inspect id=%q conflict=%v err=%v", id, conflict, err)
			}
		})
	}
}

type uncertainSender struct{ calls int }

func (s *uncertainSender) Send(context.Context, Delivery) error {
	s.calls++
	return &APIError{Service: "smtp", Uncertain: true}
}
func TestUncertainDeliveryAndEmailCorrectionNeverResend(t *testing.T) {
	f := setup(t)
	sender := &uncertainSender{}
	f.e.Sender = sender
	p := paymentAt("one", f.now.Add(-time.Minute))
	if err := f.e.ProcessPayment(context.Background(), p); err == nil {
		t.Fatal("uncertain delivery hidden")
	}
	if err := f.e.SetEmail(p.ID, "fixed@example.com"); err != nil {
		t.Fatal(err)
	}
	p.Metadata["email"] = "fixed@example.com"
	f.now = f.now.Add(time.Minute)
	_ = f.e.ProcessPayment(context.Background(), p)
	if sender.calls != 1 {
		t.Fatal("uncertain SMTP was sent again")
	}
	var d Delivery
	if _, err := f.s.Get("delivery", p.ID, &d); err != nil {
		t.Fatal(err)
	}
	if d.Phase != "uncertain" {
		t.Fatalf("uncertainty lost: %+v", d)
	}
}

type failSentCommit struct {
	StateStore
	failed bool
}

func (s *failSentCommit) Save(records ...Record) error {
	for _, r := range records {
		if !s.failed && r.Kind == "delivery" && r.Phase == "sent" {
			s.failed = true
			return errors.New("sent commit failed")
		}
	}
	return s.StateStore.Save(records...)
}
func TestDeliveryCommitFailureDoesNotResendAfterRestart(t *testing.T) {
	f := setup(t)
	f.e.Store = &failSentCommit{StateStore: f.s}
	p := paymentAt("one", f.now.Add(-time.Minute))
	if err := f.e.ProcessPayment(context.Background(), p); err == nil {
		t.Fatal("delivery commit failure hidden")
	}
	f.e.Store = f.s
	f.now = f.now.Add(time.Minute)
	_ = f.e.ProcessPayment(context.Background(), p)
	if f.sender.calls != 1 {
		t.Fatal("uncommitted SMTP result resent")
	}
}
func TestProcessPaymentHeldDeliveryReturnsError(t *testing.T) {
	f := setup(t)
	f.e.Sender = &uncertainSender{}
	p := paymentAt("one", f.now.Add(-time.Minute))
	_ = f.e.ProcessPayment(context.Background(), p)
	f.now = f.now.Add(time.Minute)
	if err := f.e.ProcessPayment(context.Background(), p); err == nil {
		t.Fatal("uncertain mail reported as completed")
	}
}
func TestManualHealthDoesNotRequireAutomaticScan(t *testing.T) {
	f := setup(t)
	f.e.Config.Manual = true
	h, err := f.e.Health()
	if err != nil || !h.Ready {
		t.Fatalf("manual idle reported unhealthy: %+v %v", h, err)
	}
}
func TestExplicitPaymentReconciliationDoesNotWaitForBackgroundBackoff(t *testing.T) {
	f := setup(t)
	p := paymentAt("one", f.now.Add(-time.Minute))
	f.tax.loseCreate = true
	if err := f.e.ProcessPayment(context.Background(), p); err == nil {
		t.Fatal("unknown create hidden")
	}
	if err := f.e.ProcessPayment(context.Background(), p); err != nil {
		t.Fatal("explicit readonly reconciliation delayed", err)
	}
	if f.tax.created != 1 || f.tax.finds != 2 || len(f.sender.sent) != 1 || getPayment(t, f.s, p.ID).Phase != "done" {
		t.Fatal("explicit reconciliation repeated write or did not deliver")
	}
}
