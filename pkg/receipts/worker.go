// Copyright 2026 fxTunnel contributors. Licensed under Apache-2.0.
package receipts

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

var ErrWorkerStopped = errors.New("receipt worker is not running")

type operation struct {
	ctx    context.Context
	fn     func(context.Context, *Engine) error
	result chan error
}

// Worker embeds the engine into the host process. Run owns all tax API calls;
// Do serializes operator actions with scanning. Wake is safe for request paths.
type Worker struct {
	engine  *Engine
	wake    chan struct{}
	ops     chan operation
	done    chan struct{}
	started atomic.Bool
}

func NewWorker(e *Engine) *Worker {
	return &Worker{engine: e, wake: make(chan struct{}, 1), ops: make(chan operation), done: make(chan struct{})}
}
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}
func (w *Worker) Do(ctx context.Context, fn func(context.Context, *Engine) error) error {
	if !w.started.Load() {
		return ErrWorkerStopped
	}
	op := operation{ctx: ctx, fn: fn, result: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return ErrWorkerStopped
	case w.ops <- op:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return ErrWorkerStopped
	case err := <-op.result:
		return err
	}
}
func (w *Worker) Run(ctx context.Context) error {
	if !w.started.CompareAndSwap(false, true) {
		return errors.New("receipt worker can only run once")
	}
	defer close(w.done)
	if w.engine.Config.PollInterval <= 0 || w.engine.Config.RetryInterval <= 0 {
		return errors.New("receipt worker intervals must be positive")
	}
	poll := time.NewTicker(w.engine.Config.PollInterval)
	defer poll.Stop()
	retry := time.NewTicker(w.engine.Config.RetryInterval)
	defer retry.Stop()
	run := func(scan bool) {
		if w.engine.Config.Manual {
			return
		}
		cycle, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := w.engine.Run(cycle, scan); err != nil && ctx.Err() == nil {
			w.engine.logger().Error("receipt cycle requires attention", "error", err.Error())
		}
	}
	run(true)
	var delayed *time.Timer
	var delayedC <-chan time.Time
	defer func() {
		if delayed != nil {
			delayed.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
			run(true)
		case <-retry.C:
			run(false)
		case <-w.wake:
			if w.engine.Config.Manual {
				continue
			}
			// The scanner intentionally leaves a 30-second settlement margin.
			if delayed == nil {
				delayed = time.NewTimer(35 * time.Second)
				delayedC = delayed.C
			}
		case <-delayedC:
			delayed = nil
			delayedC = nil
			run(true)
		case op := <-w.ops:
			if err := op.ctx.Err(); err != nil {
				op.result <- err
				continue
			}
			cycle, cancel := context.WithTimeout(op.ctx, 2*time.Minute)
			stop := context.AfterFunc(ctx, cancel)
			err := op.fn(cycle, w.engine)
			stop()
			cancel()
			op.result <- err
		}
	}
}
