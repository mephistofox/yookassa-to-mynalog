// Copyright 2026 fxTunnel contributors. Licensed under Apache-2.0.
// Go adaptation of grandvan709/yookassa-to-mynalog; see NOTICE.
package receipts

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

// Money is an integer number of kopecks. No API amount passes through float64.
type Money int64

func ParseMoney(s string) (Money, error) {
	p := strings.Split(s, ".")
	if len(p) != 2 || len(p[1]) != 2 || len(p[0]) == 0 || len(p[0]) > 14 {
		return 0, errors.New("amount must have two decimal places")
	}
	for _, c := range p[0] + p[1] {
		if c < '0' || c > '9' {
			return 0, errors.New("invalid amount")
		}
	}
	n, err := strconv.ParseInt(p[0]+p[1], 10, 64)
	if err != nil || n > math.MaxInt64/2 {
		return 0, errors.New("amount overflow")
	}
	return Money(n), nil
}

func (m Money) String() string { return fmt.Sprintf("%d.%02d", int64(m)/100, int64(m)%100) }

func validEmail(s string) bool {
	if strings.ContainsAny(s, "\r\n") {
		return false
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && !strings.ContainsAny(s, "<>")
}

type Amount struct {
	Value    string `json:"value"`
	Currency string `json:"currency"`
}
type Payment struct {
	ID             string            `json:"id"`
	Status         string            `json:"status"`
	Paid           bool              `json:"paid"`
	Test           bool              `json:"test"`
	Amount         Amount            `json:"amount"`
	RefundedAmount Amount            `json:"refunded_amount"`
	CapturedAt     time.Time         `json:"captured_at"`
	CreatedAt      time.Time         `json:"created_at"`
	Description    string            `json:"description"`
	Metadata       map[string]string `json:"metadata"`
}
type Refund struct {
	ID        string    `json:"id"`
	PaymentID string    `json:"payment_id"`
	Status    string    `json:"status"`
	Amount    Amount    `json:"amount"`
	CreatedAt time.Time `json:"created_at"`
}

type PaymentJob struct {
	ID          string    `json:"id"`
	Phase       string    `json:"phase"`
	Amount      Money     `json:"amount"`
	Remaining   Money     `json:"remaining"`
	Date        time.Time `json:"date"`
	Description string    `json:"description"`
	Email       string    `json:"email"`
	Receipt     string    `json:"receipt,omitempty"`
	Attempts    int       `json:"attempts"`
	Next        time.Time `json:"next,omitempty"`
	Error       string    `json:"error,omitempty"`
}
type RefundJob struct {
	ID          string    `json:"id"`
	PaymentID   string    `json:"payment_id"`
	Phase       string    `json:"phase"`
	Amount      Money     `json:"amount"`
	Currency    string    `json:"currency"`
	CreatedAt   time.Time `json:"created_at"`
	OldReceipt  string    `json:"old_receipt,omitempty"`
	Remaining   Money     `json:"remaining"`
	Description string    `json:"description,omitempty"`
	Attempts    int       `json:"attempts"`
	Next        time.Time `json:"next,omitempty"`
	Error       string    `json:"error,omitempty"`
}
type Delivery struct {
	ID        string    `json:"id"`
	Phase     string    `json:"phase"`
	Email     string    `json:"email"`
	Receipt   string    `json:"receipt"`
	Amount    Money     `json:"amount"`
	Cancelled bool      `json:"cancelled,omitempty"`
	Attempts  int       `json:"attempts"`
	Next      time.Time `json:"next,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type Income struct {
	Name          string          `json:"name"`
	OperationTime time.Time       `json:"operationTime"`
	ID            string          `json:"approvedReceiptUuid"`
	ReceiptID     string          `json:"receiptUuid"`
	Total         json.RawMessage `json:"totalAmount"`
	Cancellation  json.RawMessage `json:"cancellationInfo"`
}

func (i Income) UUID() string {
	if i.ID != "" {
		return i.ID
	}
	return i.ReceiptID
}
func (i Income) Cancelled() bool {
	s := strings.TrimSpace(string(i.Cancellation))
	return s != "" && s != "null" && s != "false" && s != "{}"
}
func (i Income) Money() (Money, error) {
	s := strings.TrimSpace(string(i.Total))
	invalid := errors.New("invalid FNS amount")
	if len(s) == 0 || len(s) > 128 {
		return 0, invalid
	}
	if s[0] == '"' {
		if err := json.Unmarshal([]byte(s), &s); err != nil {
			return 0, invalid
		}
	}
	if len(s) == 0 || len(s) > 64 || s[0] < '0' || s[0] > '9' || !json.Valid([]byte(s)) {
		return 0, invalid
	}
	// FNS returns scientific notation; bound its exponent before exact rational parsing.
	if pos := strings.IndexAny(s, "eE"); pos >= 0 {
		exponent, err := strconv.Atoi(s[pos+1:])
		if err != nil || exponent < -20 || exponent > 20 {
			return 0, invalid
		}
	}
	amount, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, invalid
	}
	amount.Mul(amount, big.NewRat(100, 1))
	if !amount.IsInt() || !amount.Num().IsInt64() {
		return 0, invalid
	}
	return ParseMoney(Money(amount.Num().Int64()).String())
}

// APIError intentionally never exposes a remote response body or credentials.
type APIError struct {
	Service   string
	Status    int
	Retryable bool
	Uncertain bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s request failed (HTTP %d)", e.Service, e.Status)
}
func retryable(err error) bool { var e *APIError; return errors.As(err, &e) && e.Retryable }
func uncertain(err error) bool { var e *APIError; return errors.As(err, &e) && e.Uncertain }

func backoff(n int) time.Duration {
	if n > 8 {
		n = 8
	}
	if n < 1 {
		n = 1
	}
	return time.Duration(1<<uint(n-1)) * 15 * time.Second
}
