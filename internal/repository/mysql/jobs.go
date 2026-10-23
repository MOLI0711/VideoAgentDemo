package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/jmoiron/sqlx"
)

type stepDef struct {
	Type, Kind string
	Depends    []string
}

var preprocessPlan = []stepDef{
	{"PROBE", "media", nil},
	{"EXTRACT_AUDIO", "media", []string{"PROBE"}},
	{"EXTRACT_FRAMES", "media", []string{"PROBE"}},
	{"ASR", "media", []string{"EXTRACT_AUDIO"}},
	{"OCR", "media", []string{"EXTRACT_FRAMES"}},
	{"BUILD_SEGMENTS", "media", []string{"ASR", "OCR"}},
	{"BUILD_INDEX", "agent", []string{"BUILD_SEGMENTS"}},
}

func plan(kind string) []stepDef {
	if kind == "PREPROCESS" {
		return preprocessPlan
	}
	return []stepDef{{kind, "agent", nil}}
}

func createJobTx(ctx context.Context, t *sqlx.Tx, owner, video, version domain.ID, kind string, config json.RawMessage) (*domain.Job, error) {
	id := domain.NewID()
	deadline := 14400
	if kind == "QA" {
		deadline = 300
	}
	if kind == "SUMMARY" {
		deadline = 1800
	}
	_, err := t.ExecContext(ctx, "INSERT INTO jobs(id,owner_id,video_id,video_version_id,type,status,config,deadline_at) VALUES(?,?,?,?,?,'QUEUED',?,TIMESTAMPADD(SECOND,?,UTC_TIMESTAMP(6)))", id, owner, video, version, kind, js(config), deadline)
	if err != nil {
		return nil, err
	}
	for _, d := range plan(kind) {
		step := domain.Step{ID: domain.NewID(), JobID: id, StepKey: d.Type, StepType: d.Type, WorkerKind: d.Kind, Status: domain.Blocked}
		_, err = t.ExecContext(ctx, "INSERT INTO job_steps(id,job_id,step_key,step_type,worker_kind,status,result) VALUES(?,?,?,?,?,'BLOCKED','{}')", step.ID, id, d.Type, d.Type, d.Kind)
		if err != nil {
			return nil, err
		}
		if len(d.Depends) == 0 {
			if err = enqueueTx(ctx, t, &step); err != nil {
				return nil, err
			}
		}
	}
	var out domain.Job
	err = t.GetContext(ctx, &out, "SELECT * FROM jobs WHERE id=?", id)
	return &out, err
}

func enqueueTx(ctx context.Context, t *sqlx.Tx, step *domain.Step) error {
	step.DispatchSeq++
	event := domain.DispatchEvent{SchemaVersion: 1, EventID: domain.NewID(), JobID: step.JobID, StepID: step.ID, DispatchSeq: step.DispatchSeq, WorkerKind: step.WorkerKind}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err = t.ExecContext(ctx, "UPDATE job_steps SET status='QUEUED',dispatch_seq=?,lease_owner='',lease_until=NULL,next_poll_at=NULL,next_run_at=UTC_TIMESTAMP(6) WHERE id=?", step.DispatchSeq, step.ID); err != nil {
		return err
	}
	_, err = t.ExecContext(ctx, "INSERT INTO outbox_events(event_id,step_id,dispatch_seq,payload) VALUES(?,?,?,?)", event.EventID, step.ID, step.DispatchSeq, payload)
	return err
}

func (s *Store) GetJob(ctx context.Context, owner, id domain.ID) (*domain.Job, error) {
	var out domain.Job
	err := s.DB.GetContext(ctx, &out, "SELECT * FROM jobs WHERE id=? AND owner_id=?", id, owner)
	if err != nil {
		return nil, mapped(err)
	}
	err = s.DB.SelectContext(ctx, &out.Steps, "SELECT * FROM job_steps WHERE job_id=? ORDER BY created_at,id", id)
	return &out, err
}
func (s *Store) ListJobs(ctx context.Context, owner, video domain.ID, limit int) ([]domain.Job, error) {
	out := []domain.Job{}
	q := "SELECT * FROM jobs WHERE owner_id=?"
	args := []any{owner}
	if video != (domain.ID{}) {
		q += " AND video_id=?"
		args = append(args, video)
	}
	q += " ORDER BY created_at DESC,id DESC LIMIT ?"
	args = append(args, bounded(limit))
	err := s.DB.SelectContext(ctx, &out, q, args...)
	return out, err
}

// lockJob always takes parent rows before job/step rows. Read-only discovery does
// not establish a read view because transactions use READ COMMITTED.
func lockJob(ctx context.Context, t *sqlx.Tx, id domain.ID) (*domain.Job, *domain.Run, error) {
	var j domain.Job
	if err := t.GetContext(ctx, &j, "SELECT * FROM jobs WHERE id=?", id); err != nil {
		return nil, nil, err
	}
	var video domain.ID
	if err := t.GetContext(ctx, &video, "SELECT id FROM videos WHERE id=? FOR UPDATE", j.VideoID); err != nil {
		return nil, nil, err
	}
	var r domain.Run
	err := t.GetContext(ctx, &r, "SELECT * FROM agent_runs WHERE job_id=?", id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	if err == nil {
		var session domain.ID
		if err = t.GetContext(ctx, &session, "SELECT id FROM sessions WHERE id=? FOR UPDATE", r.SessionID); err != nil {
			return nil, nil, err
		}
	}
	if err = t.GetContext(ctx, &j, "SELECT * FROM jobs WHERE id=? FOR UPDATE", id); err != nil {
		return nil, nil, err
	}
	if r.ID == (domain.ID{}) {
		return &j, nil, nil
	}
	return &j, &r, nil
}

func lockLease(ctx context.Context, t *sqlx.Tx, l domain.Lease) (*domain.Job, *domain.Step, *domain.Run, error) {
	j, r, err := lockJob(ctx, t, l.Job.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	if !active(j.Status) {
		return nil, nil, nil, domain.ErrLeaseLost
	}
	var deleted bool
	if err = t.GetContext(ctx, &deleted, "SELECT deleted_at IS NOT NULL FROM videos WHERE id=?", j.VideoID); err != nil {
		return nil, nil, nil, err
	}
	if deleted {
		return nil, nil, nil, domain.ErrLeaseLost
	}
	var step domain.Step
	if err = t.GetContext(ctx, &step, "SELECT * FROM job_steps WHERE id=? AND job_id=? FOR UPDATE", l.Step.ID, j.ID); err != nil {
		return nil, nil, nil, err
	}
	var now time.Time
	if err = t.GetContext(ctx, &now, "SELECT UTC_TIMESTAMP(6)"); err != nil {
		return nil, nil, nil, err
	}
	if step.Status != domain.Running || step.AttemptNo != l.AttemptNo || step.LeaseOwner != l.WorkerID || step.LeaseUntil == nil || !step.LeaseUntil.After(now) || !j.DeadlineAt.After(now) {
		return nil, nil, nil, domain.ErrLeaseLost
	}
	return j, &step, r, nil
}

func (s *Store) ClaimReady(ctx context.Context, kind, worker string, ttl time.Duration) (*domain.Lease, error) {
	return s.ClaimReadyTypes(ctx, kind, worker, ttl, nil)
}

func (s *Store) ClaimReadyTypes(ctx context.Context, kind, worker string, ttl time.Duration, types []string) (*domain.Lease, error) {
	if worker == "" || ttl < time.Second {
		return nil, domain.ErrInvalid
	}
	candidates := []domain.Step{}
	query := "SELECT * FROM job_steps WHERE worker_kind=? AND status='READY' AND next_run_at<=UTC_TIMESTAMP(6)"
	args := []any{kind}
	if len(types) > 0 {
		query += " AND step_type IN (?)"
		args = append(args, types)
	}
	query += " ORDER BY next_run_at,id LIMIT 16"
	query, args, expandErr := sqlx.In(query, args...)
	if expandErr != nil {
		return nil, expandErr
	}
	if err := s.DB.SelectContext(ctx, &candidates, query, args...); err != nil {
		return nil, err
	}
	for _, c := range candidates {
		var out domain.Lease
		err := s.tx(ctx, func(t *sqlx.Tx) error {
			var j domain.Job
			err := t.GetContext(ctx, &j, "SELECT * FROM jobs WHERE id=? FOR UPDATE SKIP LOCKED", c.JobID)
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrConflict
			}
			if err != nil {
				return err
			}
			if !active(j.Status) {
				return domain.ErrConflict
			}
			var step domain.Step
			if err = t.GetContext(ctx, &step, "SELECT * FROM job_steps WHERE id=? FOR UPDATE", c.ID); err != nil {
				return err
			}
			if step.Status != domain.Ready {
				return domain.ErrConflict
			}
			var now time.Time
			if err = t.GetContext(ctx, &now, "SELECT UTC_TIMESTAMP(6)"); err != nil {
				return err
			}
			if !j.DeadlineAt.After(now) {
				return domain.ErrConflict
			}
			step.AttemptNo++
			until := now.Add(ttl)
			if until.After(j.DeadlineAt) {
				until = j.DeadlineAt
			}
			step.Status = domain.Running
			step.LeaseOwner = worker
			step.LeaseUntil = &until
			if _, err = t.ExecContext(ctx, "UPDATE job_steps SET status='RUNNING',attempt_no=?,lease_owner=?,lease_until=? WHERE id=?", step.AttemptNo, worker, until, step.ID); err != nil {
				return err
			}
			if _, err = t.ExecContext(ctx, "INSERT INTO job_attempts(id,step_id,attempt_no,worker_id,status) VALUES(?,?,?,?,'RUNNING')", domain.NewID(), step.ID, step.AttemptNo, worker); err != nil {
				return err
			}
			if _, err = t.ExecContext(ctx, "UPDATE jobs SET status='RUNNING',updated_at=UTC_TIMESTAMP(6) WHERE id=?", j.ID); err != nil {
				return err
			}
			j.Status = domain.Running
			out = domain.Lease{Job: j, Step: step, WorkerID: worker, AttemptNo: step.AttemptNo, ExpiresAt: until}
			return nil
		})
		if errors.Is(err, domain.ErrConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &out, nil
	}
	return nil, domain.ErrNotFound
}

func (s *Store) RenewLease(ctx context.Context, l domain.Lease, ttl time.Duration) error {
	if ttl < time.Second {
		return domain.ErrInvalid
	}
	return s.tx(ctx, func(t *sqlx.Tx) error {
		j, step, _, err := lockLease(ctx, t, l)
		if err != nil {
			return err
		}
		_, err = t.ExecContext(ctx, "UPDATE job_steps SET lease_until=LEAST(TIMESTAMPADD(MICROSECOND,?,UTC_TIMESTAMP(6)),?) WHERE id=?", ttl.Microseconds(), j.DeadlineAt, step.ID)
		return err
	})
}

// SaveStepCheckpoint persists executor progress before a paid/external action.
// The current lease is checked in the same transaction; retries retain result.
func (s *Store) SaveStepCheckpoint(ctx context.Context, l domain.Lease, state json.RawMessage) error {
	if !json.Valid(state) || len(state) > 1<<20 {
		return domain.ErrInvalid
	}
	return s.tx(ctx, func(t *sqlx.Tx) error {
		_, step, _, err := lockLease(ctx, t, l)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]json.RawMessage{"checkpoint": state})
		if err != nil {
			return err
		}
		_, err = t.ExecContext(ctx, "UPDATE job_steps SET result=? WHERE id=?", payload, step.ID)
		return err
	})
}

func (s *Store) CompleteStep(ctx context.Context, l domain.Lease, result domain.StepResult) error {
	if result.ExternalWait != nil {
		return s.WaitExternal(ctx, l, *result.ExternalWait)
	}
	return s.tx(ctx, func(t *sqlx.Tx) error {
		j, step, run, err := lockLease(ctx, t, l)
		if err != nil {
			return err
		}
		// Never derive persisted ownership/version from a caller-modifiable lease
		// snapshot. Only the locked database rows define the publication scope.
		l.Job, l.Step = *j, *step
		if run != nil && (len(result.Answer) == 0 || !json.Valid(result.Answer)) {
			return fmt.Errorf("%w: QA requires a final answer", domain.ErrInvalid)
		}
		status := domain.Succeeded
		if result.Skipped {
			if step.StepType != "OCR" {
				return fmt.Errorf("%w: only OCR may degrade", domain.ErrInvalid)
			}
			status = domain.Skipped
		}
		if err = saveResultTx(ctx, t, l, result); err != nil {
			return err
		}
		storedResult := result.Metadata
		if len(result.Answer) > 0 {
			storedResult, err = json.Marshal(map[string]json.RawMessage{"answer": result.Answer, "metadata": json.RawMessage(js(result.Metadata))})
			if err != nil {
				return err
			}
		}
		if _, err = t.ExecContext(ctx, "UPDATE job_steps SET status=?,result=?,lease_owner='',lease_until=NULL,error_code='' WHERE id=?", status, js(storedResult), step.ID); err != nil {
			return err
		}
		if _, err = t.ExecContext(ctx, "UPDATE job_attempts SET status='SUCCEEDED',ended_at=UTC_TIMESTAMP(6) WHERE step_id=? AND attempt_no=?", step.ID, l.AttemptNo); err != nil {
			return err
		}
		if step.CurrentExternalJobID != (domain.ID{}) {
			if _, err = t.ExecContext(ctx, "UPDATE external_jobs SET status='SUCCEEDED' WHERE id=?", step.CurrentExternalJobID); err != nil {
				return err
			}
		}
		if run != nil && len(result.Answer) > 0 {
			if err = finishRunTx(ctx, t, run, l.AttemptNo, domain.Succeeded, result.Answer); err != nil {
				return err
			}
		}
		if step.StepType == "BUILD_INDEX" {
			if result.IndexName == "" || result.IndexBuildID == "" {
				return fmt.Errorf("%w: missing index build", domain.ErrInvalid)
			}
			if _, err = t.ExecContext(ctx, "UPDATE video_versions SET status='READY',index_name=?,index_build_id=? WHERE id=?", result.IndexName, result.IndexBuildID, j.VideoVersionID); err != nil {
				return err
			}
			if _, err = t.ExecContext(ctx, "UPDATE videos v JOIN video_versions vv ON vv.id=? SET v.active_version_id=vv.id,v.status='READY',v.updated_at=UTC_TIMESTAMP(6) WHERE v.id=vv.video_id AND v.desired_generation=vv.generation AND v.deleted_at IS NULL", j.VideoVersionID); err != nil {
				return err
			}
		}
		if result.DurationMS > 0 {
			if _, err = t.ExecContext(ctx, "UPDATE videos SET duration_ms=? WHERE id=?", result.DurationMS, j.VideoID); err != nil {
				return err
			}
		}
		if step.StepType == "PROBE" {
			if _, err = t.ExecContext(ctx, "UPDATE video_versions SET metadata=? WHERE id=?", js(result.Metadata), j.VideoVersionID); err != nil {
				return err
			}
		}
		return advanceTx(ctx, t, j)
	})
}

func saveResultTx(ctx context.Context, t *sqlx.Tx, l domain.Lease, result domain.StepResult) error {
	for _, a := range result.Assets {
		a.OwnerID = l.Job.OwnerID
		a.VideoID = l.Job.VideoID
		a.VideoVersionID = l.Job.VideoVersionID
		a.StepID = l.Step.ID
		a.AttemptNo = l.AttemptNo
		if err := insertAsset(ctx, t, a); err != nil {
			return err
		}
	}
	for _, v := range result.Sentences {
		if v.ID == (domain.ID{}) {
			v.ID = domain.NewID()
		}
		if v.StartMS < 0 || v.EndMS < v.StartMS {
			return domain.ErrInvalid
		}
		if _, err := t.ExecContext(ctx, "INSERT INTO transcript_sentences(id,video_version_id,start_ms,end_ms,text) VALUES(?,?,?,?,?)", v.ID, l.Job.VideoVersionID, v.StartMS, v.EndMS, v.Text); err != nil {
			return err
		}
	}
	for _, f := range result.Frames {
		if f.ID == (domain.ID{}) {
			f.ID = domain.NewID()
		}
		if f.TimestampMS < 0 {
			return domain.ErrInvalid
		}
		if f.OCRStatus == "" {
			f.OCRStatus = "PENDING"
		}
		var existing domain.Frame
		err := t.GetContext(ctx, &existing, "SELECT * FROM video_frames WHERE id=?", f.ID)
		if err == nil {
			if existing.VideoVersionID != l.Job.VideoVersionID {
				return domain.ErrConflict
			}
			if _, err = t.ExecContext(ctx, "UPDATE video_frames SET ocr_text=?,ocr_status=? WHERE id=?", f.OCRText, f.OCRStatus, f.ID); err != nil {
				return err
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err = t.ExecContext(ctx, "INSERT INTO video_frames(id,video_version_id,asset_id,timestamp_ms,ocr_text,ocr_status) VALUES(?,?,?,?,?,?)", f.ID, l.Job.VideoVersionID, f.AssetID, f.TimestampMS, f.OCRText, f.OCRStatus); err != nil {
			return err
		}
	}
	for _, v := range result.Segments {
		if v.ID == (domain.ID{}) {
			v.ID = domain.NewID()
		}
		if v.StartMS < 0 || v.EndMS < v.StartMS {
			return domain.ErrInvalid
		}
		if _, err := t.ExecContext(ctx, "INSERT INTO video_segments(id,video_version_id,ordinal,start_ms,end_ms,text,ocr_text,sentence_ids,frame_ids) VALUES(?,?,?,?,?,?,?,?,?)", v.ID, l.Job.VideoVersionID, v.Ordinal, v.StartMS, v.EndMS, v.Text, v.OCRText, array(v.SentenceIDs), array(v.FrameIDs)); err != nil {
			return err
		}
	}
	return nil
}

func advanceTx(ctx context.Context, t *sqlx.Tx, j *domain.Job) error {
	steps := []domain.Step{}
	if err := t.SelectContext(ctx, &steps, "SELECT * FROM job_steps WHERE job_id=? ORDER BY id FOR UPDATE", j.ID); err != nil {
		return err
	}
	byType := map[string]domain.Step{}
	all := true
	for _, s := range steps {
		byType[s.StepType] = s
		if s.Status != domain.Succeeded && s.Status != domain.Skipped {
			all = false
		}
	}
	if all {
		_, err := t.ExecContext(ctx, "UPDATE jobs SET status='SUCCEEDED',updated_at=UTC_TIMESTAMP(6) WHERE id=? AND status IN ('QUEUED','RUNNING')", j.ID)
		return err
	}
	for _, d := range plan(j.Type) {
		step, ok := byType[d.Type]
		if !ok || step.Status != domain.Blocked {
			continue
		}
		ready := true
		for _, dep := range d.Depends {
			v, exists := byType[dep]
			if !exists || (v.Status != domain.Succeeded && v.Status != domain.Skipped) {
				ready = false
				break
			}
		}
		if ready {
			if err := enqueueTx(ctx, t, &step); err != nil {
				return err
			}
		}
	}
	return nil
}

func failJobTx(ctx context.Context, t *sqlx.Tx, j *domain.Job, run *domain.Run, status, code, message string) error {
	if domain.Terminal(j.Status) {
		return nil
	}
	if _, err := t.ExecContext(ctx, "UPDATE job_attempts a JOIN job_steps s ON s.id=a.step_id SET a.status='CANCELLED',a.ended_at=UTC_TIMESTAMP(6),a.error_code=? WHERE s.job_id=? AND a.status='RUNNING'", code, j.ID); err != nil {
		return err
	}
	if _, err := t.ExecContext(ctx, "UPDATE job_steps SET attempt_no=attempt_no+IF(status='RUNNING',1,0),status='CANCELLED',lease_until=NULL,lease_owner='',error_code=? WHERE job_id=? AND status NOT IN ('SUCCEEDED','SKIPPED','FAILED','CANCELLED')", code, j.ID); err != nil {
		return err
	}
	if _, err := t.ExecContext(ctx, "UPDATE jobs SET status=?,error_code=?,error_message=?,updated_at=UTC_TIMESTAMP(6) WHERE id=?", status, code, message, j.ID); err != nil {
		return err
	}
	if j.Type == "PREPROCESS" {
		if _, err := t.ExecContext(ctx, "UPDATE video_versions SET status=? WHERE id=? AND status<>'READY'", status, j.VideoVersionID); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, "UPDATE videos v JOIN video_versions vv ON vv.id=? SET v.status=IF(v.active_version_id='00000000-0000-0000-0000-000000000000',?,'READY'),v.updated_at=UTC_TIMESTAMP(6) WHERE v.id=vv.video_id AND v.desired_generation=vv.generation", j.VideoVersionID, status); err != nil {
			return err
		}
	}
	if run != nil {
		return finishRunTx(ctx, t, run, 0, status, nil)
	}
	return nil
}

func (s *Store) CancelJob(ctx context.Context, owner, id domain.ID) error {
	return s.tx(ctx, func(t *sqlx.Tx) error {
		j, r, err := lockJob(ctx, t, id)
		if err != nil {
			return err
		}
		if j.OwnerID != owner {
			return domain.ErrNotFound
		}
		return failJobTx(ctx, t, j, r, domain.Cancelled, "CANCELLED", "cancelled by user")
	})
}

func (s *Store) RetryStep(ctx context.Context, l domain.Lease, code, message string, delay time.Duration, maxRetries int) error {
	return s.tx(ctx, func(t *sqlx.Tx) error {
		j, step, run, err := lockLease(ctx, t, l)
		if err != nil {
			return err
		}
		if step.RetryCount >= maxRetries {
			if _, err = t.ExecContext(ctx, "UPDATE job_attempts SET status='FAILED',error_code=?,ended_at=UTC_TIMESTAMP(6) WHERE step_id=? AND attempt_no=?", code, step.ID, l.AttemptNo); err != nil {
				return err
			}
			if _, err = t.ExecContext(ctx, "UPDATE job_steps SET status='FAILED',error_code=?,lease_owner='',lease_until=NULL WHERE id=?", code, step.ID); err != nil {
				return err
			}
			return failJobTx(ctx, t, j, run, domain.Failed, code, message)
		}
		if _, err = t.ExecContext(ctx, "UPDATE job_attempts SET status='FAILED',error_code=?,ended_at=UTC_TIMESTAMP(6) WHERE step_id=? AND attempt_no=?", code, step.ID, l.AttemptNo); err != nil {
			return err
		}
		_, err = t.ExecContext(ctx, "UPDATE job_steps SET status='RETRY_WAIT',retry_count=retry_count+1,error_code=?,lease_owner='',lease_until=NULL,next_run_at=TIMESTAMPADD(MICROSECOND,?,UTC_TIMESTAMP(6)) WHERE id=?", code, delay.Microseconds(), step.ID)
		return err
	})
}

func (s *Store) FailStep(ctx context.Context, l domain.Lease, code, message string) error {
	return s.RetryStep(ctx, l, code, message, 0, 0)
}

func (s *Store) DispatchDue(ctx context.Context, limit int) (int, error) {
	steps := []domain.Step{}
	err := s.DB.SelectContext(ctx, &steps, "SELECT * FROM job_steps WHERE (status='RETRY_WAIT' AND next_run_at<=UTC_TIMESTAMP(6)) OR (status='WAITING_EXTERNAL' AND next_poll_at<=UTC_TIMESTAMP(6)) ORDER BY id LIMIT ?", bounded(limit))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, candidate := range steps {
		err = s.tx(ctx, func(t *sqlx.Tx) error {
			j, run, err := lockJob(ctx, t, candidate.JobID)
			if err != nil {
				return err
			}
			if !active(j.Status) {
				return nil
			}
			var step domain.Step
			if err = t.GetContext(ctx, &step, "SELECT * FROM job_steps WHERE id=? FOR UPDATE", candidate.ID); err != nil {
				return err
			}
			var now time.Time
			if err = t.GetContext(ctx, &now, "SELECT UTC_TIMESTAMP(6)"); err != nil {
				return err
			}
			if !j.DeadlineAt.After(now) {
				return failJobTx(ctx, t, j, run, domain.Failed, "DEADLINE_EXCEEDED", "job deadline exceeded")
			}
			if step.Status == domain.RetryWait && step.NextRunAt.After(now) {
				return nil
			}
			if step.Status == domain.WaitingExternal && (step.NextPollAt == nil || step.NextPollAt.After(now)) {
				return nil
			}
			if step.Status != domain.RetryWait && step.Status != domain.WaitingExternal {
				return nil
			}
			if step.CurrentExternalJobID != (domain.ID{}) {
				var ext domain.ExternalJob
				if err = t.GetContext(ctx, &ext, "SELECT * FROM external_jobs WHERE id=?", step.CurrentExternalJobID); err != nil {
					return err
				}
				if !ext.DeadlineAt.After(now) {
					return failJobTx(ctx, t, j, run, domain.Failed, "ASR_DEADLINE_EXCEEDED", "external task deadline exceeded")
				}
			}
			if err = enqueueTx(ctx, t, &step); err == nil {
				n++
			}
			return err
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (s *Store) RecoverExpired(ctx context.Context, limit, maxRetries int) (int, error) {
	rows := []domain.Step{}
	if err := s.DB.SelectContext(ctx, &rows, "SELECT * FROM job_steps WHERE status='RUNNING' AND lease_until<=UTC_TIMESTAMP(6) ORDER BY lease_until,id LIMIT ?", bounded(limit)); err != nil {
		return 0, err
	}
	n := 0
	for _, c := range rows {
		err := s.tx(ctx, func(t *sqlx.Tx) error {
			j, r, err := lockJob(ctx, t, c.JobID)
			if err != nil {
				return err
			}
			if !active(j.Status) {
				return nil
			}
			var step domain.Step
			if err = t.GetContext(ctx, &step, "SELECT * FROM job_steps WHERE id=? FOR UPDATE", c.ID); err != nil {
				return err
			}
			var now time.Time
			if err = t.GetContext(ctx, &now, "SELECT UTC_TIMESTAMP(6)"); err != nil {
				return err
			}
			if step.Status != domain.Running || step.LeaseUntil == nil || step.LeaseUntil.After(now) {
				return nil
			}
			if step.RetryCount >= maxRetries || !j.DeadlineAt.After(now) {
				return failJobTx(ctx, t, j, r, domain.Failed, "LEASE_EXPIRED", "worker lease expired")
			}
			if _, err = t.ExecContext(ctx, "UPDATE job_attempts SET status='EXPIRED',ended_at=UTC_TIMESTAMP(6),error_code='LEASE_EXPIRED' WHERE step_id=? AND attempt_no=?", step.ID, step.AttemptNo); err != nil {
				return err
			}
			_, err = t.ExecContext(ctx, "UPDATE job_steps SET status='RETRY_WAIT',attempt_no=attempt_no+1,retry_count=retry_count+1,lease_owner='',lease_until=NULL,next_run_at=UTC_TIMESTAMP(6),error_code='LEASE_EXPIRED' WHERE id=?", step.ID)
			if err == nil {
				n++
			}
			return err
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
