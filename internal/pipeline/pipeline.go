// Package pipeline connects durable workflow leases to media and provider adapters.
// Returned results become visible only when the repository commits the same lease.
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/MOLI0711/MOLIVedio/internal/media"
	"github.com/MOLI0711/MOLIVedio/internal/provider"
	"github.com/MOLI0711/MOLIVedio/internal/retrieval"
	"github.com/MOLI0711/MOLIVedio/internal/storage"
	"github.com/MOLI0711/MOLIVedio/internal/workflow"
)

type Repository interface {
	GetVideo(context.Context, domain.ID, domain.ID) (*domain.Video, error)
	GetVersion(context.Context, domain.ID, domain.ID) (*domain.VideoVersion, error)
	GetAsset(context.Context, domain.ID, domain.ID) (*domain.Asset, error)
	ListAssets(context.Context, domain.ID, domain.ID, domain.ID, string) ([]domain.Asset, error)
	LoadSentences(context.Context, domain.ID) ([]domain.Sentence, error)
	LoadFrames(context.Context, domain.ID) ([]domain.Frame, error)
	LoadSegments(context.Context, domain.ID) ([]domain.Segment, error)
	BeginExternal(context.Context, domain.Lease, string, json.RawMessage) (*domain.ExternalJob, bool, error)
	GetExternalJob(context.Context, domain.ID) (*domain.ExternalJob, error)
}

type Indexer interface {
	Build(context.Context, retrieval.Scope, []retrieval.Document) error
}

type Deps struct {
	Repository       Repository
	Objects          storage.Store
	ASRStore         storage.Store // A cloud-reachable private staging bucket in live mode.
	Media            media.Engine
	ASR              provider.ASR
	Indexer          Indexer
	ASRMode          string
	ASRProviderRef   string
	TempDir          string
	IndexName        string
	EmbeddingProfile string
	MaxInputBytes    int64
	MaxDuration      time.Duration
	PollInterval     time.Duration
}

type Pipeline struct{ deps Deps }

type snapshot struct {
	ASRMode          string `json:"asr_mode"`
	IndexName        string `json:"index_name"`
	EmbeddingProfile string `json:"embedding_profile"`
	MaxDurationMS    int64  `json:"max_duration_ms"`
}

func New(deps Deps) (*Pipeline, error) {
	if deps.Repository == nil || deps.Objects == nil || deps.Media == nil {
		return nil, errors.New("pipeline requires repository, object store and media engine")
	}
	if deps.ASRMode == "" {
		deps.ASRMode = "disabled"
	}
	if deps.ASRMode != "disabled" && deps.ASRMode != "mock" && deps.ASRMode != "live" {
		return nil, errors.New("invalid ASR mode")
	}
	if deps.ASRMode != "disabled" && deps.ASR == nil {
		return nil, errors.New("enabled ASR requires an explicit adapter")
	}
	if deps.ASRMode == "live" && deps.ASRStore == nil {
		return nil, errors.New("live ASR requires a cloud-reachable ASR staging store")
	}
	if deps.ASRProviderRef == "" {
		deps.ASRProviderRef = deps.ASRMode
	}
	if deps.TempDir == "" {
		deps.TempDir = ".runtime/media"
	}
	if deps.MaxInputBytes == 0 {
		deps.MaxInputBytes = 2 << 30
	}
	if deps.MaxInputBytes < 1 {
		return nil, errors.New("invalid pipeline input limit")
	}
	if deps.MaxDuration == 0 {
		deps.MaxDuration = 2 * time.Hour
	}
	if deps.MaxDuration < time.Millisecond {
		return nil, errors.New("invalid pipeline duration limit")
	}
	if deps.PollInterval == 0 {
		deps.PollInterval = 5 * time.Second
	}
	if deps.PollInterval < time.Second || deps.PollInterval > time.Minute {
		return nil, errors.New("invalid external poll interval")
	}
	if err := os.MkdirAll(deps.TempDir, 0700); err != nil {
		return nil, err
	}
	return &Pipeline{deps: deps}, nil
}

func (p *Pipeline) Register(engine *workflow.Engine) {
	engine.Register("PROBE", p.Probe)
	engine.Register("EXTRACT_AUDIO", p.ExtractAudio)
	engine.Register("EXTRACT_FRAMES", p.ExtractFrames)
	engine.Register("ASR", p.Transcribe)
	engine.Register("OCR", p.OCR)
	engine.Register("BUILD_SEGMENTS", p.BuildSegments)
	engine.Register("BUILD_INDEX", p.BuildIndex)
}

func config(l *domain.Lease) (snapshot, error) {
	var s snapshot
	if len(l.Job.Config) > 0 && json.Unmarshal(l.Job.Config, &s) != nil {
		return s, fail("INVALID_PIPELINE_SNAPSHOT", nil)
	}
	return s, nil
}

func (p *Pipeline) workdir(l *domain.Lease) (string, error) {
	if l == nil || l.AttemptNo == 0 || l.Step.ID == (domain.ID{}) || l.Job.OwnerID == (domain.ID{}) || l.Job.VideoVersionID == (domain.ID{}) {
		return "", fail("INVALID_EXECUTION_LEASE", nil)
	}
	return os.MkdirTemp(p.deps.TempDir, fmt.Sprintf("%s-%d-", l.Step.ID, l.AttemptNo))
}

func prefix(l *domain.Lease) string {
	return fmt.Sprintf("owners/%s/videos/%s/versions/%s/steps/%s/attempts/%d/", l.Job.OwnerID, l.Job.VideoID, l.Job.VideoVersionID, l.Step.ID, l.AttemptNo)
}

func marshal(v any) json.RawMessage       { b, _ := json.Marshal(v); return b }
func fail(code string, cause error) error { return &workflow.ExecutionError{Code: code, Cause: cause} }

func execution(err error) error {
	if err == nil {
		return nil
	}
	var e *workflow.ExecutionError
	if errors.As(err, &e) {
		return err
	}
	var pe *provider.Error
	if errors.As(err, &pe) {
		if pe.SubmissionUnknown {
			return domain.ErrSubmissionUnknown
		}
		return &workflow.ExecutionError{Code: pe.Code, Cause: pe, Retryable: pe.Retryable, RetryAfter: 10 * time.Second}
	}
	var se *storage.Error
	if errors.As(err, &se) {
		return &workflow.ExecutionError{Code: se.Code, Cause: se, Retryable: se.Retryable}
	}
	var me *media.Error
	if errors.As(err, &me) {
		return fail(me.Code, me)
	}
	return fail("PIPELINE_ERROR", err)
}

// download trusts neither object metadata nor Content-Length. Only an authorized
// asset row determines which object is fetched, and the byte limit is enforced.
func (p *Pipeline) download(ctx context.Context, a domain.Asset, path string) error {
	if a.Size <= 0 || a.Size > p.deps.MaxInputBytes {
		return fail("ASSET_SIZE_INVALID", nil)
	}
	body, info, err := p.deps.Objects.Get(ctx, a.ObjectKey)
	if err != nil {
		return execution(err)
	}
	defer body.Close()
	if info.Size != a.Size {
		return fail("ASSET_SIZE_CHANGED", nil)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return execution(err)
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, a.Size+1))
	closeErr := f.Close()
	if copyErr != nil {
		return execution(copyErr)
	}
	if closeErr != nil {
		return execution(closeErr)
	}
	if n != a.Size {
		return fail("ASSET_SIZE_CHANGED", nil)
	}
	if strings.HasPrefix(a.Checksum, "sha256:") && a.Checksum != "sha256:"+hex.EncodeToString(h.Sum(nil)) {
		return fail("ASSET_CHECKSUM_MISMATCH", nil)
	}
	return ctx.Err()
}

func (p *Pipeline) source(ctx context.Context, l *domain.Lease, dir string) (string, error) {
	v, err := p.deps.Repository.GetVideo(ctx, l.Job.OwnerID, l.Job.VideoID)
	if err != nil {
		return "", execution(err)
	}
	a, err := p.deps.Repository.GetAsset(ctx, l.Job.OwnerID, v.SourceAssetID)
	if err != nil {
		return "", execution(err)
	}
	if a.OwnerID != l.Job.OwnerID || a.VideoID != l.Job.VideoID {
		return "", fail("ASSET_SCOPE_MISMATCH", nil)
	}
	path := filepath.Join(dir, "source.media")
	return path, p.download(ctx, *a, path)
}

func (p *Pipeline) upload(ctx context.Context, l *domain.Lease, path, filename, kind, contentType string, start, end int64, metadata json.RawMessage) (domain.Asset, error) {
	f, err := os.Open(path)
	if err != nil {
		return domain.Asset{}, execution(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return domain.Asset{}, execution(err)
	}
	if !st.Mode().IsRegular() || st.Size() <= 0 || st.Size() > p.deps.MaxInputBytes {
		return domain.Asset{}, fail("OUTPUT_SIZE_INVALID", nil)
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return domain.Asset{}, execution(err)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return domain.Asset{}, execution(err)
	}
	key := prefix(l) + filename
	info, err := p.deps.Objects.Put(ctx, key, contentType, f, st.Size())
	if err != nil {
		return domain.Asset{}, execution(err)
	}
	if info.Size != st.Size() {
		return domain.Asset{}, fail("STORED_SIZE_MISMATCH", nil)
	}
	return domain.Asset{ID: domain.NewID(), OwnerID: l.Job.OwnerID, VideoID: l.Job.VideoID, VideoVersionID: l.Job.VideoVersionID, StepID: l.Step.ID, AttemptNo: l.AttemptNo, Kind: kind, StorageBackend: "s3", ObjectKey: key, ContentType: contentType, Size: st.Size(), Checksum: "sha256:" + hex.EncodeToString(h.Sum(nil)), StartMS: start, EndMS: end, Metadata: metadata}, nil
}

func (p *Pipeline) Probe(ctx context.Context, l *domain.Lease) (domain.StepResult, error) {
	dir, err := p.workdir(l)
	if err != nil {
		return domain.StepResult{}, err
	}
	defer os.RemoveAll(dir)
	path, err := p.source(ctx, l, dir)
	if err != nil {
		return domain.StepResult{}, err
	}
	m, err := p.deps.Media.Probe(ctx, path)
	if err != nil {
		return domain.StepResult{}, execution(err)
	}
	s, err := config(l)
	if err != nil {
		return domain.StepResult{}, err
	}
	if m.DurationMS > p.deps.MaxDuration.Milliseconds() || (s.MaxDurationMS > 0 && m.DurationMS > s.MaxDurationMS) {
		return domain.StepResult{}, fail("VIDEO_DURATION_LIMIT", nil)
	}
	if !m.HasAudio {
		return domain.StepResult{}, fail("UNSUPPORTED_NO_SPEECH", nil)
	}
	return domain.StepResult{Metadata: marshal(m), DurationMS: m.DurationMS}, nil
}

func (p *Pipeline) ExtractAudio(ctx context.Context, l *domain.Lease) (domain.StepResult, error) {
	dir, err := p.workdir(l)
	if err != nil {
		return domain.StepResult{}, err
	}
	defer os.RemoveAll(dir)
	path, err := p.source(ctx, l, dir)
	if err != nil {
		return domain.StepResult{}, err
	}
	a, err := p.deps.Media.ExtractAudio(ctx, path, filepath.Join(dir, "audio.wav"))
	if err != nil {
		return domain.StepResult{}, execution(err)
	}
	meta := marshal(map[string]any{"sample_rate": a.SampleRate, "channels": a.Channels, "duration_ms": a.DurationMS, "start_ms": a.StartMS})
	asset, err := p.upload(ctx, l, a.Path, "audio.wav", "AUDIO", "audio/wav", a.StartMS, a.StartMS+a.DurationMS, meta)
	if err != nil {
		return domain.StepResult{}, err
	}
	return domain.StepResult{Assets: []domain.Asset{asset}, Metadata: meta}, nil
}

func (p *Pipeline) ExtractFrames(ctx context.Context, l *domain.Lease) (domain.StepResult, error) {
	dir, err := p.workdir(l)
	if err != nil {
		return domain.StepResult{}, err
	}
	defer os.RemoveAll(dir)
	path, err := p.source(ctx, l, dir)
	if err != nil {
		return domain.StepResult{}, err
	}
	frames, err := p.deps.Media.ExtractFrames(ctx, path, filepath.Join(dir, "frames"))
	if err != nil {
		return domain.StepResult{}, execution(err)
	}
	if len(frames) == 0 || len(frames) > 256 {
		return domain.StepResult{}, fail("INVALID_FRAME_COUNT", nil)
	}
	result := domain.StepResult{}
	for i, f := range frames {
		if f.TimestampMS < 0 {
			return domain.StepResult{}, fail("INVALID_FRAME_PTS", nil)
		}
		a, err := p.upload(ctx, l, f.Path, fmt.Sprintf("frame-%06d.jpg", i+1), "FRAME", "image/jpeg", f.TimestampMS, f.TimestampMS, marshal(map[string]any{"timestamp_ms": f.TimestampMS}))
		if err != nil {
			return domain.StepResult{}, err
		}
		result.Assets = append(result.Assets, a)
		result.Frames = append(result.Frames, domain.Frame{ID: domain.NewID(), VideoVersionID: l.Job.VideoVersionID, AssetID: a.ID, TimestampMS: f.TimestampMS, OCRStatus: "PENDING"})
	}
	result.Metadata = marshal(map[string]any{"frame_count": len(frames), "timestamp_source": "decoded_pts"})
	return result, nil
}
