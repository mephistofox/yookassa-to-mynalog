package receipts

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

type fakeSource struct {
	payments   []Payment
	refunds    []Refund
	watched    map[string]Refund
	paymentErr error
	windows    [][2]time.Time
}

func (s *fakeSource) Payments(_ context.Context, from, to time.Time, visit func(Payment) error) error {
	s.windows = append(s.windows, [2]time.Time{from, to})
	for _, p := range s.payments {
		if !p.CapturedAt.Before(from) && !p.CapturedAt.After(to) {
			if err := visit(p); err != nil {
				return err
			}
		}
	}
	return s.paymentErr
}
func (s *fakeSource) Refunds(_ context.Context, from, to time.Time, visit func(Refund) error) error {
	for _, r := range s.refunds {
		if !r.CreatedAt.Before(from) && !r.CreatedAt.After(to) {
			if err := visit(r); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *fakeSource) Refund(_ context.Context, id string) (Refund, error) {
	r, ok := s.watched[id]
	if !ok {
		return r, errors.New("refund unavailable")
	}
	return r, nil
}

type fakeTax struct {
	created, cancelled, finds int
	receipts                  map[string]string
	states                    map[string]string
	createErr, cancelErr      error
	loseCreate, loseCancel    bool
	names                     []string
	amounts                   []Money
	dates                     []time.Time
}

func newFakeTax() *fakeTax {
	return &fakeTax{receipts: map[string]string{}, states: map[string]string{}}
}
func (f *fakeTax) Create(_ context.Context, name string, amount Money, date time.Time) (string, error) {
	f.created++
	f.names = append(f.names, name)
	f.amounts = append(f.amounts, amount)
	f.dates = append(f.dates, date)
	if f.createErr != nil && !f.loseCreate {
		return "", f.createErr
	}
	id := "receipt:" + name
	f.receipts[name] = id
	f.states[id] = "active"
	if f.loseCreate {
		f.loseCreate = false
		return "", &APIError{Service: "nalog", Retryable: true, Uncertain: true}
	}
	return id, nil
}
func (f *fakeTax) Find(_ context.Context, name string, _ Money, _ time.Time) (string, error) {
	f.finds++
	return f.receipts[name], nil
}
func (f *fakeTax) Cancel(_ context.Context, id string) error {
	f.cancelled++
	if f.cancelErr != nil && !f.loseCancel {
		return f.cancelErr
	}
	f.states[id] = "cancelled"
	if f.loseCancel {
		f.loseCancel = false
		return &APIError{Service: "nalog", Uncertain: true, Retryable: true}
	}
	return nil
}
func (f *fakeTax) Status(_ context.Context, id string, _ time.Time) (string, error) {
	if s := f.states[id]; s != "" {
		return s, nil
	}
	return "missing", nil
}

type fakeSender struct {
	calls int
	fail  bool
	sent  []Delivery
}

func (f *fakeSender) Send(_ context.Context, d Delivery) error {
	f.calls++
	if f.fail {
		return errors.New("SMTP contains secret should not be logged")
	}
	f.sent = append(f.sent, d)
	return nil
}

type fixture struct {
	e      *Engine
	s      *Store
	source *fakeSource
	tax    *fakeTax
	sender *fakeSender
	now    time.Time
	path   string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenStore(path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := &fixture{s: s, source: &fakeSource{}, tax: newFakeTax(), sender: &fakeSender{}, now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), path: path}
	f.e = &Engine{Store: s, Source: f.source, Tax: f.tax, Sender: f.sender, Config: Config{Start: f.now.Add(-time.Hour), Description: "Доступ к сервису fxTunnel", DeliveryMode: "smtp", HealthMaxAge: 15 * time.Minute}, Now: func() time.Time { return f.now }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return f
}
func paymentAt(id string, date time.Time) Payment {
	return Payment{ID: id, Status: "succeeded", Paid: true, Amount: Amount{"100.10", "RUB"}, CapturedAt: date, CreatedAt: date.Add(-48 * time.Hour), Metadata: map[string]string{"invoice_id": "1", "user_id": "2", "subscription_id": "3", "plan_id": "4", "email": "user@example.com"}}
}
func getPayment(t *testing.T, s *Store, id string) PaymentJob {
	t.Helper()
	var p PaymentJob
	ok, err := s.Get("payment", id, &p)
	if err != nil || !ok {
		t.Fatalf("payment missing: %v", err)
	}
	return p
}
func getRefund(t *testing.T, s *Store, id string) RefundJob {
	t.Helper()
	var r RefundJob
	ok, err := s.Get("refund", id, &r)
	if err != nil || !ok {
		t.Fatalf("refund missing: %v", err)
	}
	return r
}
func runOK(t *testing.T, f *fixture, scan bool) {
	t.Helper()
	if err := f.e.Run(context.Background(), scan); err != nil {
		t.Fatal(err)
	}
}

func TestPaymentLateCaptureAndDuplicateScan(t *testing.T) {
	f := setup(t)
	p := paymentAt("payment-1", f.now.Add(-time.Minute))
	f.source.payments = []Payment{p}
	runOK(t, f, true)
	f.now = f.now.Add(time.Minute)
	runOK(t, f, true)
	if f.tax.created != 1 || len(f.sender.sent) != 1 {
		t.Fatalf("duplicate side effects: creates=%d sends=%d", f.tax.created, len(f.sender.sent))
	}
	job := getPayment(t, f.s, p.ID)
	if job.Amount != 10010 || job.Phase != "done" || !job.Date.Equal(p.CapturedAt) {
		t.Fatalf("incorrect job %+v", job)
	}
	if !f.tax.dates[0].Equal(p.CapturedAt) {
		t.Fatal("used creation date instead of capture date")
	}
}
func TestPartialScanDoesNotAdvanceCheckpoint(t *testing.T) {
	f := setup(t)
	f.source.payments = []Payment{paymentAt("one", f.now.Add(-time.Minute))}
	f.source.paymentErr = errors.New("page two failed")
	if err := f.e.Run(context.Background(), true); err == nil {
		t.Fatal("scan failure hidden")
	}
	v, _ := f.s.Meta("checkpoint.payments")
	if v != "" {
		t.Fatal("checkpoint advanced after incomplete pagination")
	}
	if f.tax.created != 1 {
		t.Fatal("persisted first page was not processed")
	}
	f.source.paymentErr = nil
	runOK(t, f, true)
	if f.tax.created != 1 {
		t.Fatal("replayed scan created duplicate")
	}
}
func TestLostCreateResponseReconcilesAfterRestart(t *testing.T) {
	f := setup(t)
	f.source.payments = []Payment{paymentAt("one", f.now.Add(-time.Minute))}
	f.tax.loseCreate = true
	if err := f.e.Run(context.Background(), true); err == nil {
		t.Fatal("lost result should be visible")
	}
	if getPayment(t, f.s, "one").Phase != "uncertain" {
		t.Fatal("lost POST was not held")
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(f.path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f.s = s
	f.e.Store = s
	f.now = f.now.Add(time.Minute)
	runOK(t, f, false)
	if f.tax.created != 1 || f.tax.finds != 1 || len(f.sender.sent) != 1 {
		t.Fatal("restart repeated financial write or lost outbox")
	}
	if getPayment(t, f.s, "one").Phase != "done" {
		t.Fatal("receipt not recovered")
	}
}
func TestCrashBeforePostDoesNotAssumeAbsence(t *testing.T) {
	f := setup(t)
	p := PaymentJob{ID: "one", Phase: "creating", Date: f.now, Amount: 10010, Remaining: 10010, Description: "exact marker", Email: "user@example.com"}
	if err := f.s.Save(paymentRecord(&p)); err != nil {
		t.Fatal(err)
	}
	_ = f.e.Run(context.Background(), false)
	f.now = f.now.Add(time.Hour)
	_ = f.e.Run(context.Background(), false)
	if f.tax.created != 0 || getPayment(t, f.s, "one").Phase != "uncertain" {
		t.Fatal("empty lookup incorrectly authorized another POST")
	}
	if err := f.e.RetryPayment(context.Background(), "one", false); err == nil {
		t.Fatal("unsafe retry allowed")
	}
	if err := f.e.RetryPayment(context.Background(), "one", true); err != nil {
		t.Fatal(err)
	}
	runOK(t, f, false)
	if f.tax.created != 1 {
		t.Fatal("explicit operator recovery failed")
	}
}
func TestRateLimitRetriesAndOutboxIsIndependent(t *testing.T) {
	f := setup(t)
	f.source.payments = []Payment{paymentAt("one", f.now.Add(-time.Minute))}
	f.tax.createErr = &APIError{Service: "nalog", Status: 429, Retryable: true}
	_ = f.e.Run(context.Background(), true)
	if getPayment(t, f.s, "one").Phase != "ready" {
		t.Fatal("definite rejection must remain retryable")
	}
	f.tax.createErr = nil
	f.sender.fail = true
	f.now = f.now.Add(time.Minute)
	_ = f.e.Run(context.Background(), false)
	if getPayment(t, f.s, "one").Phase != "done" {
		t.Fatal("SMTP failure lost registered receipt")
	}
	if h, err := f.e.Health(); err != nil || h.Ready {
		t.Fatal("delivery failure hidden from readiness")
	}
	f.sender.fail = false
	f.now = f.now.Add(time.Minute)
	runOK(t, f, false)
	if f.tax.created != 2 || len(f.sender.sent) != 1 {
		t.Fatal("SMTP retry created another receipt")
	}
}
func TestInvalidCustomerEmailCanBeRecoveredWithoutAnotherReceipt(t *testing.T) {
	f := setup(t)
	p := paymentAt("one", f.now.Add(-time.Minute))
	p.Metadata["email"] = ""
	f.source.payments = []Payment{p}
	runOK(t, f, true)
	if f.tax.created != 1 || len(f.sender.sent) != 0 {
		t.Fatal("invalid email should block delivery only")
	}
	if err := f.e.SetEmail("one", "verified@example.com"); err != nil {
		t.Fatal(err)
	}
	runOK(t, f, false)
	if f.tax.created != 1 || len(f.sender.sent) != 1 || f.sender.sent[0].Email != "verified@example.com" {
		t.Fatal("email recovery failed")
	}
}
func TestTestAndForeignPaymentsDoNotCreateIncome(t *testing.T) {
	f := setup(t)
	p := paymentAt("test", f.now.Add(-time.Minute))
	p.Test = true
	q := paymentAt("foreign", f.now.Add(-time.Minute))
	q.Amount.Currency = "USD"
	f.source.payments = []Payment{p, q}
	runOK(t, f, true)
	if f.tax.created != 0 || getPayment(t, f.s, "foreign").Phase != "blocked" {
		t.Fatal("unsupported payment taxed")
	}
	if err := f.e.RetryPayment(context.Background(), "foreign", true); err == nil {
		t.Fatal("foreign payment bypassed validation")
	}
}
func TestPartialThenFullRefundUsesRemainingAndOriginalDate(t *testing.T) {
	f := setup(t)
	f.e.Config.RefundsEnabled = true
	p := paymentAt("one", f.now.Add(-10*time.Minute))
	f.source.payments = []Payment{p}
	runOK(t, f, true)
	f.source.refunds = []Refund{{ID: "r1", PaymentID: "one", Status: "succeeded", Amount: Amount{"30.05", "RUB"}, CreatedAt: f.now.Add(-2 * time.Minute)}, {ID: "r2", PaymentID: "one", Status: "succeeded", Amount: Amount{"70.05", "RUB"}, CreatedAt: f.now.Add(-time.Minute)}}
	runOK(t, f, true)
	job := getPayment(t, f.s, "one")
	if job.Remaining != 0 || job.Receipt != "" || f.tax.cancelled != 2 || f.tax.created != 2 {
		t.Fatalf("incorrect refund state %+v calls=%d/%d", job, f.tax.created, f.tax.cancelled)
	}
	if f.tax.amounts[1] != 7005 || !f.tax.dates[1].Equal(p.CapturedAt) {
		t.Fatal("partial refund amount/date wrong")
	}
	f.now = f.now.Add(time.Minute)
	runOK(t, f, true)
	if f.tax.cancelled != 2 || f.tax.created != 2 {
		t.Fatal("refund replay repeated tax write")
	}
}
func TestPendingRefundSurvivesCheckpoint(t *testing.T) {
	f := setup(t)
	f.e.Config.RefundsEnabled = true
	f.source.payments = []Payment{paymentAt("one", f.now.Add(-10*time.Minute))}
	runOK(t, f, true)
	r := Refund{ID: "r1", PaymentID: "one", Status: "pending", Amount: Amount{"100.10", "RUB"}, CreatedAt: f.now.Add(-time.Minute)}
	f.source.refunds = []Refund{r}
	f.source.watched = map[string]Refund{r.ID: r}
	_ = f.e.Run(context.Background(), true)
	if getRefund(t, f.s, r.ID).Phase != "watching" {
		t.Fatal("pending refund lost")
	}
	f.now = f.now.Add(time.Hour)
	f.source.refunds = nil
	r.Status = "succeeded"
	f.source.watched[r.ID] = r
	runOK(t, f, false)
	if f.tax.cancelled != 1 || getRefund(t, f.s, r.ID).Phase != "done" {
		t.Fatal("pending to succeeded transition missed")
	}
}
func TestLostCancelResponseAndLostReplacementAreReconciled(t *testing.T) {
	f := setup(t)
	f.e.Config.RefundsEnabled = true
	f.source.payments = []Payment{paymentAt("one", f.now.Add(-10*time.Minute))}
	runOK(t, f, true)
	f.source.refunds = []Refund{{ID: "r1", PaymentID: "one", Status: "succeeded", Amount: Amount{"30.00", "RUB"}, CreatedAt: f.now.Add(-time.Minute)}}
	f.tax.loseCancel = true
	_ = f.e.Run(context.Background(), true)
	if getRefund(t, f.s, "r1").Phase != "cancelling" {
		t.Fatal("cancellation uncertainty lost")
	}
	f.now = f.now.Add(time.Minute)
	f.tax.loseCreate = true
	_ = f.e.Run(context.Background(), false)
	if getRefund(t, f.s, "r1").Phase != "replacing" {
		t.Fatal("replacement uncertainty lost")
	}
	f.now = f.now.Add(time.Minute)
	runOK(t, f, false)
	if f.tax.cancelled != 1 || f.tax.created != 2 || getRefund(t, f.s, "r1").Phase != "done" {
		t.Fatal("reconciliation repeated financial writes")
	}
}
func TestHistoricalRefundAndOverRefundAreBlocked(t *testing.T) {
	f := setup(t)
	f.e.Config.RefundsEnabled = true
	f.source.refunds = []Refund{{ID: "historical", PaymentID: "missing", Status: "succeeded", Amount: Amount{"10.00", "RUB"}, CreatedAt: f.now.Add(-time.Minute)}}
	_ = f.e.Run(context.Background(), true)
	if f.tax.cancelled != 0 || f.tax.created != 0 || getRefund(t, f.s, "historical").Phase != "blocked" {
		t.Fatal("historical income guessed")
	}
	f.source.payments = []Payment{paymentAt("one", f.now.Add(-time.Minute))}
	f.source.refunds = []Refund{{ID: "too-much", PaymentID: "one", Status: "succeeded", Amount: Amount{"101.00", "RUB"}, CreatedAt: f.now.Add(-time.Minute)}}
	_ = f.e.Run(context.Background(), true)
	if f.tax.cancelled != 0 || getRefund(t, f.s, "too-much").Phase != "blocked" {
		t.Fatal("over-refund changed tax income")
	}
}

func TestUncertainCancellationBlocksLaterRefundUntilOperatorReconciles(t *testing.T) {
	f := setup(t)
	f.e.Config.RefundsEnabled = true
	f.source.payments = []Payment{paymentAt("one", f.now.Add(-10*time.Minute))}
	runOK(t, f, true)
	f.source.refunds = []Refund{{ID: "first", PaymentID: "one", Status: "succeeded", Amount: Amount{"30.00", "RUB"}, CreatedAt: f.now.Add(-2 * time.Minute)}, {ID: "second", PaymentID: "one", Status: "succeeded", Amount: Amount{"70.10", "RUB"}, CreatedAt: f.now.Add(-time.Minute)}}
	f.tax.cancelErr = &APIError{Service: "nalog", Uncertain: true, Retryable: true}
	_ = f.e.Run(context.Background(), true)
	f.now = f.now.Add(time.Minute)
	_ = f.e.Run(context.Background(), false)
	if f.tax.cancelled != 1 || getRefund(t, f.s, "first").Phase != "cancelling" || getRefund(t, f.s, "second").Phase != "ready" {
		t.Fatal("uncertain cancellation retried or later refund bypassed it")
	}
	if err := f.e.RetryRefund(context.Background(), "first", false); err == nil {
		t.Fatal("operator assertion was not required")
	}
	f.tax.cancelErr = nil
	if err := f.e.RetryRefund(context.Background(), "first", true); err != nil {
		t.Fatal(err)
	}
	runOK(t, f, false)
	if getRefund(t, f.s, "second").Phase != "done" || getPayment(t, f.s, "one").Remaining != 0 {
		t.Fatal("operator recovery did not resume sequential refunds")
	}
}
