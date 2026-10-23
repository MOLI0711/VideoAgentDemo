package mysql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/jmoiron/sqlx"
)

func (s *Store) CreateUpload(ctx context.Context, owner domain.ID, in domain.CreateUpload) (*domain.Upload, error) {
	if strings.TrimSpace(in.Title) == "" || in.Size <= 0 || in.ObjectKey == "" || in.MultipartID == "" {
		return nil, domain.ErrInvalid
	}
	var out domain.Upload
	body := struct {
		Title, Filename, ContentType string
		Size                         int64
	}{in.Title, in.Filename, in.ContentType, in.Size}
	err := s.tx(ctx, func(t *sqlx.Tx) error {
		if err := quota(ctx, t, owner); err != nil {
			return err
		}
		id, err := idem(ctx, t, owner, "upload", in.IdempotencyKey, body)
		if err != nil {
			return err
		}
		if id != (domain.ID{}) {
			return t.GetContext(ctx, &out, "SELECT * FROM upload_sessions WHERE id=?", id)
		}
		videoID := domain.NewID()
		id = domain.NewID()
		if _, err = t.ExecContext(ctx, "INSERT INTO videos(id,owner_id,title,status) VALUES(?,?,?,'UPLOADING')", videoID, owner, in.Title); err != nil {
			return err
		}
		_, err = t.ExecContext(ctx, "INSERT INTO upload_sessions(id,owner_id,video_id,multipart_id,object_key,filename,content_type,size,status,expires_at) VALUES(?,?,?,?,?,?,?,?,'UPLOADING',TIMESTAMPADD(DAY,1,UTC_TIMESTAMP(6)))", id, owner, videoID, in.MultipartID, in.ObjectKey, in.Filename, in.ContentType, in.Size)
		if err != nil {
			return err
		}
		if err = saveIdem(ctx, t, owner, "upload", in.IdempotencyKey, body, id); err != nil {
			return err
		}
		return t.GetContext(ctx, &out, "SELECT * FROM upload_sessions WHERE id=?", id)
	})
	return &out, err
}

func (s *Store) GetUpload(ctx context.Context, owner, id domain.ID) (*domain.Upload, error) {
	var x domain.Upload
	err := s.DB.GetContext(ctx, &x, "SELECT u.* FROM upload_sessions u JOIN videos v ON v.id=u.video_id WHERE u.id=? AND u.owner_id=? AND v.deleted_at IS NULL", id, owner)
	return &x, mapped(err)
}

func insertAsset(ctx context.Context, t *sqlx.Tx, a domain.Asset) error {
	if a.ID == (domain.ID{}) {
		a.ID = domain.NewID()
	}
	if a.StorageBackend == "" {
		a.StorageBackend = "s3"
	}
	if a.ObjectKey == "" {
		return domain.ErrInvalid
	}
	_, err := t.ExecContext(ctx, "INSERT INTO media_assets(id,owner_id,video_id,video_version_id,step_id,attempt_no,kind,storage_backend,object_key,content_type,size,checksum,start_ms,end_ms,metadata) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", a.ID, a.OwnerID, a.VideoID, a.VideoVersionID, a.StepID, a.AttemptNo, a.Kind, a.StorageBackend, a.ObjectKey, a.ContentType, a.Size, a.Checksum, a.StartMS, a.EndMS, js(a.Metadata))
	return err
}

func (s *Store) CompleteUpload(ctx context.Context, owner, id domain.ID, asset domain.Asset, config json.RawMessage) (*domain.Job, error) {
	var out domain.Job
	err := s.tx(ctx, func(t *sqlx.Tx) error {
		if err := quota(ctx, t, owner); err != nil {
			return err
		}
		var upload domain.Upload
		if err := t.GetContext(ctx, &upload, "SELECT * FROM upload_sessions WHERE id=? AND owner_id=?", id, owner); err != nil {
			return err
		}
		var video domain.Video
		if err := t.GetContext(ctx, &video, "SELECT * FROM videos WHERE id=? AND owner_id=? AND deleted_at IS NULL FOR UPDATE", upload.VideoID, owner); err != nil {
			return err
		}
		if err := t.GetContext(ctx, &upload, "SELECT * FROM upload_sessions WHERE id=? FOR UPDATE", id); err != nil {
			return err
		}
		if upload.Status == "COMPLETED" {
			return t.GetContext(ctx, &out, "SELECT * FROM jobs WHERE id=?", upload.JobID)
		}
		if upload.Status != "UPLOADING" && upload.Status != "COMPLETING" {
			return domain.ErrConflict
		}
		if asset.ObjectKey != upload.ObjectKey || asset.Size != upload.Size {
			return fmt.Errorf("%w: uploaded object differs from declaration", domain.ErrInvalid)
		}
		if err := checkActiveQuota(ctx, t, owner, "PREPROCESS", 2); err != nil {
			return err
		}
		asset.ID = domain.NewID()
		asset.OwnerID = owner
		asset.VideoID = video.ID
		asset.VideoVersionID = domain.ID{}
		asset.StepID = domain.ID{}
		asset.Kind = "SOURCE"
		asset.AttemptNo = 0
		if err := insertAsset(ctx, t, asset); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, "UPDATE videos SET source_asset_id=?,status='PROCESSING',updated_at=UTC_TIMESTAMP(6) WHERE id=?", asset.ID, video.ID); err != nil {
			return err
		}
		job, err := createVersionJob(ctx, t, video, config)
		if err != nil {
			return err
		}
		out = *job
		_, err = t.ExecContext(ctx, "UPDATE upload_sessions SET status='COMPLETED',job_id=? WHERE id=?", job.ID, id)
		return err
	})
	return &out, err
}

func checkActiveQuota(ctx context.Context, t *sqlx.Tx, owner domain.ID, kind string, max int) error {
	var n int
	if err := t.GetContext(ctx, &n, "SELECT COUNT(*) FROM jobs WHERE owner_id=? AND type=? AND status IN ('QUEUED','RUNNING','CANCELLING')", owner, kind); err != nil {
		return err
	}
	if n >= max {
		return fmt.Errorf("%w: active task limit", domain.ErrConflict)
	}
	return nil
}

func createVersionJob(ctx context.Context, t *sqlx.Tx, v domain.Video, config json.RawMessage) (*domain.Job, error) {
	version := domain.NewID()
	generation := v.DesiredGeneration + 1
	if _, err := t.ExecContext(ctx, "UPDATE videos SET desired_generation=?,status='PROCESSING',updated_at=UTC_TIMESTAMP(6) WHERE id=?", generation, v.ID); err != nil {
		return nil, err
	}
	if _, err := t.ExecContext(ctx, "INSERT INTO video_versions(id,video_id,generation,status,config,metadata) VALUES(?,?,?,'PROCESSING',?,'{}')", version, v.ID, generation, js(config)); err != nil {
		return nil, err
	}
	return createJobTx(ctx, t, v.OwnerID, v.ID, version, "PREPROCESS", config)
}

func (s *Store) Reprocess(ctx context.Context, owner, videoID domain.ID, key string, config json.RawMessage) (*domain.Job, error) {
	var out domain.Job
	body := struct {
		VideoID domain.ID
		Config  json.RawMessage
	}{videoID, json.RawMessage(js(config))}
	err := s.tx(ctx, func(t *sqlx.Tx) error {
		if err := quota(ctx, t, owner); err != nil {
			return err
		}
		id, err := idem(ctx, t, owner, "reprocess", key, body)
		if err != nil {
			return err
		}
		if id != (domain.ID{}) {
			return t.GetContext(ctx, &out, "SELECT * FROM jobs WHERE id=?", id)
		}
		var v domain.Video
		if err = t.GetContext(ctx, &v, "SELECT * FROM videos WHERE id=? AND owner_id=? AND deleted_at IS NULL FOR UPDATE", videoID, owner); err != nil {
			return err
		}
		if v.SourceAssetID == (domain.ID{}) {
			return domain.ErrNotReady
		}
		if err = checkActiveQuota(ctx, t, owner, "PREPROCESS", 2); err != nil {
			return err
		}
		job, err := createVersionJob(ctx, t, v, config)
		if err != nil {
			return err
		}
		out = *job
		return saveIdem(ctx, t, owner, "reprocess", key, body, out.ID)
	})
	return &out, err
}

func (s *Store) ListVideos(ctx context.Context, owner domain.ID, limit int) ([]domain.Video, error) {
	out := []domain.Video{}
	err := s.DB.SelectContext(ctx, &out, "SELECT * FROM videos WHERE owner_id=? AND deleted_at IS NULL ORDER BY created_at DESC,id DESC LIMIT ?", owner, bounded(limit))
	return out, err
}
func (s *Store) GetVideo(ctx context.Context, owner, id domain.ID) (*domain.Video, error) {
	var out domain.Video
	err := s.DB.GetContext(ctx, &out, "SELECT * FROM videos WHERE id=? AND owner_id=? AND deleted_at IS NULL", id, owner)
	return &out, mapped(err)
}
func (s *Store) GetVersion(ctx context.Context, owner, id domain.ID) (*domain.VideoVersion, error) {
	var out domain.VideoVersion
	err := s.DB.GetContext(ctx, &out, "SELECT vv.* FROM video_versions vv JOIN videos v ON v.id=vv.video_id WHERE vv.id=? AND v.owner_id=? AND v.deleted_at IS NULL", id, owner)
	return &out, mapped(err)
}
func (s *Store) ListVersions(ctx context.Context, owner, video domain.ID) ([]domain.VideoVersion, error) {
	if _, err := s.GetVideo(ctx, owner, video); err != nil {
		return nil, err
	}
	out := []domain.VideoVersion{}
	err := s.DB.SelectContext(ctx, &out, "SELECT * FROM video_versions WHERE video_id=? ORDER BY generation DESC", video)
	return out, err
}
func (s *Store) GetAsset(ctx context.Context, owner, id domain.ID) (*domain.Asset, error) {
	var out domain.Asset
	err := s.DB.GetContext(ctx, &out, "SELECT a.* FROM media_assets a JOIN videos v ON v.id=a.video_id WHERE a.id=? AND a.owner_id=? AND v.deleted_at IS NULL", id, owner)
	return &out, mapped(err)
}
func (s *Store) ListAssets(ctx context.Context, owner, video, version domain.ID, kind string) ([]domain.Asset, error) {
	if _, err := s.GetVideo(ctx, owner, video); err != nil {
		return nil, err
	}
	out := []domain.Asset{}
	q := "SELECT * FROM media_assets WHERE owner_id=? AND video_id=?"
	args := []any{owner, video}
	if version != (domain.ID{}) {
		q += " AND video_version_id=?"
		args = append(args, version)
	}
	if kind != "" {
		q += " AND kind=?"
		args = append(args, kind)
	}
	q += " ORDER BY start_ms,id"
	err := s.DB.SelectContext(ctx, &out, q, args...)
	return out, err
}
func (s *Store) ListSegments(ctx context.Context, owner, version domain.ID, limit int) ([]domain.Segment, error) {
	if _, err := s.GetVersion(ctx, owner, version); err != nil {
		return nil, err
	}
	out := []domain.Segment{}
	err := s.DB.SelectContext(ctx, &out, "SELECT * FROM video_segments WHERE video_version_id=? ORDER BY ordinal LIMIT ?", version, boundedContent(limit))
	return out, err
}
func (s *Store) ListFrames(ctx context.Context, owner, version domain.ID, limit int) ([]domain.Frame, error) {
	if _, err := s.GetVersion(ctx, owner, version); err != nil {
		return nil, err
	}
	out := []domain.Frame{}
	err := s.DB.SelectContext(ctx, &out, "SELECT * FROM video_frames WHERE video_version_id=? ORDER BY timestamp_ms,id LIMIT ?", version, boundedContent(limit))
	return out, err
}
func (s *Store) LoadSentences(ctx context.Context, version domain.ID) ([]domain.Sentence, error) {
	out := []domain.Sentence{}
	err := s.DB.SelectContext(ctx, &out, "SELECT * FROM transcript_sentences WHERE video_version_id=? ORDER BY start_ms,id", version)
	return out, err
}
func (s *Store) LoadFrames(ctx context.Context, version domain.ID) ([]domain.Frame, error) {
	out := []domain.Frame{}
	err := s.DB.SelectContext(ctx, &out, "SELECT * FROM video_frames WHERE video_version_id=? ORDER BY timestamp_ms,id", version)
	return out, err
}
func (s *Store) LoadSegments(ctx context.Context, version domain.ID) ([]domain.Segment, error) {
	out := []domain.Segment{}
	err := s.DB.SelectContext(ctx, &out, "SELECT * FROM video_segments WHERE video_version_id=? ORDER BY ordinal", version)
	return out, err
}

func (s *Store) AbortUpload(ctx context.Context, owner, id domain.ID) error {
	return s.tx(ctx, func(t *sqlx.Tx) error {
		var u domain.Upload
		if err := t.GetContext(ctx, &u, "SELECT * FROM upload_sessions WHERE id=? AND owner_id=? FOR UPDATE", id, owner); err != nil {
			return err
		}
		if u.Status == "COMPLETED" {
			return domain.ErrConflict
		}
		_, err := t.ExecContext(ctx, "UPDATE upload_sessions SET status='ABORTED' WHERE id=?", id)
		return err
	})
}

func (s *Store) TouchVideoMetadata(ctx context.Context, lease domain.Lease, duration time.Duration) error {
	return s.CompleteStep(ctx, lease, domain.StepResult{DurationMS: duration.Milliseconds()})
}
