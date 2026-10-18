// Copyright 2026 fxTunnel contributors. Licensed under Apache-2.0.
package receipts

import (
	"context"
	"errors"
	"time"
)

// RetryPayment requires an explicit local operator assertion before another
// POST for an uncertain operation. An empty GET result alone is not proof.
func (e *Engine) RetryPayment(ctx context.Context, id string, confirmedAbsent bool) error {
	var p PaymentJob
	ok, err := e.Store.Get("payment", id, &p)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("payment not found")
	}
	if p.Phase == "done" {
		return errors.New("payment already has a receipt")
	}
	if p.Error == "unsupported currency" || p.Error == "missing fxTunnel payment metadata" || p.Error == "historical refund requires manual reconciliation" {
		return errors.New("source validation failed; this record cannot be retried")
	}
	if !confirmedAbsent {
		return errors.New("verify in My Nalog that no receipt exists, then pass --confirmed-absent")
	}
	receipt, err := e.Tax.Find(ctx, p.Description, p.Amount, p.Date)
	if err != nil {
		return err
	}
	if receipt != "" {
		p.Receipt = receipt
		p.Phase = "done"
		p.Error = ""
		d := e.delivery(p.ID, receipt, p.Email, p.Amount, false)
		return e.Store.Save(paymentRecord(&p), deliveryRecord(&d))
	}
	p.Phase = "ready"
	p.Error = ""
	p.Next = time.Time{}
	return e.Store.Save(paymentRecord(&p))
}
func (e *Engine) SetEmail(paymentID, email string) error {
	if !validEmail(email) {
		return errors.New("invalid customer email")
	}
	var p PaymentJob
	ok, err := e.Store.Get("payment", paymentID, &p)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("payment not found")
	}
	p.Email = email
	records := []Record{paymentRecord(&p)}
	deliveries, err := listAs[Delivery](e.Store, "delivery")
	if err != nil {
		return err
	}
	for _, d := range deliveries {
		if d.ID == p.ID || len(d.ID) > 7 && d.ID[:7] == "refund:" {
			// Match refunds via their durable payment relationship, not ID shape.
			belongs := d.ID == p.ID
			if !belongs {
				var r RefundJob
				if found, e := e.Store.Get("refund", d.ID[7:], &r); e != nil {
					return e
				} else {
					belongs = found && r.PaymentID == p.ID
				}
			}
			if belongs && (d.Phase == "ready" || d.Phase == "blocked") {
				d.Email = email
				d.Phase = "ready"
				d.Error = ""
				d.Next = time.Time{}
				records = append(records, deliveryRecord(&d))
			}
		}
	}
	return e.Store.Save(records...)
}

// RetryRefund is an explicit local recovery operation after the operator has
// checked both cancellation and replacement in My Nalog. No financial write is
// made here. The next worker cycle resumes from the reconciled durable phase.
func (e *Engine) RetryRefund(ctx context.Context, id string, confirmed bool) error {
	if !confirmed {
		return errors.New("verify cancellation and replacement in My Nalog, then pass --confirmed-state")
	}
	var r RefundJob
	ok, err := e.Store.Get("refund", id, &r)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("refund not found")
	}
	if r.Phase == "done" || r.Phase == "watching" || r.Phase == "ready" || r.OldReceipt == "" {
		return errors.New("refund has no recoverable tax operation")
	}
	var p PaymentJob
	ok, err = e.Store.Get("payment", r.PaymentID, &p)
	if err != nil {
		return err
	}
	if !ok || p.Phase != "done" || p.Receipt != r.OldReceipt {
		return errors.New("original receipt relationship changed")
	}
	status, err := e.Tax.Status(ctx, r.OldReceipt, p.Date)
	if err != nil {
		return err
	}
	switch status {
	case "active":
		if r.Phase == "replacing" || r.Phase == "cancelled" {
			return errors.New("original receipt active during replacement; manual review required")
		}
		r.Phase = "cancel_ready"
	case "cancelled":
		if r.Remaining > 0 {
			found, err := e.Tax.Find(ctx, r.Description, r.Remaining, p.Date)
			if err != nil {
				return err
			}
			if found != "" {
				return e.completeRefund(&r, &p, found)
			}
		}
		r.Phase = "cancelled"
	default:
		return errors.New("original receipt not found; manual review required")
	}
	r.Error = ""
	r.Next = time.Time{}
	return e.Store.Save(refundRecord(&r))
}
