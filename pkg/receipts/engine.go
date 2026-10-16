// Copyright 2026 fxTunnel contributors. Licensed under Apache-2.0.
// Durable reconciliation workflow adapted from upstream app/main.py.
package receipts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

type PaymentSource interface {
	Payments(context.Context, time.Time, time.Time, func(Payment) error) error
	Refunds(context.Context, time.Time, time.Time, func(Refund) error) error
	Refund(context.Context, string) (Refund, error)
}
type TaxService interface {
	Create(context.Context, string, Money, time.Time) (string, error)
	Find(context.Context, string, Money, time.Time) (string, error)
	Cancel(context.Context, string) error
	Status(context.Context, string, time.Time) (string, error)
}
type Sender interface {
	Send(context.Context, Delivery) error
}
type Engine struct {
	Config Config
	Store  StateStore
	Source PaymentSource
	Tax    TaxService
	Sender Sender
	Now    func() time.Time
	Log    *slog.Logger
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}
func (e *Engine) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}
func (e *Engine) Run(ctx context.Context, scan bool) error {
	var errs []error
	if scan {
		if err := e.scanPayments(ctx); err != nil {
			errs = append(errs, fmt.Errorf("payment scan: %w", err))
		} else if err = e.Store.SetMeta("scan.payments.ok", e.now().Format(time.RFC3339Nano)); err != nil {
			errs = append(errs, err)
		}
	}
	if err := e.processPayments(ctx); err != nil {
		errs = append(errs, err)
	}
	if e.Config.RefundsEnabled {
		if scan {
			if err := e.scanRefunds(ctx); err != nil {
				errs = append(errs, fmt.Errorf("refund scan: %w", err))
			} else if err = e.Store.SetMeta("scan.refunds.ok", e.now().Format(time.RFC3339Nano)); err != nil {
				errs = append(errs, err)
			}
		}
		if err := e.processRefunds(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if err := e.deliver(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := e.Store.SetMeta("worker.last", e.now().Format(time.RFC3339Nano)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
func (e *Engine) window(key string) (time.Time, time.Time, error) {
	v, err := e.Store.Meta(key)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	from := e.Config.Start
	if v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("invalid persisted checkpoint")
		}
		from = t.Add(-5 * time.Minute)
		if from.Before(e.Config.Start) {
			from = e.Config.Start
		}
	}
	return from, e.now().Add(-30 * time.Second), nil
}
func (e *Engine) scanPayments(ctx context.Context) error {
	from, to, err := e.window("checkpoint.payments")
	if err != nil {
		return err
	}
	if to.Before(from) {
		return nil
	}
	err = e.Source.Payments(ctx, from, to, func(p Payment) error {
		if p.ID == "" {
			return errors.New("payment without ID")
		}
		if p.Test {
			return nil
		} // Test payments must never become real NPD income.
		if p.Status != "succeeded" || !p.Paid {
			return errors.New("payment API returned an unconfirmed payment")
		}
		if p.CapturedAt.IsZero() || p.CapturedAt.Before(from) || p.CapturedAt.After(to) {
			return errors.New("payment capture date outside requested window")
		}
		var old PaymentJob
		exists, err := e.Store.Get("payment", p.ID, &old)
		if err != nil || exists {
			return err
		}
		amount, err := ParseMoney(p.Amount.Value)
		if err != nil || amount <= 0 {
			return errors.New("payment amount is invalid")
		}
		job := PaymentJob{ID: p.ID, Phase: "ready", Amount: amount, Remaining: amount, Date: p.CapturedAt, Description: e.Config.Description + " [yookassa:" + p.ID + "]", Email: p.Metadata["email"]}
		if p.Amount.Currency != "RUB" {
			job.Phase = "blocked"
			job.Error = "unsupported currency"
		}
		// These keys are present on both fxTunnel checkout and renewal. A
		// dedicated shop is still required; similar keys are not merchant identity.
		for _, key := range []string{"invoice_id", "user_id", "subscription_id", "plan_id"} {
			if p.Metadata[key] == "" {
				job.Phase = "blocked"
				job.Error = "missing fxTunnel payment metadata"
			}
		}
		return e.Store.Save(paymentRecord(&job))
	})
	if err != nil {
		return err
	}
	return e.Store.SetMeta("checkpoint.payments", to.Format(time.RFC3339Nano))
}
func (e *Engine) processPayments(ctx context.Context) error {
	jobs, err := listAs[PaymentJob](e.Store, "payment")
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p.Phase != "ready" && p.Phase != "creating" && p.Phase != "uncertain" {
			continue
		}
		if p.Next.After(e.now()) {
			continue
		}
		p.Attempts++
		var id string
		var callErr error
		if p.Phase == "ready" {
			p.Phase = "creating"
			if err = e.Store.Save(paymentRecord(&p)); err != nil {
				return err
			}
			id, callErr = e.Tax.Create(ctx, p.Description, p.Amount, p.Date)
		} else {
			id, callErr = e.Tax.Find(ctx, p.Description, p.Amount, p.Date)
			if callErr == nil && id == "" {
				callErr = errors.New("receipt not found yet; creation is held for reconciliation")
			}
			p.Phase = "uncertain"
		}
		if callErr == nil && id != "" {
			p.Receipt = id
			p.Phase = "done"
			p.Error = ""
			p.Next = time.Time{}
			d := e.delivery(p.ID, id, p.Email, p.Amount, false)
			if err = e.Store.Save(paymentRecord(&p), deliveryRecord(&d)); err != nil {
				return err
			}
			e.logger().Info("receipt registered", "payment_id", p.ID)
		} else {
			if p.Phase == "creating" {
				p.Phase = "blocked"
				if uncertain(callErr) {
					p.Phase = "uncertain"
				} else if retryable(callErr) {
					p.Phase = "ready"
				}
			}
			p.Error = safeError(callErr)
			p.Next = e.now().Add(backoff(p.Attempts))
			if err = e.Store.Save(paymentRecord(&p)); err != nil {
				return err
			}
			errs = append(errs, fmt.Errorf("receipt %s: %s", p.ID, p.Error))
		}
	}
	return errors.Join(errs...)
}
func safeError(err error) string {
	if err == nil {
		return "operation returned no receipt"
	}
	return err.Error()
}
func (e *Engine) delivery(id, receipt, email string, amount Money, cancelled bool) Delivery {
	d := Delivery{ID: id, Phase: "ready", Email: email, Receipt: receipt, Amount: amount, Cancelled: cancelled}
	if !validEmail(email) {
		d.Phase = "blocked"
		d.Error = "missing or invalid customer email"
	}
	return d
}
func (e *Engine) scanRefunds(ctx context.Context) error {
	from, to, err := e.window("checkpoint.refunds")
	if err != nil {
		return err
	}
	if to.Before(from) {
		return nil
	}
	err = e.Source.Refunds(ctx, from, to, func(r Refund) error {
		if r.ID == "" || r.PaymentID == "" || r.CreatedAt.IsZero() {
			return errors.New("invalid refund identity/date")
		}
		var old RefundJob
		exists, err := e.Store.Get("refund", r.ID, &old)
		if err != nil || exists {
			return err
		}
		amount, err := ParseMoney(r.Amount.Value)
		if err != nil || amount <= 0 {
			return errors.New("invalid refund amount")
		}
		job := RefundJob{ID: r.ID, PaymentID: r.PaymentID, Phase: "watching", Amount: amount, Currency: r.Amount.Currency, CreatedAt: r.CreatedAt}
		switch r.Status {
		case "succeeded":
			job.Phase = "ready"
		case "canceled":
			job.Phase = "done"
		case "pending":
		default:
			return errors.New("unknown refund status")
		}
		return e.Store.Save(refundRecord(&job))
	})
	if err != nil {
		return err
	}
	return e.Store.SetMeta("checkpoint.refunds", to.Format(time.RFC3339Nano))
}
func (e *Engine) processRefunds(ctx context.Context) error {
	jobs, err := listAs[RefundJob](e.Store, "refund")
	if err != nil {
		return err
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].CreatedAt.Equal(jobs[j].CreatedAt) {
			return jobs[i].ID < jobs[j].ID
		}
		return jobs[i].CreatedAt.Before(jobs[j].CreatedAt)
	})
	busy := map[string]bool{}
	var errs []error
	for _, r := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if r.Phase == "done" {
			continue
		}
		if busy[r.PaymentID] {
			continue
		}
		busy[r.PaymentID] = true
		if r.Next.After(e.now()) || r.Phase == "blocked" {
			continue
		}
		err = e.processRefund(ctx, &r)
		if err != nil {
			errs = append(errs, fmt.Errorf("refund %s: %w", r.ID, err))
		}
		if r.Phase == "done" {
			busy[r.PaymentID] = false
		}
	}
	return errors.Join(errs...)
}
func (e *Engine) processRefund(ctx context.Context, r *RefundJob) error {
	r.Attempts++
	fail := func(err error, phase string) error {
		r.Phase = phase
		r.Error = safeError(err)
		r.Next = e.now().Add(backoff(r.Attempts))
		if saveErr := e.Store.Save(refundRecord(r)); saveErr != nil {
			return saveErr
		}
		return err
	}
	if r.Phase == "watching" {
		actual, err := e.Source.Refund(ctx, r.ID)
		if err != nil {
			return fail(err, "watching")
		}
		amount, parseErr := ParseMoney(actual.Amount.Value)
		if parseErr != nil || amount != r.Amount || actual.PaymentID != r.PaymentID || actual.Amount.Currency != r.Currency {
			return fail(errors.New("refund changed identity or amount"), "blocked")
		}
		switch actual.Status {
		case "succeeded":
			r.Phase = "ready"
		case "canceled":
			r.Phase = "done"
		case "pending":
			return fail(errors.New("refund is pending"), "watching")
		default:
			return fail(errors.New("unknown refund status"), "blocked")
		}
		if err = e.Store.Save(refundRecord(r)); err != nil {
			return err
		}
		if r.Phase == "done" {
			return nil
		}
	}
	var p PaymentJob
	exists, err := e.Store.Get("payment", r.PaymentID, &p)
	if err != nil {
		return err
	}
	if !exists {
		return fail(errors.New("original receipt absent; reconcile historical refund manually"), "blocked")
	}
	if p.Phase != "done" {
		return fail(errors.New("original receipt is not registered yet"), r.Phase)
	}
	if r.Phase == "ready" {
		if r.Currency != "RUB" || r.Amount > p.Remaining || p.Receipt == "" {
			return fail(errors.New("refund exceeds remaining income or has wrong currency"), "blocked")
		}
		r.OldReceipt = p.Receipt
		r.Remaining = p.Remaining - r.Amount
		r.Description = p.Description + " [refund:" + r.ID + "]"
		r.Phase = "cancel_ready"
		if err = e.Store.Save(refundRecord(r)); err != nil {
			return err
		}
	}
	if r.Phase == "cancelling" {
		state, err := e.Tax.Status(ctx, r.OldReceipt, p.Date)
		if err != nil {
			return fail(err, "cancelling")
		}
		if state == "cancelled" {
			r.Phase = "cancelled"
		} else if state == "active" {
			return fail(errors.New("cancellation result uncertain; receipt still active"), "cancelling")
		} else {
			return fail(errors.New("cancellation result uncertain; receipt missing"), "cancelling")
		}
		if err = e.Store.Save(refundRecord(r)); err != nil {
			return err
		}
	}
	if r.Phase == "cancel_ready" {
		r.Phase = "cancelling"
		if err = e.Store.Save(refundRecord(r)); err != nil {
			return err
		}
		if err = e.Tax.Cancel(ctx, r.OldReceipt); err != nil {
			phase := "blocked"
			if uncertain(err) {
				phase = "cancelling"
			} else if retryable(err) {
				phase = "cancel_ready"
			}
			return fail(err, phase)
		}
		r.Phase = "cancelled"
		if err = e.Store.Save(refundRecord(r)); err != nil {
			return err
		}
	}
	if r.Phase == "cancelled" && r.Remaining == 0 {
		return e.completeRefund(r, &p, "")
	}
	var id string
	if r.Phase == "cancelled" {
		r.Phase = "replacing"
		if err = e.Store.Save(refundRecord(r)); err != nil {
			return err
		}
		id, err = e.Tax.Create(ctx, r.Description, r.Remaining, p.Date)
		if err != nil {
			phase := "blocked"
			if uncertain(err) {
				phase = "replacing"
			} else if retryable(err) {
				phase = "cancelled"
			}
			return fail(err, phase)
		}
	} else if r.Phase == "replacing" {
		id, err = e.Tax.Find(ctx, r.Description, r.Remaining, p.Date)
		if err != nil {
			return fail(err, "replacing")
		}
		if id == "" {
			return fail(errors.New("replacement receipt result uncertain; reconciliation pending"), "replacing")
		}
	}
	if id != "" {
		return e.completeRefund(r, &p, id)
	}
	return fail(errors.New("invalid refund phase"), "blocked")
}
func (e *Engine) completeRefund(r *RefundJob, p *PaymentJob, id string) error {
	r.Phase = "done"
	r.Error = ""
	r.Next = time.Time{}
	p.Remaining = r.Remaining
	p.Receipt = id
	records := []Record{refundRecord(r), paymentRecord(p)}
	d := e.delivery("refund:"+r.ID, id, p.Email, r.Remaining, id == "")
	if id == "" {
		d.Receipt = r.OldReceipt
	}
	records = append(records, deliveryRecord(&d))
	// Do not deliver an obsolete receipt whose delivery has not happened yet.
	old, err := listAs[Delivery](e.Store, "delivery")
	if err != nil {
		return err
	}
	for _, v := range old {
		if v.Receipt == r.OldReceipt && v.Phase != "sent" && v.Phase != "superseded" {
			v.Phase = "superseded"
			v.Error = "receipt replaced or cancelled"
			records = append(records, deliveryRecord(&v))
		}
	}
	return e.Store.Save(records...)
}
func (e *Engine) deliver(ctx context.Context) error {
	jobs, err := listAs[Delivery](e.Store, "delivery")
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.Phase != "ready" || d.Next.After(e.now()) {
			continue
		}
		if e.Config.DeliveryMode == "disabled" {
			continue
		}
		d.Attempts++
		if e.Sender == nil {
			err = errors.New("SMTP sender is unavailable")
		} else {
			err = e.Sender.Send(ctx, d)
		}
		if err != nil {
			d.Error = "SMTP delivery failed"
			d.Next = e.now().Add(backoff(d.Attempts))
			errs = append(errs, fmt.Errorf("delivery %s failed", d.ID))
		} else {
			d.Phase = "sent"
			d.Error = ""
			d.Next = time.Time{}
		}
		if err = e.Store.Save(deliveryRecord(&d)); err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

type Health struct {
	Ready  bool           `json:"ready"`
	Counts map[string]int `json:"counts"`
	Issues []string       `json:"issues,omitempty"`
}

func (e *Engine) Health() (Health, error) {
	counts, err := e.Store.Counts()
	if err != nil {
		return Health{}, err
	}
	h := Health{Ready: true, Counts: counts}
	for _, key := range []string{"scan.payments.ok", "worker.last"} {
		v, err := e.Store.Meta(key)
		if err != nil {
			return h, err
		}
		t, parseErr := time.Parse(time.RFC3339Nano, v)
		if parseErr != nil || e.now().Sub(t) > e.Config.HealthMaxAge {
			h.Issues = append(h.Issues, key+" stale")
		}
	}
	if e.Config.RefundsEnabled {
		v, err := e.Store.Meta("scan.refunds.ok")
		if err != nil {
			return h, err
		}
		t, parseErr := time.Parse(time.RFC3339Nano, v)
		if parseErr != nil || e.now().Sub(t) > e.Config.HealthMaxAge {
			h.Issues = append(h.Issues, "refund scan stale")
		}
	}
	for key, n := range counts {
		if n > 0 && (strings.HasSuffix(key, ".blocked") || strings.HasSuffix(key, ".uncertain") || strings.HasSuffix(key, ".creating") || strings.HasSuffix(key, ".replacing") || strings.HasSuffix(key, ".cancelling")) {
			h.Issues = append(h.Issues, key+" requires attention")
		}
	}
	if e.Config.DeliveryMode == "disabled" && counts["delivery.ready"] > 0 {
		h.Issues = append(h.Issues, "delivery disabled with queued receipts")
	}
	payments, err := listAs[PaymentJob](e.Store, "payment")
	if err != nil {
		return h, err
	}
	for _, p := range payments {
		if p.Phase == "ready" && p.Attempts > 0 {
			h.Issues = append(h.Issues, "receipt registration retrying")
			break
		}
	}
	refunds, err := listAs[RefundJob](e.Store, "refund")
	if err != nil {
		return h, err
	}
	for _, r := range refunds {
		if r.Phase != "done" && r.Phase != "watching" && r.Error != "" {
			h.Issues = append(h.Issues, "refund processing requires attention")
			break
		}
	}
	// Repeated delivery failures should be visible even when scanning succeeds.
	deliveries, err := listAs[Delivery](e.Store, "delivery")
	if err != nil {
		return h, err
	}
	for _, d := range deliveries {
		if d.Phase == "ready" && d.Attempts > 0 {
			h.Issues = append(h.Issues, "customer delivery retrying")
			break
		}
	}
	sort.Strings(h.Issues)
	h.Ready = len(h.Issues) == 0
	return h, nil
}
