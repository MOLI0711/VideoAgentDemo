//go:build integration

package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
)

func TestMySQLPublicationBoundaries(t *testing.T) {
	s := isolatedStore(t)
	ctx := context.Background()
	t.Run("publication uses authoritative ownership and content lists exceed 200", func(t *testing.T) {
		owner, job := fixtureJob(t, s)
		l := claim(t, s, "media", "PROBE")
		l.Job.OwnerID, l.Job.VideoID, l.Job.VideoVersionID = domain.NewID(), domain.NewID(), domain.NewID()
		asset := domain.Asset{ID: domain.NewID(), ObjectKey: "tests/" + domain.NewID().String(), Kind: "FRAME"}
		result := domain.StepResult{Assets: []domain.Asset{asset}, Metadata: json.RawMessage(`{"duration_ms":12000}`)}
		for i := 0; i < 301; i++ {
			result.Frames = append(result.Frames, domain.Frame{ID: domain.NewID(), AssetID: asset.ID, TimestampMS: int64(i)})
			result.Segments = append(result.Segments, domain.Segment{ID: domain.NewID(), Ordinal: i, StartMS: int64(i), EndMS: int64(i + 1), Text: "fixture"})
		}
		complete(t, s, l, result)
		saved, err := s.GetAsset(ctx, owner, asset.ID)
		if err != nil || saved.OwnerID != owner || saved.VideoID != job.VideoID || saved.VideoVersionID != job.VideoVersionID {
			t.Fatalf("publication trusted caller scope: %+v %v", saved, err)
		}
		segments, err := s.ListSegments(ctx, owner, job.VideoVersionID, 1000)
		if err != nil || len(segments) != 301 {
			t.Fatalf("content truncated: segments=%d error=%v", len(segments), err)
		}
		frames, err := s.ListFrames(ctx, owner, job.VideoVersionID, 1000)
		if err != nil || len(frames) != 301 {
			t.Fatalf("content truncated: frames=%d error=%v", len(frames), err)
		}
		version, err := s.GetVersion(ctx, owner, job.VideoVersionID)
		if err != nil || !strings.Contains(string(version.Metadata), "12000") {
			t.Fatalf("probe metadata not persisted: %+v %v", version, err)
		}
	})
	t.Run("checkpoint survives reclaim and rejects stale writers", func(t *testing.T) {
		_, _ = fixtureJob(t, s)
		l := claim(t, s, "media", "PROBE")
		state := json.RawMessage(`{"calls_reserved":3,"map_index":2}`)
		if err := s.SaveStepCheckpoint(ctx, *l, state); err != nil {
			t.Fatal(err)
		}
		tooLarge := json.RawMessage(`"` + strings.Repeat("x", 1<<20) + `"`)
		if err := s.SaveStepCheckpoint(ctx, *l, tooLarge); !errors.Is(err, domain.ErrInvalid) {
			t.Fatal("oversized checkpoint accepted", err)
		}
		if _, err := s.DB.Exec("UPDATE job_steps SET lease_until=TIMESTAMPADD(SECOND,-1,UTC_TIMESTAMP(6)) WHERE id=?", l.Step.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecoverExpired(ctx, 100, 3); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DispatchDue(ctx, 100); err != nil {
			t.Fatal(err)
		}
		next := claim(t, s, "media", "PROBE")
		var saved struct {
			Checkpoint json.RawMessage `json:"checkpoint"`
		}
		if err := json.Unmarshal(next.Step.Result, &saved); err != nil || canonical(saved.Checkpoint) != canonical(state) {
			t.Fatalf("checkpoint lost: %s %v", next.Step.Result, err)
		}
		if err := s.SaveStepCheckpoint(ctx, *l, json.RawMessage(`{}`)); !errors.Is(err, domain.ErrLeaseLost) {
			t.Fatal("stale checkpoint accepted", err)
		}
		complete(t, s, next, domain.StepResult{Metadata: json.RawMessage(`{"done":true}`)})
		done, _ := s.GetStep(ctx, next.Step.ID)
		if strings.Contains(string(done.Result), "checkpoint") {
			t.Fatal("final publication retained intermediate checkpoint")
		}
	})
	t.Run("job deadline revokes renewals and final writes", func(t *testing.T) {
		owner, job := fixtureJob(t, s)
		l := claim(t, s, "media", "PROBE")
		if _, err := s.DB.Exec("UPDATE jobs SET deadline_at=TIMESTAMPADD(SECOND,-1,UTC_TIMESTAMP(6)) WHERE id=?", job.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.RenewLease(ctx, *l, time.Minute); !errors.Is(err, domain.ErrLeaseLost) {
			t.Fatal("past-deadline renewal accepted", err)
		}
		if err := s.CompleteStep(ctx, *l, domain.StepResult{}); !errors.Is(err, domain.ErrLeaseLost) {
			t.Fatal("past-deadline result accepted", err)
		}
		if _, err := s.ExpireDeadlines(ctx, 100); err != nil {
			t.Fatal(err)
		}
		failed, _ := s.GetJob(ctx, owner, job.ID)
		if failed.Status != domain.Failed || failed.ErrorCode != "DEADLINE_EXCEEDED" {
			t.Fatalf("deadline not terminal: %+v", failed)
		}
	})
	t.Run("older completed index cannot replace the desired version", func(t *testing.T) {
		owner, old := fixtureJob(t, s)
		for _, typ := range []string{"PROBE", "EXTRACT_AUDIO", "EXTRACT_FRAMES", "ASR", "OCR", "BUILD_SEGMENTS"} {
			complete(t, s, claim(t, s, "media", typ), domain.StepResult{})
		}
		oldIndex := claim(t, s, "agent", "BUILD_INDEX")
		newJob, err := s.Reprocess(ctx, owner, old.VideoID, domain.NewID().String(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		for _, typ := range []string{"PROBE", "EXTRACT_AUDIO", "EXTRACT_FRAMES", "ASR", "OCR", "BUILD_SEGMENTS"} {
			complete(t, s, claim(t, s, "media", typ), domain.StepResult{})
		}
		newIndex := claim(t, s, "agent", "BUILD_INDEX")
		complete(t, s, newIndex, domain.StepResult{IndexName: "index", IndexBuildID: "new-build"})
		complete(t, s, oldIndex, domain.StepResult{IndexName: "index", IndexBuildID: "old-build"})
		video, err := s.GetVideo(ctx, owner, old.VideoID)
		if err != nil || video.ActiveVersionID != newJob.VideoVersionID {
			t.Fatalf("late generation replaced active: %+v %v", video, err)
		}
		version, err := s.GetVersion(ctx, owner, old.VideoVersionID)
		if err != nil || version.Status != "READY" || version.IndexBuildID != "old-build" {
			t.Fatal("old published version not retained for pinned access", err)
		}
	})
	t.Run("failed attempt is recorded as failure", func(t *testing.T) {
		_, _ = fixtureJob(t, s)
		l := claim(t, s, "media", "PROBE")
		if err := s.FailStep(ctx, *l, "TEST_FAILURE", "fixture"); err != nil {
			t.Fatal(err)
		}
		var status string
		if err := s.DB.Get(&status, "SELECT status FROM job_attempts WHERE step_id=? AND attempt_no=?", l.Step.ID, l.AttemptNo); err != nil || status != domain.Failed {
			t.Fatalf("attempt failure misrecorded as %q: %v", status, err)
		}
	})
}
