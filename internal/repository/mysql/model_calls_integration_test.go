//go:build integration

package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
)

func modelLease(t *testing.T, s *Store, kind string) *domain.Lease {
	t.Helper()
	ctx := context.Background()
	owner, videoJob := readyVideo(t, s)
	a, err := s.CreateAgent(ctx, owner, "model-budget-fixture", "", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	versions, err := s.ListAgentVersions(ctx, owner, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PublishAgentVersion(ctx, owner, versions[0].ID); err != nil {
		t.Fatal(err)
	}
	var jobID domain.ID
	if kind == "SUMMARY" {
		job, e := s.CreateSummaryPinned(ctx, owner, videoJob.VideoID, videoJob.VideoVersionID, versions[0].ID, domain.NewID().String())
		if e != nil {
			t.Fatal(e)
		}
		jobID = job.ID
	} else {
		session, e := s.CreateSession(ctx, owner, videoJob.VideoID, versions[0].ID, videoJob.VideoVersionID)
		if e != nil {
			t.Fatal(e)
		}
		run, e := s.CreateRun(ctx, owner, session.ID, "fixture", domain.NewID().String())
		if e != nil {
			t.Fatal(e)
		}
		jobID = run.JobID
	}
	t.Cleanup(func() { _ = s.CancelJob(context.Background(), owner, jobID) })
	return claim(t, s, "agent", kind)
}

func modelReservation(reserved, max int) domain.ModelReservation {
	return domain.ModelReservation{CallKey: domain.NewID().String(), RequestHash: strings.Repeat("a", 64), ProviderRef: "fixture", ModelID: "text-model", ReservedTokens: reserved, MaxTokens: max}
}

func TestMySQLModelBudget(t *testing.T) {
	s := isolatedStore(t)
	ctx := context.Background()
	t.Run("concurrent reservations cannot exceed job budget", func(t *testing.T) {
		l := modelLease(t, s, "SUMMARY")
		type outcome struct {
			call *domain.ModelCall
			req  domain.ModelReservation
			err  error
		}
		results := make(chan outcome, 8)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := modelReservation(300, 1000)
				call, _, err := s.ReserveModelCall(ctx, *l, req)
				results <- outcome{call, req, err}
			}()
		}
		wg.Wait()
		close(results)
		accepted := []outcome{}
		denied := 0
		for v := range results {
			if errors.Is(v.err, domain.ErrBudgetExceeded) {
				denied++
			} else if v.err != nil {
				t.Error(v.err)
			} else {
				accepted = append(accepted, v)
			}
		}
		if len(accepted) != 3 || denied != 5 {
			t.Fatalf("accepted=%d budget_denied=%d", len(accepted), denied)
		}
		first := accepted[0]
		if first.call.RunID != (domain.ID{}) || first.call.JobID != l.Job.ID || first.call.StepID != l.Step.ID || first.call.UsageKnown {
			t.Fatalf("incorrect reservation ownership: %+v", first.call)
		}
		if same, created, err := s.ReserveModelCall(ctx, *l, first.req); err != nil || created || same.ID != first.call.ID {
			t.Fatalf("reconciliation created another physical call: %v %v", created, err)
		}
		changed := first.req
		changed.RequestHash = strings.Repeat("b", 64)
		if _, _, err := s.ReserveModelCall(ctx, *l, changed); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("changed request reused reservation", err)
		}
		if _, _, err := s.ReserveModelCall(ctx, *l, modelReservation(1, 2000)); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("caller raised pinned budget", err)
		}
		if err := s.FinishModelCall(ctx, *l, first.call.ID, false, 0); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.ReserveModelCall(ctx, *l, modelReservation(101, 1000)); !errors.Is(err, domain.ErrBudgetExceeded) {
			t.Fatal("unknown outcome released budget", err)
		}
		if err := s.FinishModelCall(ctx, *l, first.call.ID, true, 50); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishModelCall(ctx, *l, first.call.ID, true, 50); err != nil {
			t.Fatal("identical settlement not idempotent", err)
		}
		if err := s.FinishModelCall(ctx, *l, first.call.ID, true, 49); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("known usage changed", err)
		}
		if err := s.FinishModelCall(ctx, *l, first.call.ID, false, 0); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.ReserveModelCall(ctx, *l, modelReservation(350, 1000)); err != nil {
			t.Fatal("settlement did not release unused estimate", err)
		}
		if err := s.FinishModelCall(ctx, *l, accepted[1].call.ID, true, 1200); err != nil {
			t.Fatal("actual usage over estimate must remain auditable", err)
		}
		if _, _, err := s.ReserveModelCall(ctx, *l, modelReservation(1, 1000)); !errors.Is(err, domain.ErrBudgetExceeded) {
			t.Fatal("overdrawn budget accepted another request", err)
		}
	})
	t.Run("lease recovery keeps unknown reservations and rejects stale settlement", func(t *testing.T) {
		l := modelLease(t, s, "SUMMARY")
		req := modelReservation(90, 100)
		call, _, err := s.ReserveModelCall(ctx, *l, req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.DB.Exec("UPDATE job_steps SET lease_until=TIMESTAMPADD(SECOND,-1,UTC_TIMESTAMP(6)) WHERE id=?", l.Step.ID); err != nil {
			t.Fatal(err)
		}
		if err = s.FinishModelCall(ctx, *l, call.ID, true, 10); !errors.Is(err, domain.ErrLeaseLost) {
			t.Fatal("expired lease settled usage", err)
		}
		if _, _, err = s.ReserveModelCall(ctx, *l, modelReservation(1, 100)); !errors.Is(err, domain.ErrLeaseLost) {
			t.Fatal("expired lease reserved budget", err)
		}
		if _, err = s.RecoverExpired(ctx, 100, 3); err != nil {
			t.Fatal(err)
		}
		if _, err = s.DispatchDue(ctx, 100); err != nil {
			t.Fatal(err)
		}
		next := claim(t, s, "agent", "SUMMARY")
		if same, created, err := s.ReserveModelCall(ctx, *next, req); err != nil || created || same.ID != call.ID {
			t.Fatal("recovered reservation changed", err)
		}
		if _, _, err = s.ReserveModelCall(ctx, *next, modelReservation(20, 100)); !errors.Is(err, domain.ErrBudgetExceeded) {
			t.Fatal("reclaim forgot unknown usage", err)
		}
		foreign := modelLease(t, s, "SUMMARY")
		if err = s.FinishModelCall(ctx, *foreign, call.ID, true, 0); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("another task settled reservation", err)
		}
		if err = s.FinishModelCall(ctx, *next, call.ID, true, 10); err != nil {
			t.Fatal(err)
		}
		if _, _, err = s.ReserveModelCall(ctx, *next, modelReservation(90, 100)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("QA records run attribution and genuine zero usage", func(t *testing.T) {
		l := modelLease(t, s, "QA")
		call, _, err := s.ReserveModelCall(ctx, *l, modelReservation(100, 100))
		if err != nil || call.RunID == (domain.ID{}) {
			t.Fatal("missing QA attribution", err)
		}
		if err = s.FinishModelCall(ctx, *l, call.ID, true, 0); err != nil {
			t.Fatal(err)
		}
		if _, _, err = s.ReserveModelCall(ctx, *l, modelReservation(100, 100)); err != nil {
			t.Fatal("known zero usage treated as unknown", err)
		}
	})
}
