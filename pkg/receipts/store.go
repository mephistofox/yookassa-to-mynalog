// Copyright 2026 fxTunnel contributors. Licensed under Apache-2.0.
package receipts

import (
	"encoding/json"
	"github.com/mephistofox/yookassa-to-mynalog/pkg/state"
)

type StateStore = state.Store
type Record = state.Record

func listAs[T any](s StateStore, kind string) ([]T, error) {
	rows, err := s.List(kind)
	if err != nil {
		return nil, err
	}
	result := make([]T, 0, len(rows))
	for _, b := range rows {
		var v T
		if err = json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, nil
}

func paymentRecord(p *PaymentJob) Record {
	return Record{Kind: "payment", ID: p.ID, Phase: p.Phase, Value: p}
}
func refundRecord(r *RefundJob) Record {
	return Record{Kind: "refund", ID: r.ID, Phase: r.Phase, Value: r}
}
func deliveryRecord(d *Delivery) Record {
	return Record{Kind: "delivery", ID: d.ID, Phase: d.Phase, Value: d}
}
