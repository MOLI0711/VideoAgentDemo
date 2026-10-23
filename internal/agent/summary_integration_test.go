//go:build integration

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/MOLI0711/MOLIVedio/internal/provider"
	db "github.com/MOLI0711/MOLIVedio/internal/repository/mysql"
	"github.com/MOLI0711/MOLIVedio/internal/workflow"
	driver "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

func summaryTestStore(t *testing.T) *db.Store {
	t.Helper()
	dsn := os.Getenv("VA_MYSQL_TEST_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set VA_MYSQL_TEST_ADMIN_DSN to run real MySQL summary recovery tests")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid test admin DSN")
	}
	cfg.DBName, cfg.ParseTime = "", true
	admin, err := sqlx.Connect("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	name := "molivideo_summary_test_" + strings.ReplaceAll(domain.NewID().String(), "-", "")
	if _, err = admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg.DBName = name
	store, err := db.Open(context.Background(), cfg.FormatDSN())
	if err != nil {
		admin.Exec("DROP DATABASE `" + name + "`")
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.Close()
		if _, e := admin.Exec("DROP DATABASE `" + name + "`"); e != nil {
			t.Error(e)
		}
		admin.Close()
	})
	if err = store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var version string
	if err = store.DB.Get(&version, "SELECT VERSION()"); err != nil {
		t.Fatal(err)
	}
	t.Logf("real MySQL version: %s; model responses are explicit test fixtures", version)
	return store
}

func summaryClaim(t *testing.T, s *db.Store, kind, step string) *domain.Lease {
	t.Helper()
	if _, err := s.DispatchLocal(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	l, err := s.ClaimReadyTypes(context.Background(), kind, domain.NewID().String(), time.Minute, []string{step})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func summaryFixture(t *testing.T, s *db.Store, segmentCount int) *domain.Lease {
	t.Helper()
	ctx := context.Background()
	user, err := s.SeedUser(ctx, "summary-"+domain.NewID().String(), "test-only-hash")
	if err != nil {
		t.Fatal(err)
	}
	upload, err := s.CreateUpload(ctx, user.ID, domain.CreateUpload{Filename: "fixture.mp4", ContentType: "video/mp4", Title: "Summary fixture", Size: 1, ObjectKey: "fixtures/" + domain.NewID().String(), MultipartID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	preprocess, err := s.CompleteUpload(ctx, user.ID, upload.ID, domain.Asset{ObjectKey: upload.ObjectKey, Size: 1}, json.RawMessage(`{"asr_mode":"mock"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"PROBE", "EXTRACT_AUDIO", "EXTRACT_FRAMES", "ASR", "OCR", "BUILD_SEGMENTS", "BUILD_INDEX"} {
		kind := "media"
		result := domain.StepResult{}
		if typ == "BUILD_SEGMENTS" {
			for i := 0; i < segmentCount; i++ {
				result.Segments = append(result.Segments, domain.Segment{ID: domain.NewID(), Ordinal: i, StartMS: int64(i * 1000), EndMS: int64((i + 1) * 1000), Text: strings.Repeat("fixture ", 250)})
			}
		}
		if typ == "BUILD_INDEX" {
			kind = "agent"
			result.IndexName, result.IndexBuildID = "fixture-index", "fixture-build"
		}
		if err = s.CompleteStep(ctx, *summaryClaim(t, s, kind, typ), result); err != nil {
			t.Fatal(err)
		}
	}
	agent, err := s.CreateAgent(ctx, user.ID, "fixture", "", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	versions, err := s.ListAgentVersions(ctx, user.ID, agent.ID)
	if err != nil || len(versions) != 1 {
		t.Fatal("missing agent version", err)
	}
	if err = s.PublishAgentVersion(ctx, user.ID, versions[0].ID); err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateSummaryPinned(ctx, user.ID, upload.VideoID, preprocess.VideoVersionID, versions[0].ID, domain.NewID().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.CancelJob(context.Background(), user.ID, job.ID) })
	return summaryClaim(t, s, "agent", "SUMMARY")
}

func summaryReload(t *testing.T, s *db.Store, l *domain.Lease) *domain.Lease {
	t.Helper()
	if _, err := s.DB.Exec("UPDATE job_steps SET lease_until=TIMESTAMPADD(SECOND,-1,UTC_TIMESTAMP(6)) WHERE id=?", l.Step.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverExpired(context.Background(), 100, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DispatchDue(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	next := summaryClaim(t, s, "agent", "SUMMARY")
	if next.Step.ID != l.Step.ID || next.AttemptNo <= l.AttemptNo {
		t.Fatal("recovery did not reclaim the same step with a new fence")
	}
	return next
}

func summarySavedState(t *testing.T, s *db.Store, l *domain.Lease) summaryState {
	t.Helper()
	step, err := s.GetStep(context.Background(), l.Step.ID)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Checkpoint summaryState `json:"checkpoint"`
	}
	if err = json.Unmarshal(step.Result, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Checkpoint
}

type summaryFixtureModel struct {
	t      *testing.T
	store  *db.Store
	lease  *domain.Lease
	calls  []string
	failAt int
}

func (m *summaryFixtureModel) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	var items []struct {
		ID          string   `json:"id"`
		EvidenceIDs []string `json:"evidence_ids"`
	}
	if len(req.Messages) != 2 || json.Unmarshal([]byte(req.Messages[1].Content), &items) != nil || len(items) == 0 {
		m.t.Fatal("unexpected summary model input")
	}
	id, phase := items[0].ID, "map:"
	if id == "" && len(items[0].EvidenceIDs) > 0 {
		id, phase = items[0].EvidenceIDs[0], "reduce:"
	}
	m.calls = append(m.calls, phase+id)
	state := summarySavedState(m.t, m.store, m.lease)
	if state.Calls == 0 || len(state.Usage) == 0 || state.Usage[len(state.Usage)-1].Known {
		m.t.Fatal("network call preceded durable unknown-usage reservation")
	}
	if len(m.calls) == m.failAt {
		return provider.ChatResponse{}, errors.New("fixture: process stops after request dispatch")
	}
	claims := []Claim{}
	for i := 0; i < 3; i++ {
		claims = append(claims, Claim{Text: fmt.Sprintf("Synthetic claim %d", i), EvidenceIDs: []string{id}})
	}
	return provider.ChatResponse{Content: string(bytesOf(map[string]any{"status": "answered", "claims": claims, "limitations": []string{}})), Usage: provider.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15, Known: true, Mock: true}, Mock: true}, nil
}
func (*summaryFixtureModel) Embed(context.Context, []string) (provider.EmbeddingResponse, error) {
	return provider.EmbeddingResponse{}, errors.New("summary must not call embeddings")
}
func (*summaryFixtureModel) Vision(context.Context, provider.VisionRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, errors.New("summary must not call vision")
}

func TestSummaryMySQLRecovery(t *testing.T) {
	s := summaryTestStore(t)
	ctx := context.Background()
	t.Run("completed map groups and final result survive restart", func(t *testing.T) {
		l := summaryFixture(t, s, 16) // Seven bounded segments per group: three groups.
		model := &summaryFixtureModel{t: t, store: s, lease: l, failAt: 2}
		worker := Worker{Store: s, Model: model}
		if _, err := worker.summary(ctx, l); err == nil {
			t.Fatal("expected simulated process interruption")
		}
		state := summarySavedState(t, s, l)
		if state.MapIndex != 1 || state.Calls != 2 || len(state.Usage) != 2 || !state.Usage[0].Known || state.Usage[1].Known {
			t.Fatalf("interrupted state incorrect: %+v", state)
		}
		l = summaryReload(t, s, l)
		model.lease = l
		result, err := worker.summary(ctx, l)
		if err != nil {
			t.Fatal(err)
		}
		if len(model.calls) != 4 || model.calls[1] != model.calls[2] || model.calls[0] == model.calls[2] {
			t.Fatalf("completed group replayed: %v", model.calls)
		}
		state = summarySavedState(t, s, l)
		if state.Calls != 4 || state.MapIndex != 3 || state.Final == nil || state.Usage[1].Known {
			t.Fatalf("recovered budget/result incorrect: %+v", state)
		}
		l = summaryReload(t, s, l) // Crash after final checkpoint but before publication.
		model.lease = l
		again, err := worker.summary(ctx, l)
		if err != nil || len(model.calls) != 4 || string(again.Answer) != string(result.Answer) {
			t.Fatal("final checkpoint repeated model work", err)
		}
		if err = s.CompleteStep(ctx, *l, again); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("completed reduce chunks survive restart", func(t *testing.T) {
		l := summaryFixture(t, s, 35) // Five maps yield 15 claims, then two reductions.
		model := &summaryFixtureModel{t: t, store: s, lease: l, failAt: 7}
		worker := Worker{Store: s, Model: model}
		if _, err := worker.summary(ctx, l); err == nil {
			t.Fatal("expected reduce interruption")
		}
		state := summarySavedState(t, s, l)
		if state.Phase != "reduce" || state.ReduceOffset != 12 || len(state.ReduceNext) != 3 || state.Calls != 7 {
			t.Fatalf("reduce checkpoint incorrect: %+v", state)
		}
		l = summaryReload(t, s, l)
		model.lease = l
		result, err := worker.summary(ctx, l)
		if err != nil || len(model.calls) != 8 || model.calls[6] != model.calls[7] {
			t.Fatalf("reduce work repeated: %v error=%v", model.calls, err)
		}
		var answer Result
		if json.Unmarshal(result.Answer, &answer) != nil || len(answer.Claims) != 6 || !answer.Mock {
			t.Fatalf("bad reduce result: %s", result.Answer)
		}
	})
	t.Run("unknown dispatch consumes cross-attempt budget", func(t *testing.T) {
		l := summaryFixture(t, s, 1)
		state := summaryState{Phase: "map", Calls: maxSummaryCalls - 1, Usage: make([]provider.Usage, maxSummaryCalls-1)}
		if err := s.SaveStepCheckpoint(ctx, *l, bytesOf(state)); err != nil {
			t.Fatal(err)
		}
		step, _ := s.GetStep(ctx, l.Step.ID)
		l.Step = *step
		model := &summaryFixtureModel{t: t, store: s, lease: l, failAt: 1}
		worker := Worker{Store: s, Model: model}
		if _, err := worker.summary(ctx, l); err == nil {
			t.Fatal("expected network outcome uncertainty")
		}
		l = summaryReload(t, s, l)
		model.lease = l
		_, err := worker.summary(ctx, l)
		var execution *workflow.ExecutionError
		if !errors.As(err, &execution) || execution.Code != "SUMMARY_BUDGET_EXCEEDED" || len(model.calls) != 1 {
			t.Fatalf("restart reset request budget: calls=%d error=%v", len(model.calls), err)
		}
	})
}
