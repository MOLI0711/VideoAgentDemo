// Package workflow runs the fixed video pipeline using durable execution leases.
// It never treats a goroutine or a Kafka offset as the task's source of truth.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
)

type Handler func(context.Context, *domain.Lease) (domain.StepResult, error)

type Repository interface {
	ClaimReadyTypes(context.Context, string, string, time.Duration, []string) (*domain.Lease, error)
	RenewLease(context.Context, domain.Lease, time.Duration) error
	CompleteStep(context.Context, domain.Lease, domain.StepResult) error
	RetryStep(context.Context, domain.Lease, string, string, time.Duration, int) error
	FailStep(context.Context, domain.Lease, string, string) error
	MarkExternalUnknown(context.Context, domain.Lease) error
	DispatchLocal(context.Context, int) (int, error)
	DispatchDue(context.Context, int) (int, error)
	RecoverExpired(context.Context, int, int) (int, error)
	ExpireDeadlines(context.Context, int) (int, error)
}

type Options struct {
	WorkerID          string
	WorkerKind        string
	Concurrency       int
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	PollInterval      time.Duration
	StageTimeout      time.Duration
	MaxRetries        int
	DispatchMode      string
	RecoverExpired    bool
	Logger            *slog.Logger
}

type Engine struct {
	store    Repository
	options  Options
	handlers map[string]Handler
}

func New(store Repository, options Options) *Engine {
	if options.WorkerID == "" {
		options.WorkerID = domain.NewID().String()
	}
	if options.WorkerKind == "" {
		options.WorkerKind = "media"
	}
	if options.Concurrency <= 0 {
		options.Concurrency = 1
	}
	if options.LeaseDuration <= 0 {
		options.LeaseDuration = 120 * time.Second
	}
	if options.HeartbeatInterval <= 0 {
		options.HeartbeatInterval = options.LeaseDuration / 4
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 500 * time.Millisecond
	}
	if options.MaxRetries <= 0 {
		options.MaxRetries = 3
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &Engine{store: store, options: options, handlers: map[string]Handler{}}
}

// Register must be called before Run. Empty names and duplicate registrations
// panic during application assembly, rather than silently replacing an executor.
func (e *Engine) Register(stepType string, handler Handler) {
	if stepType == "" || handler == nil {
		panic("workflow: invalid handler")
	}
	if _, ok := e.handlers[stepType]; ok {
		panic("workflow: duplicate handler " + stepType)
	}
	e.handlers[stepType] = handler
}

func (e *Engine) Run(ctx context.Context) error {
	if len(e.handlers) == 0 {
		return errors.New("workflow: no registered handlers")
	}
	if e.options.HeartbeatInterval >= e.options.LeaseDuration {
		return errors.New("workflow: heartbeat must be shorter than lease")
	}
	if e.options.DispatchMode != "" && e.options.DispatchMode != "db_dev" && e.options.DispatchMode != "kafka" {
		return errors.New("workflow: invalid dispatch mode")
	}
	types := make([]string, 0, len(e.handlers))
	for k := range e.handlers {
		types = append(types, k)
	}
	sort.Strings(types)
	var wg sync.WaitGroup
	if e.options.DispatchMode == "db_dev" {
		wg.Add(1)
		go func() { defer wg.Done(); e.coordinate(ctx) }()
	}
	for i := 0; i < e.options.Concurrency; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			worker := fmt.Sprintf("%s-%d", e.options.WorkerID, slot)
			for ctx.Err() == nil {
				lease, err := e.store.ClaimReadyTypes(ctx, e.options.WorkerKind, worker, e.options.LeaseDuration, types)
				if err != nil {
					if !errors.Is(err, domain.ErrNotFound) && ctx.Err() == nil {
						e.options.Logger.Error("claim step", "error", err, "worker_kind", e.options.WorkerKind)
					}
					if !wait(ctx, e.options.PollInterval) {
						return
					}
					continue
				}
				e.execute(ctx, lease)
			}
		}(i)
	}
	wg.Wait()
	return nil
}

func (e *Engine) coordinate(ctx context.Context) {
	for ctx.Err() == nil {
		if _, err := e.store.DispatchDue(ctx, 100); err != nil && ctx.Err() == nil {
			e.options.Logger.Error("dispatch due steps", "error", err)
		}
		if _, err := e.store.DispatchLocal(ctx, 100); err != nil && ctx.Err() == nil {
			e.options.Logger.Error("local dispatch", "error", err)
		}
		if e.options.RecoverExpired {
			if _, err := e.store.RecoverExpired(ctx, 100, e.options.MaxRetries); err != nil && ctx.Err() == nil {
				e.options.Logger.Error("recover leases", "error", err)
			}
		}
		if _, err := e.store.ExpireDeadlines(ctx, 100); err != nil && ctx.Err() == nil {
			e.options.Logger.Error("expire task deadlines", "error", err)
		}
		if !wait(ctx, e.options.PollInterval) {
			return
		}
	}
}

func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type ExecutionError struct {
	Code       string
	Cause      error
	Retryable  bool
	RetryAfter time.Duration
}

func (e *ExecutionError) Error() string {
	if e.Cause != nil {
		return e.Code + ": " + e.Cause.Error()
	}
	return e.Code
}
func (e *ExecutionError) Unwrap() error { return e.Cause }

func (e *Engine) execute(parent context.Context, lease *domain.Lease) {
	deadline := lease.Job.DeadlineAt
	if e.options.StageTimeout > 0 && time.Now().Add(e.options.StageTimeout).Before(deadline) {
		deadline = time.Now().Add(e.options.StageTimeout)
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	heartbeatDone := make(chan struct{})
	lost := make(chan error, 1)
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(e.options.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := e.store.RenewLease(ctx, *lease, e.options.LeaseDuration); err != nil {
					lost <- err
					cancel()
					return
				}
			}
		}
	}()
	result, runErr := invoke(e.handlers[lease.Step.StepType], ctx, lease)
	if parent.Err() != nil {
		cancel()
		<-heartbeatDone
		return
	}
	select {
	case err := <-lost:
		e.options.Logger.Warn("lease renewal failed; result discarded", "step_id", lease.Step.ID, "error", err)
		cancel()
		<-heartbeatDone
		return
	default:
	}
	var commitErr error
	if runErr == nil {
		commitErr = e.store.CompleteStep(ctx, *lease, result)
	} else if errors.Is(runErr, domain.ErrCommitUnknown) || errors.Is(runErr, domain.ErrLeaseLost) {
		// A checkpoint/intent may have committed even though its acknowledgement
		// was lost. Do not replace durable progress with a terminal failure or
		// immediately repeat the external action; recovery reloads MySQL state.
		commitErr = runErr
	} else if errors.Is(runErr, domain.ErrSubmissionUnknown) {
		commitErr = e.store.MarkExternalUnknown(ctx, *lease)
	} else {
		var execution *ExecutionError
		code := "EXECUTION_FAILED"
		retry := false
		delay := 2 * time.Second
		if errors.As(runErr, &execution) {
			code = execution.Code
			retry = execution.Retryable
			if execution.RetryAfter > 0 {
				delay = execution.RetryAfter
			}
		}
		if errors.Is(runErr, context.DeadlineExceeded) {
			code = "EXECUTION_TIMEOUT"
		}
		message := runErr.Error()
		if len(message) > 900 {
			message = message[:900]
		}
		if retry {
			commitErr = e.store.RetryStep(parent, *lease, code, message, delay, e.options.MaxRetries)
		} else {
			commitErr = e.store.FailStep(parent, *lease, code, message)
		}
	}
	cancel()
	<-heartbeatDone
	if commitErr != nil && !errors.Is(commitErr, domain.ErrLeaseLost) && parent.Err() == nil {
		e.options.Logger.Error("step state commit failed; recovery owns retry", "step_id", lease.Step.ID, "attempt_no", lease.AttemptNo, "error", commitErr)
	}
}

func invoke(handler Handler, ctx context.Context, lease *domain.Lease) (result domain.StepResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &ExecutionError{Code: "EXECUTOR_PANIC", Cause: fmt.Errorf("%v", p)}
		}
	}()
	return handler(ctx, lease)
}
