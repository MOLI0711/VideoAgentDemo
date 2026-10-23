package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/jmoiron/sqlx"
)

func (s *Store) ClaimOutbox(ctx context.Context, limit int, ttl time.Duration) ([]domain.OutboxEvent, error) {
	out := []domain.OutboxEvent{}
	err := s.tx(ctx, func(t *sqlx.Tx) error {
		if err := t.SelectContext(ctx, &out, "SELECT * FROM outbox_events WHERE (status='PENDING' AND next_publish_at<=UTC_TIMESTAMP(6)) OR (status='PUBLISHING' AND publish_lease_until<=UTC_TIMESTAMP(6)) ORDER BY next_publish_at,event_id LIMIT ? FOR UPDATE SKIP LOCKED", bounded(limit)); err != nil {
			return err
		}
		for i := range out {
			if _, err := t.ExecContext(ctx, "UPDATE outbox_events SET status='PUBLISHING',publish_attempt=publish_attempt+1,publish_lease_until=TIMESTAMPADD(MICROSECOND,?,UTC_TIMESTAMP(6)) WHERE event_id=?", ttl.Microseconds(), out[i].EventID); err != nil {
				return err
			}
			out[i].PublishAttempt++
			out[i].Status = "PUBLISHING"
		}
		return nil
	})
	return out, err
}

func (s *Store) MarkOutboxPublished(ctx context.Context, event domain.OutboxEvent) error {
	res, err := s.DB.ExecContext(ctx, "UPDATE outbox_events SET status='PUBLISHED',publish_lease_until=NULL WHERE event_id=? AND status='PUBLISHING' AND publish_attempt=? AND publish_lease_until>UTC_TIMESTAMP(6)", event.EventID, event.PublishAttempt)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return domain.ErrLeaseLost
	}
	return nil
}
func (s *Store) RetryOutbox(ctx context.Context, event domain.OutboxEvent, delay time.Duration) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE outbox_events SET status='PENDING',publish_lease_until=NULL,next_publish_at=TIMESTAMPADD(MICROSECOND,?,UTC_TIMESTAMP(6)) WHERE event_id=? AND status='PUBLISHING' AND publish_attempt=?", delay.Microseconds(), event.EventID, event.PublishAttempt)
	return err
}

func (s *Store) AcceptDispatch(ctx context.Context, consumer string, event domain.DispatchEvent) error {
	if event.SchemaVersion != 1 || event.EventID == (domain.ID{}) || event.StepID == (domain.ID{}) || event.DispatchSeq == 0 {
		return domain.ErrInvalid
	}
	return s.tx(ctx, func(t *sqlx.Tx) error {
		// Lock the job before the step, like completion and cancellation.
		var job domain.Job
		if err := t.GetContext(ctx, &job, "SELECT * FROM jobs WHERE id=? FOR UPDATE", event.JobID); err != nil {
			return err
		}
		var exists int
		if err := t.GetContext(ctx, &exists, "SELECT COUNT(*) FROM inbox_events WHERE consumer_name=? AND event_id=?", consumer, event.EventID); err != nil {
			return err
		}
		if exists > 0 {
			return nil
		}
		if _, err := t.ExecContext(ctx, "INSERT INTO inbox_events(consumer_name,event_id) VALUES(?,?)", consumer, event.EventID); err != nil {
			return err
		}
		if !active(job.Status) {
			return nil
		}
		_, err := t.ExecContext(ctx, "UPDATE job_steps SET status='READY',next_run_at=UTC_TIMESTAMP(6) WHERE id=? AND job_id=? AND worker_kind=? AND status='QUEUED' AND dispatch_seq=?", event.StepID, event.JobID, event.WorkerKind, event.DispatchSeq)
		return err
	})
}

// DispatchLocal uses the exact same durable acceptance contract as Kafka. This
// method is for the explicitly configured db_dev transport only.
func (s *Store) DispatchLocal(ctx context.Context, limit int) (int, error) {
	events, err := s.ClaimOutbox(ctx, limit, 30*time.Second)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range events {
		var d domain.DispatchEvent
		if err = json.Unmarshal(e.Payload, &d); err != nil {
			return n, err
		}
		if err = s.AcceptDispatch(ctx, "video-dispatch", d); err != nil {
			return n, err
		}
		if err = s.MarkOutboxPublished(ctx, e); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *Store) Quarantine(ctx context.Context, topic string, partition int32, offset int64, code string) error {
	_, err := s.DB.ExecContext(ctx, "INSERT IGNORE INTO quarantined_events(topic,partition_id,offset_value,error_code) VALUES(?,?,?,?)", topic, partition, offset, code)
	return err
}

func (s *Store) BeginExternal(ctx context.Context, l domain.Lease, provider string, request json.RawMessage) (*domain.ExternalJob, bool, error) {
	var out domain.ExternalJob
	created := false
	err := s.tx(ctx, func(t *sqlx.Tx) error {
		_, step, _, err := lockLease(ctx, t, l)
		if err != nil {
			return err
		}
		if step.CurrentExternalJobID != (domain.ID{}) {
			return t.GetContext(ctx, &out, "SELECT * FROM external_jobs WHERE id=?", step.CurrentExternalJobID)
		}
		id := domain.NewID()
		requestID := domain.NewID().String()
		_, err = t.ExecContext(ctx, "INSERT INTO external_jobs(id,step_id,submit_generation,provider_ref,client_request_id,status,request,deadline_at) VALUES(?,?,1,?,?,'SUBMITTING',?,TIMESTAMPADD(HOUR,1,UTC_TIMESTAMP(6)))", id, step.ID, provider, requestID, js(request))
		if err != nil {
			return err
		}
		if _, err = t.ExecContext(ctx, "UPDATE job_steps SET current_external_job_id=? WHERE id=?", id, step.ID); err != nil {
			return err
		}
		created = true
		return t.GetContext(ctx, &out, "SELECT * FROM external_jobs WHERE id=?", id)
	})
	return &out, created, err
}

func (s *Store) GetExternalJob(ctx context.Context, id domain.ID) (*domain.ExternalJob, error) {
	var e domain.ExternalJob
	err := s.DB.GetContext(ctx, &e, "SELECT * FROM external_jobs WHERE id=?", id)
	return &e, mapped(err)
}

func (s *Store) WaitExternal(ctx context.Context, l domain.Lease, wait domain.ExternalWait) error {
	return s.tx(ctx, func(t *sqlx.Tx) error {
		_, step, _, err := lockLease(ctx, t, l)
		if err != nil {
			return err
		}
		if wait.ExternalJobID != step.CurrentExternalJobID || wait.ProviderJobID == "" {
			return domain.ErrInvalid
		}
		var ext domain.ExternalJob
		if err = t.GetContext(ctx, &ext, "SELECT * FROM external_jobs WHERE id=? AND step_id=? FOR UPDATE", wait.ExternalJobID, step.ID); err != nil {
			return err
		}
		if ext.ProviderJobID != "" && ext.ProviderJobID != wait.ProviderJobID {
			return domain.ErrConflict
		}
		delay := wait.PollAfter
		if delay < time.Second {
			delay = 5 * time.Second
		}
		if _, err = t.ExecContext(ctx, "UPDATE external_jobs SET provider_job_id=?,status='PENDING' WHERE id=?", wait.ProviderJobID, ext.ID); err != nil {
			return err
		}
		if _, err = t.ExecContext(ctx, "UPDATE job_steps SET status='WAITING_EXTERNAL',next_poll_at=TIMESTAMPADD(MICROSECOND,?,UTC_TIMESTAMP(6)),lease_until=NULL,lease_owner='' WHERE id=?", delay.Microseconds(), step.ID); err != nil {
			return err
		}
		_, err = t.ExecContext(ctx, "UPDATE job_attempts SET status='WAITING_EXTERNAL',ended_at=UTC_TIMESTAMP(6) WHERE step_id=? AND attempt_no=?", step.ID, l.AttemptNo)
		return err
	})
}

func (s *Store) MarkExternalUnknown(ctx context.Context, l domain.Lease) error {
	return s.tx(ctx, func(t *sqlx.Tx) error {
		j, step, r, err := lockLease(ctx, t, l)
		if err != nil {
			return err
		}
		if step.CurrentExternalJobID != (domain.ID{}) {
			if _, err = t.ExecContext(ctx, "UPDATE external_jobs SET status='SUBMISSION_UNKNOWN' WHERE id=?", step.CurrentExternalJobID); err != nil {
				return err
			}
		}
		return failJobTx(ctx, t, j, r, domain.Failed, "SUBMISSION_UNKNOWN", "provider submission outcome requires reconciliation")
	})
}

func (s *Store) GetStep(ctx context.Context, id domain.ID) (*domain.Step, error) {
	var out domain.Step
	err := s.DB.GetContext(ctx, &out, "SELECT * FROM job_steps WHERE id=?", id)
	return &out, mapped(err)
}

func (s *Store) GetRunByJob(ctx context.Context, job domain.ID) (*domain.Run, error) {
	var r domain.Run
	err := s.DB.GetContext(ctx, &r, "SELECT * FROM agent_runs WHERE job_id=?", job)
	return &r, mapped(err)
}

// ExpireDeadlines also closes tasks that have not yet acquired a lease.
func (s *Store) ExpireDeadlines(ctx context.Context, limit int) (int, error) {
	var ids []domain.ID
	err := s.DB.SelectContext(ctx, &ids, "SELECT id FROM jobs WHERE status IN ('QUEUED','RUNNING') AND deadline_at<=UTC_TIMESTAMP(6) LIMIT ?", bounded(limit))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		err = s.tx(ctx, func(t *sqlx.Tx) error {
			j, r, err := lockJob(ctx, t, id)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if !active(j.Status) {
				return nil
			}
			n++
			return failJobTx(ctx, t, j, r, domain.Failed, "DEADLINE_EXCEEDED", "task deadline exceeded")
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
