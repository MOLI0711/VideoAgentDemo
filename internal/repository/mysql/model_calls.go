package mysql

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
	"strings"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/jmoiron/sqlx"
)

// ReserveModelCall serializes each job's text-model quota with the same parent,
// job and step locks used by execution. Unknown outcomes retain their reservation.
// created=false is reconciliation only, never permission to replay a network call.
func (s *Store) ReserveModelCall(ctx context.Context, l domain.Lease, request domain.ModelReservation) (*domain.ModelCall, bool, error) {
	request.RequestHash = strings.ToLower(request.RequestHash)
	_, hashErr := hex.DecodeString(request.RequestHash)
	if request.CallKey == "" || len(request.CallKey) > 128 || len(request.RequestHash) != 64 || hashErr != nil || request.ProviderRef == "" || len(request.ProviderRef) > 100 || request.ModelID == "" || len(request.ModelID) > 200 || request.ReservedTokens <= 0 || request.ReservedTokens > math.MaxInt32 || request.MaxTokens <= 0 || request.MaxTokens > math.MaxInt32 {
		return nil, false, domain.ErrInvalid
	}
	var out domain.ModelCall
	created := false
	err := s.tx(ctx, func(t *sqlx.Tx) error {
		j, step, run, err := lockLease(ctx, t, l)
		if err != nil {
			return err
		}
		if j.Type != "QA" && j.Type != "SUMMARY" {
			return domain.ErrInvalid
		}
		err = t.GetContext(ctx, &out, "SELECT * FROM model_calls WHERE step_id=? AND call_key=?", step.ID, request.CallKey)
		if err == nil {
			if out.JobID != j.ID || out.RequestHash != request.RequestHash || out.ProviderRef != request.ProviderRef || out.ModelID != request.ModelID || out.ReservedTokens != request.ReservedTokens || out.BudgetLimit != request.MaxTokens {
				return domain.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var budget struct {
			Used int64 `db:"used"`
			Min  int   `db:"min_limit"`
			Max  int   `db:"max_limit"`
		}
		if err = t.GetContext(ctx, &budget, "SELECT COALESCE(SUM(IF(usage_known,actual_tokens,reserved_tokens)),0) AS used,COALESCE(MIN(budget_limit),0) AS min_limit,COALESCE(MAX(budget_limit),0) AS max_limit FROM model_calls WHERE job_id=?", j.ID); err != nil {
			return err
		}
		if budget.Max != 0 && (budget.Min != request.MaxTokens || budget.Max != request.MaxTokens) {
			return domain.ErrConflict
		}
		if budget.Used+int64(request.ReservedTokens) > int64(request.MaxTokens) {
			return domain.ErrBudgetExceeded
		}
		runID := domain.ID{}
		if run != nil {
			runID = run.ID
		}
		id := domain.NewID()
		if _, err = t.ExecContext(ctx, "INSERT INTO model_calls(id,run_id,job_id,step_id,call_key,request_hash,attempt_no,provider_ref,model_id,status,reserved_tokens,actual_tokens,usage_known,budget_limit) VALUES(?,?,?,?,?,?,?,?,?,'RESERVED',?,0,FALSE,?)", id, runID, j.ID, step.ID, request.CallKey, request.RequestHash, l.AttemptNo, request.ProviderRef, request.ModelID, request.ReservedTokens, request.MaxTokens); err != nil {
			return err
		}
		created = true
		return t.GetContext(ctx, &out, "SELECT * FROM model_calls WHERE id=?", id)
	})
	if err != nil {
		return nil, false, err
	}
	return &out, created, nil
}

// FinishModelCall settles even a response that later fails JSON validation.
// Missing usage remains UNKNOWN and consumes reserved tokens. Known usage is
// immutable, including genuine zero usage, and may exceed the initial estimate.
func (s *Store) FinishModelCall(ctx context.Context, l domain.Lease, callID domain.ID, usageKnown bool, actualTokens int) error {
	if callID == (domain.ID{}) || actualTokens < 0 || actualTokens > math.MaxInt32 || (!usageKnown && actualTokens != 0) {
		return domain.ErrInvalid
	}
	return s.tx(ctx, func(t *sqlx.Tx) error {
		j, step, _, err := lockLease(ctx, t, l)
		if err != nil {
			return err
		}
		var call domain.ModelCall
		if err = t.GetContext(ctx, &call, "SELECT * FROM model_calls WHERE id=? AND job_id=? AND step_id=? FOR UPDATE", callID, j.ID, step.ID); err != nil {
			return err
		}
		if call.UsageKnown {
			if usageKnown && call.ActualTokens != actualTokens {
				return domain.ErrConflict
			}
			return nil // Never regress already known usage to unknown.
		}
		if !usageKnown {
			_, err = t.ExecContext(ctx, "UPDATE model_calls SET status='UNKNOWN' WHERE id=?", call.ID)
			return err
		}
		_, err = t.ExecContext(ctx, "UPDATE model_calls SET status='SETTLED',usage_known=TRUE,actual_tokens=? WHERE id=?", actualTokens, call.ID)
		return err
	})
}
