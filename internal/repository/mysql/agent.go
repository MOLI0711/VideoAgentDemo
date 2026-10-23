package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/jmoiron/sqlx"
)

func (s *Store) CreateAgent(ctx context.Context, owner domain.ID, name, description string, config json.RawMessage) (*domain.Agent, error) {
	if strings.TrimSpace(name) == "" || !json.Valid([]byte(js(config))) {
		return nil, domain.ErrInvalid
	}
	var a domain.Agent
	err := s.tx(ctx, func(t *sqlx.Tx) error {
		id := domain.NewID()
		if _, err := t.ExecContext(ctx, "INSERT INTO agents(id,owner_id,name,description) VALUES(?,?,?,?)", id, owner, name, description); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, "INSERT INTO agent_versions(id,agent_id,version_no,status,config) VALUES(?,?,1,'DRAFT',?)", domain.NewID(), id, js(config)); err != nil {
			return err
		}
		return t.GetContext(ctx, &a, "SELECT * FROM agents WHERE id=?", id)
	})
	return &a, err
}
func (s *Store) ListAgents(ctx context.Context, owner domain.ID) ([]domain.Agent, error) {
	out := []domain.Agent{}
	err := s.DB.SelectContext(ctx, &out, "SELECT * FROM agents WHERE owner_id=? ORDER BY created_at DESC,id", owner)
	return out, err
}
func (s *Store) GetAgent(ctx context.Context, owner, id domain.ID) (*domain.Agent, error) {
	var a domain.Agent
	err := s.DB.GetContext(ctx, &a, "SELECT * FROM agents WHERE id=? AND owner_id=?", id, owner)
	return &a, mapped(err)
}
func (s *Store) ListAgentVersions(ctx context.Context, owner, agent domain.ID) ([]domain.AgentVersion, error) {
	if _, err := s.GetAgent(ctx, owner, agent); err != nil {
		return nil, err
	}
	out := []domain.AgentVersion{}
	err := s.DB.SelectContext(ctx, &out, "SELECT * FROM agent_versions WHERE agent_id=? ORDER BY version_no DESC", agent)
	return out, err
}
func (s *Store) GetAgentVersion(ctx context.Context, owner, id domain.ID) (*domain.AgentVersion, error) {
	var a domain.AgentVersion
	err := s.DB.GetContext(ctx, &a, "SELECT av.* FROM agent_versions av JOIN agents a ON a.id=av.agent_id WHERE av.id=? AND a.owner_id=?", id, owner)
	return &a, mapped(err)
}
func (s *Store) CreateAgentVersion(ctx context.Context, owner, agent domain.ID, config json.RawMessage) (*domain.AgentVersion, error) {
	var out domain.AgentVersion
	if !json.Valid([]byte(js(config))) {
		return nil, domain.ErrInvalid
	}
	err := s.tx(ctx, func(t *sqlx.Tx) error {
		var a domain.Agent
		if err := t.GetContext(ctx, &a, "SELECT * FROM agents WHERE id=? AND owner_id=? FOR UPDATE", agent, owner); err != nil {
			return err
		}
		var seq int
		if err := t.GetContext(ctx, &seq, "SELECT COALESCE(MAX(version_no),0)+1 FROM agent_versions WHERE agent_id=?", agent); err != nil {
			return err
		}
		id := domain.NewID()
		if _, err := t.ExecContext(ctx, "INSERT INTO agent_versions(id,agent_id,version_no,status,config) VALUES(?,?,?,'DRAFT',?)", id, agent, seq, js(config)); err != nil {
			return err
		}
		return t.GetContext(ctx, &out, "SELECT * FROM agent_versions WHERE id=?", id)
	})
	return &out, err
}
func (s *Store) EditDraftVersion(ctx context.Context, owner, id domain.ID, config json.RawMessage) error {
	if !json.Valid([]byte(js(config))) {
		return domain.ErrInvalid
	}
	return s.tx(ctx, func(t *sqlx.Tx) error {
		var v domain.AgentVersion
		if err := t.GetContext(ctx, &v, "SELECT av.* FROM agent_versions av JOIN agents a ON a.id=av.agent_id WHERE av.id=? AND a.owner_id=? FOR UPDATE", id, owner); err != nil {
			return err
		}
		if v.Status != "DRAFT" {
			return domain.ErrConflict
		}
		_, err := t.ExecContext(ctx, "UPDATE agent_versions SET config=? WHERE id=?", js(config), id)
		return err
	})
}
func (s *Store) PublishAgentVersion(ctx context.Context, owner, id domain.ID) error {
	return s.tx(ctx, func(t *sqlx.Tx) error {
		var v domain.AgentVersion
		if err := t.GetContext(ctx, &v, "SELECT av.* FROM agent_versions av JOIN agents a ON a.id=av.agent_id WHERE av.id=? AND a.owner_id=?", id, owner); err != nil {
			return err
		}
		var a domain.Agent
		if err := t.GetContext(ctx, &a, "SELECT * FROM agents WHERE id=? FOR UPDATE", v.AgentID); err != nil {
			return err
		}
		if err := t.GetContext(ctx, &v, "SELECT * FROM agent_versions WHERE id=? FOR UPDATE", id); err != nil {
			return err
		}
		if v.Status != "DRAFT" && v.Status != "PUBLISHED" {
			return domain.ErrConflict
		}
		if _, err := t.ExecContext(ctx, "UPDATE agent_versions SET status='PUBLISHED' WHERE id=?", id); err != nil {
			return err
		}
		_, err := t.ExecContext(ctx, "UPDATE agents SET active_version_id=?,status='ACTIVE' WHERE id=?", id, v.AgentID)
		return err
	})
}
func (s *Store) SetAgentStatus(ctx context.Context, owner, id domain.ID, status string) error {
	if status != "ACTIVE" && status != "DISABLED" {
		return domain.ErrInvalid
	}
	res, err := s.DB.ExecContext(ctx, "UPDATE agents SET status=? WHERE id=? AND owner_id=?", status, id, owner)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		_, err = s.GetAgent(ctx, owner, id)
	}
	return err
}

func (s *Store) CreateSession(ctx context.Context, owner, videoID, agentVersionID, contentVersionID domain.ID) (*domain.Session, error) {
	var out domain.Session
	err := s.tx(ctx, func(t *sqlx.Tx) error {
		var v domain.Video
		if err := t.GetContext(ctx, &v, "SELECT * FROM videos WHERE id=? AND owner_id=? AND deleted_at IS NULL FOR UPDATE", videoID, owner); err != nil {
			return err
		}
		if contentVersionID == (domain.ID{}) {
			contentVersionID = v.ActiveVersionID
		}
		if contentVersionID == (domain.ID{}) {
			return domain.ErrNotReady
		}
		var version domain.VideoVersion
		if err := t.GetContext(ctx, &version, "SELECT * FROM video_versions WHERE id=? AND video_id=? AND status='READY'", contentVersionID, videoID); err != nil {
			return err
		}
		var av domain.AgentVersion
		var err error
		if agentVersionID == (domain.ID{}) {
			err = t.GetContext(ctx, &av, "SELECT av.* FROM agent_versions av JOIN agents a ON a.active_version_id=av.id WHERE a.owner_id=? AND a.status='ACTIVE' AND av.status='PUBLISHED' ORDER BY a.created_at LIMIT 1", owner)
		} else {
			err = t.GetContext(ctx, &av, "SELECT av.* FROM agent_versions av JOIN agents a ON a.id=av.agent_id WHERE av.id=? AND a.owner_id=? AND a.status='ACTIVE' AND av.status='PUBLISHED'", agentVersionID, owner)
		}
		if err != nil {
			return err
		}
		id := domain.NewID()
		if _, err = t.ExecContext(ctx, "INSERT INTO sessions(id,owner_id,video_id,video_version_id,agent_version_id) VALUES(?,?,?,?,?)", id, owner, videoID, contentVersionID, av.ID); err != nil {
			return err
		}
		return t.GetContext(ctx, &out, "SELECT * FROM sessions WHERE id=?", id)
	})
	return &out, err
}

func (s *Store) GetSession(ctx context.Context, owner, id domain.ID) (*domain.Session, error) {
	var out domain.Session
	err := s.DB.GetContext(ctx, &out, "SELECT s.* FROM sessions s JOIN videos v ON v.id=s.video_id WHERE s.id=? AND s.owner_id=? AND v.deleted_at IS NULL", id, owner)
	return &out, mapped(err)
}
func (s *Store) ListSessions(ctx context.Context, owner, video domain.ID, limit int) ([]domain.Session, error) {
	out := []domain.Session{}
	q := "SELECT s.* FROM sessions s JOIN videos v ON v.id=s.video_id WHERE s.owner_id=? AND v.deleted_at IS NULL"
	args := []any{owner}
	if video != (domain.ID{}) {
		q += " AND s.video_id=?"
		args = append(args, video)
	}
	q += " ORDER BY s.created_at DESC,s.id LIMIT ?"
	args = append(args, bounded(limit))
	err := s.DB.SelectContext(ctx, &out, q, args...)
	return out, err
}

func (s *Store) CreateRun(ctx context.Context, owner, sessionID domain.ID, question, key string) (*domain.Run, error) {
	question = strings.TrimSpace(question)
	if question == "" || len(question) > 32000 {
		return nil, domain.ErrInvalid
	}
	var out domain.Run
	body := struct {
		Session  domain.ID
		Question string
	}{sessionID, question}
	err := s.tx(ctx, func(t *sqlx.Tx) error {
		if err := quota(ctx, t, owner); err != nil {
			return err
		}
		id, err := idem(ctx, t, owner, "run", key, body)
		if err != nil {
			return err
		}
		if id != (domain.ID{}) {
			return t.GetContext(ctx, &out, "SELECT * FROM agent_runs WHERE id=?", id)
		}
		var session domain.Session
		if err = t.GetContext(ctx, &session, "SELECT * FROM sessions WHERE id=? AND owner_id=?", sessionID, owner); err != nil {
			return err
		}
		var video domain.Video
		if err = t.GetContext(ctx, &video, "SELECT * FROM videos WHERE id=? AND owner_id=? AND deleted_at IS NULL FOR UPDATE", session.VideoID, owner); err != nil {
			return err
		}
		if err = t.GetContext(ctx, &session, "SELECT * FROM sessions WHERE id=? FOR UPDATE", sessionID); err != nil {
			return err
		}
		if session.ActiveRunID != (domain.ID{}) {
			return domain.ErrConflict
		}
		if err = checkActiveQuota(ctx, t, owner, "QA", 2); err != nil {
			return err
		}
		var av domain.AgentVersion
		if err = t.GetContext(ctx, &av, "SELECT av.* FROM agent_versions av JOIN agents a ON a.id=av.agent_id WHERE av.id=? AND a.status='ACTIVE' AND av.status='PUBLISHED'", session.AgentVersionID); err != nil {
			return err
		}
		var version domain.VideoVersion
		if err = t.GetContext(ctx, &version, "SELECT * FROM video_versions WHERE id=? AND status='READY'", session.VideoVersionID); err != nil {
			return err
		}
		job, err := createJobTx(ctx, t, owner, video.ID, version.ID, "QA", av.Config)
		if err != nil {
			return err
		}
		id = domain.NewID()
		if _, err = t.ExecContext(ctx, "INSERT INTO agent_runs(id,job_id,session_id,owner_id,video_version_id,agent_version_id,index_name,index_build_id,status,question,answer) VALUES(?,?,?,?,?,?,?,?,'QUEUED',?,'{}')", id, job.ID, sessionID, owner, version.ID, av.ID, version.IndexName, version.IndexBuildID, question); err != nil {
			return err
		}
		var seq int64
		if err = t.GetContext(ctx, &seq, "SELECT COALESCE(MAX(seq),0)+1 FROM messages WHERE session_id=?", sessionID); err != nil {
			return err
		}
		if _, err = t.ExecContext(ctx, "INSERT INTO messages(id,session_id,run_id,seq,role,content) VALUES(?,?,?,?,'user',?)", domain.NewID(), sessionID, id, seq, question); err != nil {
			return err
		}
		if _, err = t.ExecContext(ctx, "UPDATE sessions SET active_run_id=? WHERE id=?", id, sessionID); err != nil {
			return err
		}
		if err = saveIdem(ctx, t, owner, "run", key, body, id); err != nil {
			return err
		}
		if err = appendRunEventTx(ctx, t, id, 0, "run.queued", json.RawMessage(`{"status":"QUEUED"}`)); err != nil {
			return err
		}
		return t.GetContext(ctx, &out, "SELECT * FROM agent_runs WHERE id=?", id)
	})
	return &out, err
}

func (s *Store) GetRun(ctx context.Context, owner, id domain.ID) (*domain.Run, error) {
	var out domain.Run
	err := s.DB.GetContext(ctx, &out, "SELECT r.* FROM agent_runs r JOIN video_versions vv ON vv.id=r.video_version_id JOIN videos v ON v.id=vv.video_id WHERE r.id=? AND r.owner_id=? AND v.deleted_at IS NULL", id, owner)
	return &out, mapped(err)
}
func (s *Store) ListMessages(ctx context.Context, owner, session domain.ID) ([]domain.Message, error) {
	if _, err := s.GetSession(ctx, owner, session); err != nil {
		return nil, err
	}
	out := []domain.Message{}
	err := s.DB.SelectContext(ctx, &out, "SELECT * FROM messages WHERE session_id=? ORDER BY seq", session)
	return out, err
}
func (s *Store) ListRunEvents(ctx context.Context, owner, run domain.ID, after int64, limit int) ([]domain.RunEvent, error) {
	if _, err := s.GetRun(ctx, owner, run); err != nil {
		return nil, err
	}
	out := []domain.RunEvent{}
	err := s.DB.SelectContext(ctx, &out, "SELECT * FROM run_events WHERE run_id=? AND seq>? ORDER BY seq LIMIT ?", run, after, bounded(limit))
	return out, err
}

func appendRunEventTx(ctx context.Context, t *sqlx.Tx, run domain.ID, attempt uint64, kind string, payload json.RawMessage) error {
	var seq int64
	if err := t.GetContext(ctx, &seq, "SELECT next_event_seq FROM agent_runs WHERE id=? FOR UPDATE", run); err != nil {
		return err
	}
	seq++
	if _, err := t.ExecContext(ctx, "INSERT INTO run_events(run_id,seq,attempt_no,type,payload) VALUES(?,?,?,?,?)", run, seq, attempt, kind, js(payload)); err != nil {
		return err
	}
	_, err := t.ExecContext(ctx, "UPDATE agent_runs SET next_event_seq=? WHERE id=?", seq, run)
	return err
}
func (s *Store) AppendRunEvent(ctx context.Context, l domain.Lease, kind string, payload json.RawMessage) error {
	return s.tx(ctx, func(t *sqlx.Tx) error {
		_, _, r, err := lockLease(ctx, t, l)
		if err != nil {
			return err
		}
		if r == nil {
			return domain.ErrNotFound
		}
		if _, err = t.ExecContext(ctx, "UPDATE agent_runs SET status='RUNNING' WHERE id=? AND status='QUEUED'", r.ID); err != nil {
			return err
		}
		return appendRunEventTx(ctx, t, r.ID, l.AttemptNo, kind, payload)
	})
}

func finishRunTx(ctx context.Context, t *sqlx.Tx, r *domain.Run, attempt uint64, status string, answer json.RawMessage) error {
	if _, err := t.ExecContext(ctx, "UPDATE agent_runs SET status=?,answer=? WHERE id=?", status, js(answer), r.ID); err != nil {
		return err
	}
	if status == domain.Succeeded {
		var seq int64
		if err := t.GetContext(ctx, &seq, "SELECT COALESCE(MAX(seq),0)+1 FROM messages WHERE session_id=?", r.SessionID); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, "INSERT INTO messages(id,session_id,run_id,seq,role,content) VALUES(?,?,?,?,'assistant',?)", domain.NewID(), r.SessionID, r.ID, seq, js(answer)); err != nil {
			return err
		}
	}
	if _, err := t.ExecContext(ctx, "UPDATE sessions SET active_run_id=? WHERE id=? AND active_run_id=?", domain.ID{}, r.SessionID, r.ID); err != nil {
		return err
	}
	kind := "run.failed"
	if status == domain.Succeeded {
		kind = "answer.completed"
	}
	if status == domain.Cancelled {
		kind = "run.cancelled"
	}
	payload, _ := json.Marshal(map[string]any{"status": status, "answer": json.RawMessage(js(answer))})
	return appendRunEventTx(ctx, t, r.ID, attempt, kind, payload)
}

func (s *Store) CreateSummary(ctx context.Context, owner, videoID domain.ID, key string) (*domain.Job, error) {
	return s.CreateSummaryPinned(ctx, owner, videoID, domain.ID{}, domain.ID{}, key)
}

func (s *Store) CreateSummaryPinned(ctx context.Context, owner, videoID, contentVersion, agentVersion domain.ID, key string) (*domain.Job, error) {
	var out domain.Job
	body := struct{ Video, ContentVersion, AgentVersion domain.ID }{videoID, contentVersion, agentVersion}
	err := s.tx(ctx, func(t *sqlx.Tx) error {
		if err := quota(ctx, t, owner); err != nil {
			return err
		}
		id, err := idem(ctx, t, owner, "summary", key, body)
		if err != nil {
			return err
		}
		if id != (domain.ID{}) {
			return t.GetContext(ctx, &out, "SELECT * FROM jobs WHERE id=?", id)
		}
		var video domain.Video
		if err = t.GetContext(ctx, &video, "SELECT * FROM videos WHERE id=? AND owner_id=? AND deleted_at IS NULL FOR UPDATE", videoID, owner); err != nil {
			return err
		}
		selected := contentVersion
		if selected == (domain.ID{}) {
			selected = video.ActiveVersionID
		}
		if selected == (domain.ID{}) {
			return domain.ErrNotReady
		}
		var version domain.VideoVersion
		if err = t.GetContext(ctx, &version, "SELECT * FROM video_versions WHERE id=? AND video_id=? AND status='READY'", selected, videoID); err != nil {
			return err
		}
		var av domain.AgentVersion
		if agentVersion == (domain.ID{}) {
			err = t.GetContext(ctx, &av, "SELECT av.* FROM agent_versions av JOIN agents a ON a.active_version_id=av.id WHERE a.owner_id=? AND a.status='ACTIVE' AND av.status='PUBLISHED' ORDER BY a.created_at LIMIT 1", owner)
		} else {
			err = t.GetContext(ctx, &av, "SELECT av.* FROM agent_versions av JOIN agents a ON a.id=av.agent_id WHERE av.id=? AND a.owner_id=? AND a.status='ACTIVE' AND av.status='PUBLISHED'", agentVersion, owner)
		}
		if err != nil {
			return err
		}
		var config map[string]any
		if err = json.Unmarshal(av.Config, &config); err != nil {
			return err
		}
		if config == nil {
			config = map[string]any{}
		}
		config["content_version"] = version.ID
		config["agent_version_id"] = av.ID
		config["index_name"] = version.IndexName
		config["index_build_id"] = version.IndexBuildID
		configJSON, err := json.Marshal(config)
		if err != nil {
			return err
		}
		if err = checkActiveQuota(ctx, t, owner, "SUMMARY", 2); err != nil {
			return err
		}
		job, err := createJobTx(ctx, t, owner, videoID, version.ID, "SUMMARY", configJSON)
		if err != nil {
			return err
		}
		out = *job
		return saveIdem(ctx, t, owner, "summary", key, body, out.ID)
	})
	return &out, err
}

func (s *Store) SaveCheckpoint(ctx context.Context, l domain.Lease, state json.RawMessage) error {
	if !json.Valid(state) {
		return domain.ErrInvalid
	}
	return s.tx(ctx, func(t *sqlx.Tx) error {
		_, _, r, err := lockLease(ctx, t, l)
		if err != nil {
			return err
		}
		if r == nil {
			return domain.ErrNotFound
		}
		if err = t.GetContext(ctx, r, "SELECT * FROM agent_runs WHERE id=? FOR UPDATE", r.ID); err != nil {
			return err
		}
		seq := r.CheckpointSeq + 1
		if _, err = t.ExecContext(ctx, "INSERT INTO run_checkpoints(run_id,seq,attempt_no,state) VALUES(?,?,?,?)", r.ID, seq, l.AttemptNo, state); err != nil {
			return err
		}
		_, err = t.ExecContext(ctx, "UPDATE agent_runs SET checkpoint_seq=? WHERE id=?", seq, r.ID)
		return err
	})
}
func (s *Store) GetCheckpoint(ctx context.Context, run domain.ID) (*domain.Checkpoint, error) {
	var c domain.Checkpoint
	err := s.DB.GetContext(ctx, &c, "SELECT * FROM run_checkpoints WHERE run_id=? ORDER BY seq DESC LIMIT 1", run)
	return &c, mapped(err)
}

func (s *Store) SaveToolIntent(ctx context.Context, l domain.Lease, logicalID, name string, args json.RawMessage, maxCalls int) (*domain.ToolInvocation, bool, error) {
	if logicalID == "" || name == "" || !json.Valid(args) {
		return nil, false, domain.ErrInvalid
	}
	var out domain.ToolInvocation
	created := false
	err := s.tx(ctx, func(t *sqlx.Tx) error {
		_, _, r, err := lockLease(ctx, t, l)
		if err != nil {
			return err
		}
		if r == nil {
			return domain.ErrNotFound
		}
		if err = t.GetContext(ctx, r, "SELECT * FROM agent_runs WHERE id=? FOR UPDATE", r.ID); err != nil {
			return err
		}
		err = t.GetContext(ctx, &out, "SELECT * FROM tool_invocations WHERE run_id=? AND logical_call_id=?", r.ID, logicalID)
		if err == nil {
			if out.ToolName != name || canonical(out.Args) != canonical(args) {
				return domain.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if maxCalls <= 0 {
			maxCalls = 8
		}
		if r.BudgetUsed >= maxCalls {
			return domain.ErrBudgetExceeded
		}
		id := domain.NewID()
		if _, err = t.ExecContext(ctx, "INSERT INTO tool_invocations(id,run_id,logical_call_id,tool_name,args,status,result,attempt_no) VALUES(?,?,?,?,?,'PENDING','{}',?)", id, r.ID, logicalID, name, args, l.AttemptNo); err != nil {
			return err
		}
		if _, err = t.ExecContext(ctx, "UPDATE agent_runs SET budget_used=budget_used+1 WHERE id=?", r.ID); err != nil {
			return err
		}
		created = true
		return t.GetContext(ctx, &out, "SELECT * FROM tool_invocations WHERE id=?", id)
	})
	return &out, created, err
}
func canonical(b json.RawMessage) string {
	var x any
	if json.Unmarshal(b, &x) != nil {
		return string(b)
	}
	v, _ := json.Marshal(x)
	return string(v)
}
func (s *Store) SaveToolResult(ctx context.Context, l domain.Lease, logicalID string, result json.RawMessage) error {
	if !json.Valid(result) {
		return domain.ErrInvalid
	}
	return s.tx(ctx, func(t *sqlx.Tx) error {
		_, _, r, err := lockLease(ctx, t, l)
		if err != nil {
			return err
		}
		if r == nil {
			return domain.ErrNotFound
		}
		var call domain.ToolInvocation
		if err = t.GetContext(ctx, &call, "SELECT * FROM tool_invocations WHERE run_id=? AND logical_call_id=? FOR UPDATE", r.ID, logicalID); err != nil {
			return err
		}
		if call.Status == "COMPLETED" {
			return nil
		}
		_, err = t.ExecContext(ctx, "UPDATE tool_invocations SET status='COMPLETED',result=?,attempt_no=? WHERE id=?", result, l.AttemptNo, call.ID)
		return err
	})
}
func (s *Store) ListToolInvocations(ctx context.Context, run domain.ID) ([]domain.ToolInvocation, error) {
	out := []domain.ToolInvocation{}
	err := s.DB.SelectContext(ctx, &out, "SELECT * FROM tool_invocations WHERE run_id=? ORDER BY id", run)
	return out, err
}
func (s *Store) ListSentences(ctx context.Context, owner, version domain.ID) ([]domain.Sentence, error) {
	if _, err := s.GetVersion(ctx, owner, version); err != nil {
		return nil, err
	}
	return s.LoadSentences(ctx, version)
}

// DeleteVideo revokes every active task in the same transaction as the tombstone.
// Object/index cleanup is deliberately separate from this authorization boundary.
func (s *Store) DeleteVideo(ctx context.Context, owner, id domain.ID) error {
	return s.tx(ctx, func(t *sqlx.Tx) error {
		var video domain.Video
		if err := t.GetContext(ctx, &video, "SELECT * FROM videos WHERE id=? AND owner_id=? FOR UPDATE", id, owner); err != nil {
			return err
		}
		if video.DeletedAt != nil {
			return nil
		}
		var sessionIDs []domain.ID
		if err := t.SelectContext(ctx, &sessionIDs, "SELECT id FROM sessions WHERE video_id=? ORDER BY id FOR UPDATE", id); err != nil {
			return err
		}
		var jobs []domain.Job
		if err := t.SelectContext(ctx, &jobs, "SELECT * FROM jobs WHERE video_id=? AND status IN ('QUEUED','RUNNING','CANCELLING') ORDER BY id FOR UPDATE", id); err != nil {
			return err
		}
		for i := range jobs {
			var run domain.Run
			err := t.GetContext(ctx, &run, "SELECT * FROM agent_runs WHERE job_id=?", jobs[i].ID)
			var rp *domain.Run
			if err == nil {
				rp = &run
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err = failJobTx(ctx, t, &jobs[i], rp, domain.Cancelled, "VIDEO_DELETED", "video deleted"); err != nil {
				return err
			}
		}
		_, err := t.ExecContext(ctx, "UPDATE videos SET deleted_at=UTC_TIMESTAMP(6),status='DELETED',updated_at=UTC_TIMESTAMP(6) WHERE id=?", id)
		return err
	})
}
