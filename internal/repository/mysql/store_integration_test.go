//go:build integration

package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	driver "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

// Every invocation creates and removes only its own randomly named database.
// No test truncates, migrates, or drops the application database in the DSN.
func isolatedStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("VA_MYSQL_TEST_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set VA_MYSQL_TEST_ADMIN_DSN to run real MySQL tests")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid test admin DSN")
	}
	cfg.DBName = ""
	cfg.ParseTime = true
	admin, err := sqlx.Connect("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatalf("connect test MySQL: %v", err)
	}
	name := "molivideo_test_" + strings.ReplaceAll(domain.NewID().String(), "-", "")
	if _, err = admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg.DBName = name
	store, err := Open(context.Background(), cfg.FormatDSN())
	if err != nil {
		admin.Exec("DROP DATABASE `" + name + "`")
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.Close()
		if _, err := admin.Exec("DROP DATABASE `" + name + "`"); err != nil {
			t.Errorf("remove isolated test database: %v", err)
		}
		admin.Close()
	})
	if err = store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(context.Background()); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	var version string
	if err = store.DB.Get(&version, "SELECT VERSION()"); err != nil {
		t.Fatal(err)
	}
	t.Logf("real MySQL version: %s", version)
	return store
}

func fixtureJob(t *testing.T, s *Store) (domain.ID, *domain.Job) {
	t.Helper()
	ctx := context.Background()
	u, err := s.SeedUser(ctx, "test-"+domain.NewID().String(), "test-only-password-hash")
	if err != nil {
		t.Fatal(err)
	}
	up, err := s.CreateUpload(ctx, u.ID, domain.CreateUpload{Title: "Test", Filename: "sample.mp4", ContentType: "video/mp4", Size: 10, ObjectKey: "tests/" + domain.NewID().String(), MultipartID: "test-multipart", IdempotencyKey: domain.NewID().String()})
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.CompleteUpload(ctx, u.ID, up.ID, domain.Asset{ObjectKey: up.ObjectKey, Size: 10, StorageBackend: "s3"}, json.RawMessage(`{"fixture":true}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.CancelJob(context.Background(), u.ID, job.ID) })
	return u.ID, job
}

func claim(t *testing.T, s *Store, kind, typ string) *domain.Lease {
	t.Helper()
	if _, err := s.DispatchLocal(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	l, err := s.ClaimReadyTypes(context.Background(), kind, "test-worker", time.Minute, []string{typ})
	if err != nil {
		t.Fatalf("claim %s: %v", typ, err)
	}
	return l
}
func complete(t *testing.T, s *Store, l *domain.Lease, result domain.StepResult) {
	t.Helper()
	if err := s.CompleteStep(context.Background(), *l, result); err != nil {
		t.Fatalf("complete %s: %v", l.Step.StepType, err)
	}
}
func readyVideo(t *testing.T, s *Store) (domain.ID, *domain.Job) {
	t.Helper()
	owner, job := fixtureJob(t, s)
	for _, typ := range []string{"PROBE", "EXTRACT_AUDIO", "EXTRACT_FRAMES", "ASR", "OCR", "BUILD_SEGMENTS"} {
		complete(t, s, claim(t, s, "media", typ), domain.StepResult{})
	}
	complete(t, s, claim(t, s, "agent", "BUILD_INDEX"), domain.StepResult{IndexName: "test-index", IndexBuildID: domain.NewID().String()})
	return owner, job
}

func TestMySQLTransactions(t *testing.T) {
	s := isolatedStore(t)
	ctx := context.Background()
	t.Run("concurrent request idempotency", func(t *testing.T) {
		u, err := s.SeedUser(ctx, "idem-"+domain.NewID().String(), "test")
		if err != nil {
			t.Fatal(err)
		}
		input := domain.CreateUpload{Title: "same", Filename: "a.mp4", ContentType: "video/mp4", Size: 12, ObjectKey: "test/" + domain.NewID().String(), MultipartID: "multipart", IdempotencyKey: "same-key"}
		var wg sync.WaitGroup
		ids := make(chan domain.ID, 8)
		errs := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				v, e := s.CreateUpload(ctx, u.ID, input)
				if e != nil {
					errs <- e
					return
				}
				ids <- v.ID
			}()
		}
		wg.Wait()
		close(ids)
		close(errs)
		for e := range errs {
			t.Error(e)
		}
		var first domain.ID
		for id := range ids {
			if first == (domain.ID{}) {
				first = id
			}
			if id != first {
				t.Error("idempotent request created multiple resources")
			}
		}
		input.Size++
		if _, err = s.CreateUpload(ctx, u.ID, input); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("changed body error=%v", err)
		}
	})
	t.Run("outbox replay and inbox acceptance", func(t *testing.T) {
		_, job := fixtureJob(t, s)
		events, err := s.ClaimOutbox(ctx, 100, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		var target domain.OutboxEvent
		for _, e := range events {
			var d domain.DispatchEvent
			if err = json.Unmarshal(e.Payload, &d); err != nil {
				t.Fatal(err)
			}
			if d.JobID == job.ID {
				target = e
			}
			if err = s.AcceptDispatch(ctx, "integration", d); err != nil {
				t.Fatal(err)
			}
			if err = s.AcceptDispatch(ctx, "integration", d); err != nil {
				t.Fatal(err)
			}
			if err = s.MarkOutboxPublished(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		if target.EventID == (domain.ID{}) {
			t.Fatal("missing outbox event")
		}
		var count int
		if err = s.DB.Get(&count, "SELECT COUNT(*) FROM inbox_events WHERE consumer_name='integration' AND event_id=?", target.EventID); err != nil || count != 1 {
			t.Fatalf("inbox count=%d error=%v", count, err)
		}
		step, err := s.GetStep(ctx, target.StepID)
		if err != nil || step.Status != domain.Ready {
			t.Fatalf("step=%v error=%v", step, err)
		}
	})
	t.Run("one owner and stale execution fence", func(t *testing.T) {
		_, job := fixtureJob(t, s)
		if _, err := s.DispatchLocal(ctx, 100); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		won := make(chan *domain.Lease, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l, err := s.ClaimReadyTypes(ctx, "media", domain.NewID().String(), time.Minute, []string{"PROBE"})
				if err == nil {
					won <- l
				} else if !errors.Is(err, domain.ErrNotFound) {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		close(won)
		var old *domain.Lease
		n := 0
		for l := range won {
			old = l
			n++
		}
		if n != 1 {
			t.Fatalf("claim winners=%d", n)
		}
		if old.Job.ID != job.ID {
			t.Fatal("claimed unexpected job")
		}
		if _, err := s.DB.Exec("UPDATE job_steps SET lease_until=TIMESTAMPADD(SECOND,-1,UTC_TIMESTAMP(6)) WHERE id=?", old.Step.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.RenewLease(ctx, *old, time.Minute); !errors.Is(err, domain.ErrLeaseLost) {
			t.Fatalf("expired renewal=%v", err)
		}
		if _, err := s.RecoverExpired(ctx, 100, 3); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DispatchDue(ctx, 100); err != nil {
			t.Fatal(err)
		}
		newLease := claim(t, s, "media", "PROBE")
		if newLease.AttemptNo <= old.AttemptNo {
			t.Fatal("execution version did not increase")
		}
		if err := s.CompleteStep(ctx, *old, domain.StepResult{}); !errors.Is(err, domain.ErrLeaseLost) {
			t.Fatalf("stale result=%v", err)
		}
		complete(t, s, newLease, domain.StepResult{})
	})
	t.Run("async polling does not resubmit or consume retries", func(t *testing.T) {
		_, _ = fixtureJob(t, s)
		complete(t, s, claim(t, s, "media", "PROBE"), domain.StepResult{})
		complete(t, s, claim(t, s, "media", "EXTRACT_AUDIO"), domain.StepResult{})
		l := claim(t, s, "media", "ASR")
		ext, created, err := s.BeginExternal(ctx, *l, "fixture-provider", json.RawMessage(`{"input":"audio"}`))
		if err != nil || !created {
			t.Fatalf("begin external=%v created=%v", err, created)
		}
		if err = s.WaitExternal(ctx, *l, domain.ExternalWait{ExternalJobID: ext.ID, ProviderJobID: "provider-task", PollAfter: time.Second}); err != nil {
			t.Fatal(err)
		}
		if _, err = s.DB.Exec("UPDATE job_steps SET next_poll_at=TIMESTAMPADD(SECOND,-1,UTC_TIMESTAMP(6)) WHERE id=?", l.Step.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = s.DispatchDue(ctx, 100); err != nil {
			t.Fatal(err)
		}
		again := claim(t, s, "media", "ASR")
		existing, created, err := s.BeginExternal(ctx, *again, "fixture-provider", json.RawMessage(`{}`))
		if err != nil || created || existing.ID != ext.ID || existing.ProviderJobID != "provider-task" {
			t.Fatalf("external task changed: %+v created=%v error=%v", existing, created, err)
		}
		if again.Step.RetryCount != 0 {
			t.Fatal("normal polling consumed retries")
		}
		complete(t, s, again, domain.StepResult{})
	})
	t.Run("unknown external submission is terminal and recorded", func(t *testing.T) {
		owner, j := fixtureJob(t, s)
		complete(t, s, claim(t, s, "media", "PROBE"), domain.StepResult{})
		complete(t, s, claim(t, s, "media", "EXTRACT_AUDIO"), domain.StepResult{})
		l := claim(t, s, "media", "ASR")
		ext, _, err := s.BeginExternal(ctx, *l, "fixture-provider", json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if err = s.MarkExternalUnknown(ctx, *l); err != nil {
			t.Fatal(err)
		}
		e, _ := s.GetExternalJob(ctx, ext.ID)
		job, _ := s.GetJob(ctx, owner, j.ID)
		if e.Status != "SUBMISSION_UNKNOWN" || job.Status != domain.Failed {
			t.Fatalf("external=%s job=%s", e.Status, job.Status)
		}
	})
	t.Run("conversation version checkpoint and atomic finalization", func(t *testing.T) {
		owner, job := readyVideo(t, s)
		a, err := s.CreateAgent(ctx, owner, "test-agent", "fixture", json.RawMessage(`{"model":"fake"}`))
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
		session, err := s.CreateSession(ctx, owner, job.VideoID, versions[0].ID, domain.ID{})
		if err != nil {
			t.Fatal(err)
		}
		run, err := s.CreateRun(ctx, owner, session.ID, "what happened?", "question-1")
		if err != nil {
			t.Fatal(err)
		}
		same, err := s.CreateRun(ctx, owner, session.ID, "what happened?", "question-1")
		if err != nil || same.ID != run.ID {
			t.Fatal("run idempotency failed", err)
		}
		if _, err = s.CreateRun(ctx, owner, session.ID, "another question", "question-2"); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("parallel run not rejected", err)
		}
		l := claim(t, s, "agent", "QA")
		call, created, err := s.SaveToolIntent(ctx, *l, "call-1", "search_segments", json.RawMessage(`{"query":"test"}`), 8)
		if err != nil || !created {
			t.Fatal(err)
		}
		if err = s.SaveToolResult(ctx, *l, call.LogicalCallID, json.RawMessage(`{"evidence":["segment-1"]}`)); err != nil {
			t.Fatal(err)
		}
		if err = s.SaveCheckpoint(ctx, *l, json.RawMessage(`{"messages":[],"evidence":["segment-1"]}`)); err != nil {
			t.Fatal(err)
		}
		complete(t, s, l, domain.StepResult{Answer: json.RawMessage(`{"answer":"answer","claims":[]}`)})
		done, err := s.GetRun(ctx, owner, run.ID)
		if err != nil || done.Status != domain.Succeeded || done.BudgetUsed != 1 {
			t.Fatalf("run=%+v error=%v", done, err)
		}
		current, _ := s.GetSession(ctx, owner, session.ID)
		if current.ActiveRunID != (domain.ID{}) {
			t.Fatal("active run not cleared")
		}
		events, err := s.ListRunEvents(ctx, owner, run.ID, 0, 100)
		if err != nil || len(events) != 2 || events[len(events)-1].Type != "answer.completed" {
			t.Fatalf("events=%+v error=%v", events, err)
		}
		if err = s.AppendRunEvent(ctx, *l, "late", json.RawMessage(`{}`)); !errors.Is(err, domain.ErrLeaseLost) {
			t.Fatal("terminal run accepted stale event", err)
		}
	})
	t.Run("cancel revokes execution and protects tenant data", func(t *testing.T) {
		owner, job := fixtureJob(t, s)
		l := claim(t, s, "media", "PROBE")
		if err := s.CancelJob(ctx, domain.NewID(), job.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("cross-tenant cancel allowed", err)
		}
		if err := s.CancelJob(ctx, owner, job.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.RenewLease(ctx, *l, time.Minute); !errors.Is(err, domain.ErrLeaseLost) {
			t.Fatal("cancelled lease renewed", err)
		}
		if err := s.CompleteStep(ctx, *l, domain.StepResult{}); !errors.Is(err, domain.ErrLeaseLost) {
			t.Fatal("cancelled execution published", err)
		}
		got, _ := s.GetJob(ctx, owner, job.ID)
		if got.Status != domain.Cancelled {
			t.Fatal(got.Status)
		}
		if _, err := s.GetVideo(ctx, domain.NewID(), job.VideoID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("cross-tenant video readable", err)
		}
	})
}
