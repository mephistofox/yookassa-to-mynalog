package receipts

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestEmbeddedWorkerSerializesActionsAndStops(t *testing.T) {
	f := setup(t)
	f.e.Config.PollInterval = time.Hour
	f.e.Config.RetryInterval = time.Hour
	w := NewWorker(f.e)
	if err := w.Do(context.Background(), func(context.Context, *Engine) error { return nil }); err != ErrWorkerStopped {
		t.Fatal("inactive worker accepted an action")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	for deadline := time.Now().Add(time.Second); !w.started.Load(); {
		if time.Now().After(deadline) {
			t.Fatal("worker did not start")
		}
		time.Sleep(time.Millisecond)
	}
	var group sync.WaitGroup
	value := 0
	for i := 0; i < 10; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := w.Do(ctx, func(context.Context, *Engine) error { value++; return nil }); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if value != 10 {
		t.Fatal("operator actions were not serialized")
	}
	for i := 0; i < 100; i++ {
		w.Wake()
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
	if err := w.Do(context.Background(), func(context.Context, *Engine) error { return nil }); err != ErrWorkerStopped {
		t.Fatal("stopped worker accepted an action")
	}
}
