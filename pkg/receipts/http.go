// Copyright 2026 fxTunnel contributors. Licensed under Apache-2.0.
package receipts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

func newHTTPClient(timeout time.Duration, sourceIP string) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if sourceIP != "" {
		d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(sourceIP)}}
		transport.DialContext = d.DialContext
	}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("API redirects are disabled") }}
}
func jsonRequest(ctx context.Context, method, address string, value any) (*http.Request, error) {
	var data []byte
	var err error
	if value != nil {
		data, err = json.Marshal(value)
		if err != nil {
			return nil, err
		}
	}
	r, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Accept", "application/json")
	if value != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return r, nil
}
func execute(client *http.Client, r *http.Request, service string, write bool, out any) error {
	response, err := client.Do(r)
	if err != nil {
		return &APIError{Service: service, Retryable: true, Uncertain: write}
	}
	defer response.Body.Close()
	status := response.StatusCode
	if status != http.StatusOK {
		// Never log or retain a remote response body: it may contain PII/tokens.
		return &APIError{Service: service, Status: status, Retryable: status == 408 || status == 425 || status == 429 || status >= 500, Uncertain: write && (status == 408 || status >= 500)}
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil || len(b) > 8<<20 {
		return &APIError{Service: service, Status: 200, Retryable: true, Uncertain: write}
	}
	if out == nil {
		return nil
	}
	if json.Unmarshal(b, out) != nil {
		return &APIError{Service: service, Status: 200, Retryable: true, Uncertain: write}
	}
	return nil
}
