package api

import (
	"errors"
	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/MOLI0711/MOLIVedio/internal/storage"
	"github.com/google/uuid"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type partDTO struct {
	Number int32  `json:"part_number"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size_bytes,omitempty"`
}

func (a *Server) uploads() {
	a.route("POST /v1/uploads", true, a.createUpload)
	a.route("POST /v1/uploads/{id}/part-urls", true, a.partURLs)
	a.route("GET /v1/uploads/{id}/parts", true, a.parts)
	a.route("POST /v1/uploads/{id}/complete", true, a.completeUpload)
	a.route("DELETE /v1/uploads/{id}", true, a.abortUpload)
}
func (a *Server) upload(r *http.Request) (*domain.Upload, error) {
	i, e := id(r, "id")
	if e != nil {
		return nil, e
	}
	return a.Store.GetUpload(r.Context(), user(r).ID, i)
}
func (a *Server) createUpload(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Title       string `json:"title"`
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size_bytes"`
	}
	if e := decode(w, r, &in); e != nil {
		return e
	}
	if in.Size <= 0 || in.Size > a.Config.Limits.UploadMaxBytes || len(in.Filename) > 255 || in.Filename == "" || len(in.Title) > 200 {
		return domain.ErrInvalid
	}
	switch strings.ToLower(filepath.Ext(in.Filename)) {
	case ".mp4", ".mov", ".mkv", ".webm":
	default:
		return &failure{400, "UNSUPPORTED_MEDIA", "支持 MP4、MOV、MKV、WebM 视频"}
	}
	if !strings.HasPrefix(in.ContentType, "video/") {
		in.ContentType = "application/octet-stream"
	}
	if in.Title == "" {
		in.Title = filepath.Base(in.Filename)
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 {
		return &failure{400, "IDEMPOTENCY_KEY_REQUIRED", "必须提供 8–128 字符的幂等键"}
	}
	objectKey := "uploads/" + user(r).ID.String() + "/" + uuid.NewString() + "/source" + strings.ToLower(filepath.Ext(in.Filename))
	remote, e := a.Storage.CreateMultipart(r.Context(), objectKey, in.ContentType)
	if e != nil {
		return e
	}
	u, e := a.Store.CreateUpload(r.Context(), user(r).ID, domain.CreateUpload{Title: in.Title, Filename: in.Filename, ContentType: in.ContentType, Size: in.Size, MultipartID: remote, ObjectKey: objectKey, IdempotencyKey: key})
	// An uncertain DB commit may own the remote multipart. Its lifecycle policy cleans true orphans.
	if e != nil {
		if !errors.Is(e, domain.ErrCommitUnknown) {
			_ = a.Storage.Abort(r.Context(), objectKey, remote)
		}
		return e
	}
	if u.MultipartID != remote {
		_ = a.Storage.Abort(r.Context(), objectKey, remote)
	}
	return reply(w, 201, map[string]any{"upload_id": u.ID, "video_id": u.VideoID, "part_size_bytes": a.Config.Limits.UploadPartBytes, "status": u.Status, "job_id": nonzero(u.JobID)})
}
func (a *Server) partURLs(w http.ResponseWriter, r *http.Request) error {
	u, e := a.upload(r)
	if e != nil {
		return e
	}
	if u.Status != "UPLOADING" || time.Now().After(u.ExpiresAt) {
		return domain.ErrConflict
	}
	var in struct {
		Numbers []int32 `json:"part_numbers"`
	}
	if e = decode(w, r, &in); e != nil {
		return e
	}
	if len(in.Numbers) < 1 || len(in.Numbers) > 10 {
		return domain.ErrInvalid
	}
	count := (u.Size + a.Config.Limits.UploadPartBytes - 1) / a.Config.Limits.UploadPartBytes
	out := []any{}
	seen := map[int32]bool{}
	for _, n := range in.Numbers {
		if n < 1 || int64(n) > count || seen[n] {
			return domain.ErrInvalid
		}
		seen[n] = true
		s, e := a.Storage.SignPart(r.Context(), u.ObjectKey, u.MultipartID, n, 15*time.Minute)
		if e != nil {
			return e
		}
		headers := map[string]string{}
		for k, v := range s.Headers {
			headers[k] = strings.Join(v, ",")
		}
		out = append(out, map[string]any{"part_number": n, "url": s.URL, "headers": headers, "expires_at": s.ExpiresAt})
	}
	return items(w, out)
}
func (a *Server) parts(w http.ResponseWriter, r *http.Request) error {
	u, e := a.upload(r)
	if e != nil {
		return e
	}
	if u.Status != "UPLOADING" {
		return domain.ErrConflict
	}
	ps, e := a.Storage.ListParts(r.Context(), u.ObjectKey, u.MultipartID)
	if e != nil {
		return e
	}
	out := []partDTO{}
	for _, p := range ps {
		out = append(out, partDTO{p.Number, p.ETag, p.Size})
	}
	return items(w, out)
}
func verifyParts(size, partSize int64, sent []partDTO, actual []storage.Part) error {
	if size <= 0 || partSize <= 0 {
		return domain.ErrInvalid
	}
	count := int((size + partSize - 1) / partSize)
	if len(sent) != count || len(actual) != count {
		return &failure{409, "UPLOAD_INCOMPLETE", "分片尚未全部上传"}
	}
	sort.Slice(actual, func(i, j int) bool { return actual[i].Number < actual[j].Number })
	sort.Slice(sent, func(i, j int) bool { return sent[i].Number < sent[j].Number })
	for i, p := range actual {
		want := partSize
		if i == count-1 {
			want = size - int64(i)*partSize
		}
		if strings.TrimSpace(strings.Trim(p.ETag, "\"")) == "" || p.Number != int32(i+1) || sent[i].Number != p.Number || p.Size != want || strings.Trim(p.ETag, "\"") != strings.Trim(sent[i].ETag, "\"") {
			return &failure{409, "UPLOAD_PART_MISMATCH", "分片大小或 ETag 不一致，请重新同步分片"}
		}
	}
	return nil
}
func (a *Server) completeUpload(w http.ResponseWriter, r *http.Request) error {
	u, e := a.upload(r)
	if e != nil {
		return e
	}
	if u.JobID != uuid.Nil {
		return reply(w, 202, map[string]any{"video_id": u.VideoID, "job_id": u.JobID})
	}
	if u.Status != "UPLOADING" {
		return domain.ErrConflict
	}
	var in struct {
		Parts []partDTO `json:"parts"`
	}
	if e = decode(w, r, &in); e != nil {
		return e
	}
	ps, e := a.Storage.ListParts(r.Context(), u.ObjectKey, u.MultipartID)
	var obj storage.ObjectInfo
	if e == nil {
		if e = verifyParts(u.Size, a.Config.Limits.UploadPartBytes, in.Parts, ps); e != nil {
			return e
		}
		obj, e = a.Storage.Complete(r.Context(), u.ObjectKey, u.MultipartID, ps)
	}
	// CompleteMultipartUpload can succeed remotely while its response is lost. Only the server-owned unique key is probed.
	if e != nil {
		obj, e = a.Storage.Head(r.Context(), u.ObjectKey)
		if e != nil {
			return e
		}
	}
	if obj.Size != u.Size {
		return &failure{409, "UPLOAD_SIZE_MISMATCH", "对象大小与上传声明不一致"}
	}
	j, e := a.Store.CompleteUpload(r.Context(), user(r).ID, u.ID, domain.Asset{ID: uuid.New(), Kind: "SOURCE", StorageBackend: "s3", ObjectKey: u.ObjectKey, ContentType: u.ContentType, Size: obj.Size, Checksum: obj.ETag}, a.pipelineConfig())
	if e != nil {
		return e
	}
	return reply(w, 202, map[string]any{"video_id": u.VideoID, "job_id": j.ID})
}
func (a *Server) abortUpload(w http.ResponseWriter, r *http.Request) error {
	u, e := a.upload(r)
	if e != nil {
		return e
	}
	if u.Status != "UPLOADING" {
		return domain.ErrConflict
	}
	if e = a.Store.AbortUpload(r.Context(), user(r).ID, u.ID); e != nil {
		return e
	}
	_ = a.Storage.Abort(r.Context(), u.ObjectKey, u.MultipartID)
	return reply(w, 200, map[string]bool{"ok": true})
}
