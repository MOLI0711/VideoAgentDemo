package workflow

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
)

type testRepository struct {
	mu           sync.Mutex
	claimed      bool
	renewalError error
	completions  int
	failures     int
	unknown      int
	published    chan struct{}
}

func (r *testRepository) ClaimReadyTypes(ctx context.Context, kind, worker string, ttl time.Duration, types []string) (*domain.Lease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.claimed {
		return nil, domain.ErrNotFound
	}
	r.claimed = true
	return &domain.Lease{Job: domain.Job{ID: domain.NewID(), DeadlineAt: time.Now().Add(time.Minute)}, Step: domain.Step{ID: domain.NewID(), StepType: "TEST"}, WorkerID: worker, AttemptNo: 1}, nil
}
func (r *testRepository) RenewLease(context.Context, domain.Lease, time.Duration) error {
	return r.renewalError
}
func (r *testRepository) CompleteStep(context.Context, domain.Lease, domain.StepResult) error {
	r.mu.Lock()
	r.completions++
	r.mu.Unlock()
	r.published <- struct{}{}
	return nil
}
func (r *testRepository) RetryStep(context.Context, domain.Lease, string, string, time.Duration, int) error {
	return nil
}
func (r *testRepository) FailStep(context.Context, domain.Lease, string, string) error {
	r.mu.Lock()
	r.failures++
	r.mu.Unlock()
	r.published <- struct{}{}
	return nil
}
func (r *testRepository) MarkExternalUnknown(context.Context, domain.Lease) error {
	r.mu.Lock()
	r.unknown++
	r.mu.Unlock()
	r.published <- struct{}{}
	return nil
}
func (r *testRepository) DispatchLocal(context.Context, int) (int, error)       { return 0, nil }
func (r *testRepository) DispatchDue(context.Context, int) (int, error)         { return 0, nil }
func (r *testRepository) RecoverExpired(context.Context, int, int) (int, error) { return 0, nil }
func (r *testRepository) ExpireDeadlines(context.Context, int) (int, error)     { return 0, nil }

func TestLostHeartbeatDiscardsEvenSuccessfulHandlerResult(t *testing.T) {
	r := &testRepository{renewalError: domain.ErrLeaseLost, published: make(chan struct{}, 1)}
	e := New(r, Options{LeaseDuration: time.Second, HeartbeatInterval: time.Millisecond, PollInterval: time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	handlerStopped := make(chan struct{})
	e.Register("TEST", func(ctx context.Context, l *domain.Lease) (domain.StepResult, error) {
		<-ctx.Done()
		close(handlerStopped)
		return domain.StepResult{}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	select {
	case <-handlerStopped:
		cancel()
	case <-ctx.Done():
		t.Fatal("handler did not receive lost-lease cancellation")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.completions != 0 || r.failures != 0 {
		t.Fatalf("lost owner published result or failure: %+v", r)
	}
}

func TestUnknownSubmissionUsesReconciliationPath(t *testing.T) {
	r := &testRepository{published: make(chan struct{}, 1)}
	e := New(r, Options{PollInterval: time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	e.Register("TEST", func(context.Context, *domain.Lease) (domain.StepResult, error) {
		return domain.StepResult{}, domain.ErrSubmissionUnknown
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	select {
	case <-r.published:
		cancel()
	case <-ctx.Done():
		t.Fatal("no terminal transition")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unknown != 1 || r.completions != 0 || r.failures != 0 {
		t.Fatal("unknown submission treated as ordinary retry/failure")
	}
}

func TestUnknownCheckpointCommitLeavesRecoveryInControl(t *testing.T) {
	r := &testRepository{published: make(chan struct{}, 1)}
	e := New(r, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	e.Register("TEST", func(context.Context, *domain.Lease) (domain.StepResult, error) {
		return domain.StepResult{}, domain.ErrCommitUnknown
	})
	e.execute(context.Background(), &domain.Lease{Job: domain.Job{DeadlineAt: time.Now().Add(time.Minute)}, Step: domain.Step{StepType: "TEST"}})
	if r.completions != 0 || r.failures != 0 || r.unknown != 0 {
		t.Fatalf("unknown database commit became a new state transition: %+v", r)
	}
}

func TestPanicBecomesExplicitFailure(t *testing.T) {
	_, err := invoke(func(context.Context, *domain.Lease) (domain.StepResult, error) { panic("broken executor") }, context.Background(), &domain.Lease{})
	var e *ExecutionError
	if !errors.As(err, &e) || e.Code != "EXECUTOR_PANIC" {
		t.Fatalf("panic error=%v", err)
	}
}
