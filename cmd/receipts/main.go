// Copyright 2026 fxTunnel contributors. Licensed under Apache-2.0.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mephistofox/yookassa-to-mynalog/pkg/receipts"
	"github.com/mephistofox/yookassa-to-mynalog/pkg/sqlitestate"
)

var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("command failed", "error", err.Error())
		os.Exit(1)
	}
}
func printJSON(v any) error { return json.NewEncoder(os.Stdout).Encode(v) }
func run() error {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if command == "version" {
		fmt.Println(version)
		return nil
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	dbDefault := os.Getenv("STATE_DB")
	if f := os.Getenv("STATE_DB_FILE"); f != "" {
		if dbDefault != "" {
			return errors.New("set only STATE_DB or STATE_DB_FILE")
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return errors.New("cannot read STATE_DB_FILE")
		}
		dbDefault = strings.TrimSpace(string(b))
	}
	if dbDefault == "" {
		dbDefault = "data/receipts-go.db"
	}
	dbPath := flags.String("db", dbDefault, "SQLite state path")
	destination := flags.String("out", "", "new backup path")
	paymentID := flags.String("payment", "", "payment ID for operator action")
	refundID := flags.String("refund", "", "refund ID for operator action")
	email := flags.String("email", "", "verified customer email")
	confirmed := flags.Bool("confirmed-absent", false, "operator verified that no income receipt exists in My Nalog")
	confirmedState := flags.Bool("confirmed-state", false, "operator verified cancellation and replacement in My Nalog")
	if len(os.Args) > 2 {
		if err := flags.Parse(os.Args[2:]); err != nil {
			return err
		}
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if command == "status" || command == "backup" {
		s, err := sqlitestate.Open(*dbPath, false)
		if err != nil {
			return err
		}
		defer s.Close()
		if command == "backup" {
			if *destination == "" {
				return errors.New("--out is required")
			}
			return s.Backup(ctx, *destination)
		}
		counts, err := s.Counts()
		if err != nil {
			return err
		}
		payments, err := s.Meta("checkpoint.payments")
		if err != nil {
			return err
		}
		refunds, err := s.Meta("checkpoint.refunds")
		if err != nil {
			return err
		}
		pending, err := s.Pending()
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"counts": counts, "pending": pending, "payment_checkpoint": payments, "refund_checkpoint": refunds})
	}
	switch command {
	case "serve", "once", "check", "retry-payment", "retry-refund", "set-email":
	default:
		return errors.New("commands: serve, once, check, status, backup, retry-payment, retry-refund, set-email, version")
	}
	c, err := receipts.LoadConfig()
	if err != nil {
		return err
	}
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "db" {
			c.DBPath = *dbPath
		}
	})
	s, err := sqlitestate.Open(c.DBPath, true)
	if err != nil {
		return err
	}
	defer s.Close()
	n, err := receipts.NewNalog(c, s)
	if err != nil {
		return err
	}
	y := receipts.NewYooKassa(c)
	e := &receipts.Engine{Config: c, Store: s, Source: y, Tax: n, Sender: &receipts.SMTPSender{Config: c}}
	switch command {
	case "check":
		checkCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
		defer stop()
		if err = n.Authenticate(checkCtx); err != nil {
			return err
		}
		count := 0
		end := time.Now().UTC().Add(-30 * time.Second)
		if !c.Start.After(end) {
			if err = y.Payments(checkCtx, c.Start, end, func(receipts.Payment) error { count++; return nil }); err != nil {
				return err
			}
		}
		return printJSON(map[string]any{"nalog_auth": "ok", "payments_in_window": count, "tax_writes": 0, "smtp_delivery_tested": false})
	case "set-email":
		if *paymentID == "" {
			return errors.New("--payment is required")
		}
		return e.SetEmail(*paymentID, *email)
	case "retry-payment":
		if *paymentID == "" {
			return errors.New("--payment is required")
		}
		return e.RetryPayment(ctx, *paymentID, *confirmed)
	case "retry-refund":
		if *refundID == "" {
			return errors.New("--refund is required")
		}
		return e.RetryRefund(ctx, *refundID, *confirmedState)
	case "once":
		cycle, stop := context.WithTimeout(ctx, 2*time.Minute)
		defer stop()
		return e.Run(cycle, true)
	}
	return serve(ctx, e)
}
func serve(ctx context.Context, e *receipts.Engine) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"alive": true})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		health, err := e.Health()
		if err != nil {
			w.WriteHeader(503)
			json.NewEncoder(w).Encode(map[string]string{"error": "state unavailable"})
			return
		}
		if !health.Ready {
			w.WriteHeader(503)
		}
		json.NewEncoder(w).Encode(health)
	})
	server := &http.Server{Addr: e.Config.Listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.ListenAndServe() }()
	defer func() {
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		server.Shutdown(shutdown)
	}()
	poll := time.NewTicker(e.Config.PollInterval)
	defer poll.Stop()
	retry := time.NewTicker(e.Config.RetryInterval)
	defer retry.Stop()
	run := func(scan bool) {
		cycle, stop := context.WithTimeout(ctx, 2*time.Minute)
		defer stop()
		if err := e.Run(cycle, scan); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("receipt cycle requires attention", "error", err.Error())
		}
	}
	run(true)
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-serverErr:
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		case <-poll.C:
			run(true)
		case <-retry.C:
			run(false)
		}
	}
}
