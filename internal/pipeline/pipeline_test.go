package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/MOLI0711/MOLIVedio/internal/media"
	"github.com/MOLI0711/MOLIVedio/internal/provider"
	"github.com/MOLI0711/MOLIVedio/internal/retrieval"
	"github.com/MOLI0711/MOLIVedio/internal/storage"
	"github.com/MOLI0711/MOLIVedio/internal/workflow"
)

type memoryObjects struct {
	storage.Store
	data    map[string][]byte
	puts    []string
	signURL string
}

func (m *memoryObjects) Get(_ context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	b, ok := m.data[key]
	if !ok {
		return nil, storage.ObjectInfo{}, domain.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), storage.ObjectInfo{Key: key, Size: int64(len(b))}, nil
}
func (m *memoryObjects) Put(_ context.Context, key, typ string, body io.Reader, size int64) (storage.ObjectInfo, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if int64(len(b)) != size {
		return storage.ObjectInfo{}, errors.New("bad size")
	}
	m.data[key] = b
	m.puts = append(m.puts, key)
	return storage.ObjectInfo{Key: key, ContentType: typ, Size: size}, nil
}
func (m *memoryObjects) SignGet(_ context.Context, key string, _ time.Duration) (storage.SignedURL, error) {
	u := m.signURL
	if u == "" {
		u = "https://fixtures.example/" + key
	}
	return storage.SignedURL{URL: u}, nil
}

type memoryRepo struct {
	Repository
	video     domain.Video
	version   domain.VideoVersion
	assets    map[domain.ID]domain.Asset
	sentences []domain.Sentence
	frames    []domain.Frame
	segments  []domain.Segment
	external  *domain.ExternalJob
}

func (m *memoryRepo) GetVideo(_ context.Context, owner, id domain.ID) (*domain.Video, error) {
	if owner != m.video.OwnerID || id != m.video.ID {
		return nil, domain.ErrNotFound
	}
	return &m.video, nil
}
func (m *memoryRepo) GetVersion(_ context.Context, owner, id domain.ID) (*domain.VideoVersion, error) {
	if owner != m.video.OwnerID || id != m.version.ID {
		return nil, domain.ErrNotFound
	}
	return &m.version, nil
}
func (m *memoryRepo) GetAsset(_ context.Context, owner, id domain.ID) (*domain.Asset, error) {
	a, ok := m.assets[id]
	if !ok || a.OwnerID != owner {
		return nil, domain.ErrNotFound
	}
	return &a, nil
}
func (m *memoryRepo) ListAssets(_ context.Context, owner, video, version domain.ID, kind string) ([]domain.Asset, error) {
	out := []domain.Asset{}
	for _, a := range m.assets {
		if a.OwnerID == owner && a.VideoID == video && a.VideoVersionID == version && a.Kind == kind {
			out = append(out, a)
		}
	}
	return out, nil
}
func (m *memoryRepo) LoadSentences(context.Context, domain.ID) ([]domain.Sentence, error) {
	return append([]domain.Sentence(nil), m.sentences...), nil
}
func (m *memoryRepo) LoadFrames(context.Context, domain.ID) ([]domain.Frame, error) {
	return append([]domain.Frame(nil), m.frames...), nil
}
func (m *memoryRepo) LoadSegments(context.Context, domain.ID) ([]domain.Segment, error) {
	return append([]domain.Segment(nil), m.segments...), nil
}
func (m *memoryRepo) BeginExternal(_ context.Context, l domain.Lease, ref string, request json.RawMessage) (*domain.ExternalJob, bool, error) {
	if m.external != nil {
		return m.external, false, nil
	}
	m.external = &domain.ExternalJob{ID: domain.NewID(), StepID: l.Step.ID, ProviderRef: ref, Status: "SUBMITTING", ClientRequestID: "audit-id", Request: request, DeadlineAt: time.Now().Add(time.Hour)}
	return m.external, true, nil
}
func (m *memoryRepo) GetExternalJob(_ context.Context, id domain.ID) (*domain.ExternalJob, error) {
	if m.external == nil || m.external.ID != id {
		return nil, domain.ErrNotFound
	}
	return m.external, nil
}
func (m *memoryRepo) apply(result domain.StepResult) {
	for _, a := range result.Assets {
		m.assets[a.ID] = a
	}
	if len(result.Sentences) > 0 {
		m.sentences = result.Sentences
	}
	if len(result.Frames) > 0 {
		m.frames = result.Frames
	}
	if len(result.Segments) > 0 {
		m.segments = result.Segments
	}
	if result.DurationMS > 0 {
		m.version.Metadata = result.Metadata
		m.video.DurationMS = result.DurationMS
	}
	if result.ExternalWait != nil {
		m.external.ProviderJobID = result.ExternalWait.ProviderJobID
		m.external.Status = "PENDING"
	}
}

type fixtureMedia struct {
	media.Engine
	ocrError error
}

func (*fixtureMedia) Probe(context.Context, string) (media.Metadata, error) {
	return media.Metadata{DurationMS: 6000, HasAudio: true, StartMS: 10000, AudioStartMS: 250}, nil
}
func (*fixtureMedia) ExtractAudio(_ context.Context, _ string, out string) (media.Audio, error) {
	err := os.WriteFile(out, []byte("synthetic wav fixture"), 0600)
	return media.Audio{Path: out, StartMS: 250, DurationMS: 5000, Size: 21, SampleRate: 16000, Channels: 1}, err
}
func (*fixtureMedia) ExtractFrames(_ context.Context, _ string, dir string) ([]media.Frame, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	out := []media.Frame{}
	for i, pts := range []int64{500, 3500} {
		path := filepath.Join(dir, string(rune('a'+i))+".jpg")
		if err := os.WriteFile(path, []byte("synthetic image fixture"), 0600); err != nil {
			return nil, err
		}
		out = append(out, media.Frame{Path: path, TimestampMS: pts})
	}
	return out, nil
}
func (m *fixtureMedia) OCR(context.Context, string) (media.OCRResult, error) {
	return media.OCRResult{Text: "Synthetic slide 42", Language: "eng"}, m.ocrError
}

type fixtureASR struct {
	provider.ASR
	submits, polls int
	submitError    error
	pollErrors     []error
	responses      []provider.ASRResult
	last           provider.ASRRequest
}

func (a *fixtureASR) Submit(_ context.Context, r provider.ASRRequest) (provider.ASRSubmission, error) {
	a.submits++
	a.last = r
	return provider.ASRSubmission{JobID: "mock-fixture", Mock: true}, a.submitError
}
func (a *fixtureASR) Poll(_ context.Context, id string) (provider.ASRResult, error) {
	n := a.polls
	a.polls++
	if n < len(a.pollErrors) && a.pollErrors[n] != nil {
		return provider.ASRResult{}, a.pollErrors[n]
	}
	r := a.responses[min(n, len(a.responses)-1)]
	r.JobID = id
	r.Mock = true
	return r, nil
}

type fixtureIndex struct {
	scope retrieval.Scope
	docs  []retrieval.Document
	err   error
}

func (i *fixtureIndex) Build(_ context.Context, s retrieval.Scope, d []retrieval.Document) error {
	i.scope = s
	i.docs = d
	return i.err
}

func setup(t *testing.T) (*Pipeline, *memoryRepo, *memoryObjects, *fixtureASR, *fixtureIndex, *domain.Lease) {
	t.Helper()
	owner, video, version, source := domain.NewID(), domain.NewID(), domain.NewID(), domain.NewID()
	repo := &memoryRepo{video: domain.Video{ID: video, OwnerID: owner, SourceAssetID: source, Title: "Synthetic fixture"}, version: domain.VideoVersion{ID: version, VideoID: video}, assets: map[domain.ID]domain.Asset{source: {ID: source, OwnerID: owner, VideoID: video, Kind: "SOURCE", ObjectKey: "source", Size: 14}}}
	objects := &memoryObjects{data: map[string][]byte{"source": []byte("synthetic file")}}
	asr := &fixtureASR{responses: []provider.ASRResult{{Status: provider.ASRSucceeded, Sentences: []provider.Sentence{{StartMS: 0, EndMS: 1000, Text: "Fixture alpha"}, {StartMS: 2000, EndMS: 4500, Text: "Fixture beta"}}}}}
	index := &fixtureIndex{}
	p, err := New(Deps{Repository: repo, Objects: objects, Media: &fixtureMedia{}, ASR: asr, ASRMode: "mock", ASRProviderRef: "fixture", Indexer: index, TempDir: t.TempDir(), IndexName: "fixture-index", EmbeddingProfile: "fixture-embed"})
	if err != nil {
		t.Fatal(err)
	}
	l := &domain.Lease{Job: domain.Job{ID: domain.NewID(), OwnerID: owner, VideoID: video, VideoVersionID: version, Config: marshal(map[string]any{"asr_mode": "mock", "index_name": "fixture-index", "embedding_profile": "fixture-embed", "max_duration_ms": 10000})}, Step: domain.Step{ID: domain.NewID()}, AttemptNo: 2}
	return p, repo, objects, asr, index, l
}

func TestVideoPipelineConnectsEvidenceAndAttemptScopedAssets(t *testing.T) {
	p, repo, objects, asr, index, l := setup(t)
	ctx := context.Background()
	for _, stage := range []struct {
		name string
		run  workflow.Handler
	}{{"PROBE", p.Probe}, {"EXTRACT_AUDIO", p.ExtractAudio}, {"EXTRACT_FRAMES", p.ExtractFrames}} {
		l.Step.ID = domain.NewID()
		l.Step.StepType = stage.name
		r, err := stage.run(ctx, l)
		if err != nil {
			t.Fatalf("%s: %v", stage.name, err)
		}
		for _, a := range r.Assets {
			if !strings.HasPrefix(a.ObjectKey, prefix(l)) {
				t.Fatalf("asset escaped attempt: %s", a.ObjectKey)
			}
		}
		repo.apply(r)
	}
	l.Step.ID = domain.NewID()
	l.Step.StepType = "ASR"
	wait, err := p.Transcribe(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	if wait.ExternalWait == nil {
		t.Fatal("submission did not release worker for polling")
	}
	repo.apply(wait)
	l.Step.CurrentExternalJobID = repo.external.ID
	l.AttemptNo++
	r, err := p.Transcribe(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	repo.apply(r)
	if asr.submits != 1 || asr.polls != 1 || len(asr.last.AudioSHA256) != 64 {
		t.Fatalf("wrong provider calls: %+v", asr)
	}
	if repo.sentences[0].StartMS != 250 || repo.sentences[1].EndMS != 4750 {
		t.Fatalf("audio timeline offset lost: %+v", repo.sentences)
	}
	if strings.Contains(string(repo.external.Request), "https://") {
		t.Fatal("signed bearer URL persisted")
	}
	l.Step.ID = domain.NewID()
	l.Step.CurrentExternalJobID = domain.ID{}
	l.AttemptNo = 2
	r, err = p.OCR(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	repo.apply(r)
	r, err = p.BuildSegments(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	repo.apply(r)
	if len(repo.segments) != 1 || !strings.Contains(repo.segments[0].Text, "Fixture beta") || repo.segments[0].OCRText != "Synthetic slide 42" {
		t.Fatalf("bad segment evidence: %+v", repo.segments)
	}
	var ids []string
	if err = json.Unmarshal(repo.segments[0].FrameIDs, &ids); err != nil || len(ids) != 2 {
		t.Fatal("frame references missing", err)
	}
	l.Step.ID = domain.NewID()
	r, err = p.BuildIndex(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	if r.IndexBuildID != l.Step.ID.String()+"-2" || index.scope.OwnerID != repo.video.OwnerID.String() || len(index.docs) != 1 {
		t.Fatalf("unsafe index build: %+v %+v", r, index)
	}
	if len(objects.puts) != 4 {
		t.Fatalf("expected audio, two frames, transcript: %v", objects.puts)
	}
	entries, err := os.ReadDir(p.deps.TempDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("attempt temp files leaked", err)
	}
}

func prepareASR(t *testing.T) (*Pipeline, *memoryRepo, *fixtureASR, *domain.Lease) {
	t.Helper()
	p, r, _, a, _, l := setup(t)
	r.version.Metadata = marshal(map[string]int{"duration_ms": 6000})
	id := domain.NewID()
	r.assets[id] = domain.Asset{ID: id, OwnerID: l.Job.OwnerID, VideoID: l.Job.VideoID, VideoVersionID: l.Job.VideoVersionID, Kind: "AUDIO", ObjectKey: "audio", Size: 20, StartMS: 250, EndMS: 5250}
	return p, r, a, l
}

func TestASRPollTransientAndPendingNeverResubmit(t *testing.T) {
	p, r, a, l := prepareASR(t)
	a.pollErrors = []error{&provider.Error{Code: "RATE_LIMITED", Retryable: true}, nil, nil}
	a.responses = []provider.ASRResult{{}, {Status: provider.ASRRunning}, {Status: provider.ASRSucceeded, Sentences: []provider.Sentence{{StartMS: 0, EndMS: 1000, Text: "fixture"}}}}
	wait, err := p.Transcribe(context.Background(), l)
	if err != nil {
		t.Fatal(err)
	}
	r.apply(wait)
	l.Step.CurrentExternalJobID = r.external.ID
	for i := 0; i < 2; i++ {
		l.AttemptNo++
		out, err := p.Transcribe(context.Background(), l)
		if err != nil || out.ExternalWait == nil {
			t.Fatalf("poll must wait without failure retry: %+v %v", out, err)
		}
	}
	l.AttemptNo++
	out, err := p.Transcribe(context.Background(), l)
	if err != nil || len(out.Sentences) != 1 {
		t.Fatal("poll did not complete", err)
	}
	if a.submits != 1 || a.polls != 3 || l.Step.RetryCount != 0 {
		t.Fatal("provider resubmitted or consumed failure retries")
	}
}

func TestASRUnknownCannotResubmit(t *testing.T) {
	for _, status := range []string{"SUBMITTING", "SUBMISSION_UNKNOWN", "UNKNOWN"} {
		t.Run(status, func(t *testing.T) {
			p, r, a, l := prepareASR(t)
			r.external = &domain.ExternalJob{ID: domain.NewID(), StepID: l.Step.ID, ProviderRef: "fixture", Status: status}
			l.Step.CurrentExternalJobID = r.external.ID
			_, err := p.Transcribe(context.Background(), l)
			if !errors.Is(err, domain.ErrSubmissionUnknown) || a.submits != 0 || a.polls != 0 {
				t.Fatalf("unknown submission retried: %v", err)
			}
		})
	}
	p, _, a, l := prepareASR(t)
	a.submitError = &provider.Error{Code: "SUBMISSION_UNKNOWN", SubmissionUnknown: true}
	_, err := p.Transcribe(context.Background(), l)
	if !errors.Is(err, domain.ErrSubmissionUnknown) || a.submits != 1 {
		t.Fatal(err)
	}
}

func TestASRRejectsOutsideVideoTimestamps(t *testing.T) {
	p, r, a, l := prepareASR(t)
	r.external = &domain.ExternalJob{ID: domain.NewID(), StepID: l.Step.ID, ProviderRef: "fixture", Status: "PENDING", ProviderJobID: "mock-fixture"}
	l.Step.CurrentExternalJobID = r.external.ID
	a.responses = []provider.ASRResult{{Status: provider.ASRSucceeded, Sentences: []provider.Sentence{{StartMS: 0, EndMS: 8000, Text: "bad"}}}}
	_, err := p.Transcribe(context.Background(), l)
	var e *workflow.ExecutionError
	if !errors.As(err, &e) || e.Code != "ASR_TIMESTAMP_OUT_OF_RANGE" {
		t.Fatal(err)
	}
}

func TestLiveRequiresRemoteStagingAndRejectsLoopback(t *testing.T) {
	p, _, _, _, _, _ := setup(t)
	d := p.deps
	d.ASRMode = "live"
	if _, err := New(d); err == nil {
		t.Fatal("live ASR accepted missing stage store")
	}
	for _, raw := range []string{"http://bucket.example/audio", "https://localhost/audio", "https://127.0.0.1/audio", "https://192.168.1.3/audio"} {
		if cloudURL(raw) {
			t.Fatalf("accepted %s", raw)
		}
	}
	if !cloudURL("https://bucket.cos.ap-shanghai.myqcloud.com/audio?signature=fixture") {
		t.Fatal("valid cloud URL rejected")
	}
}

func TestOCRDegradesExplicitlyAndEnforcesOwnership(t *testing.T) {
	p, r, _, _, _, l := setup(t)
	out, err := p.ExtractFrames(context.Background(), l)
	if err != nil {
		t.Fatal(err)
	}
	r.apply(out)
	p.deps.Media = &fixtureMedia{ocrError: errors.New("fixture OCR failure")}
	out, err = p.OCR(context.Background(), l)
	if err != nil || !out.Skipped || out.Frames[0].OCRStatus != "FAILED" {
		t.Fatalf("OCR degradation not explicit: %+v %v", out, err)
	}
	a := r.assets[r.frames[0].AssetID]
	a.VideoVersionID = domain.NewID()
	r.assets[a.ID] = a
	_, err = p.OCR(context.Background(), l)
	if err == nil {
		t.Fatal("foreign version frame accepted")
	}
}

func TestSegmentsPreserveEvidenceAndSplitGaps(t *testing.T) {
	version := domain.NewID()
	sentences := []domain.Sentence{{ID: domain.NewID(), VideoVersionID: version, StartMS: 0, EndMS: 1000, Text: "first"}, {ID: domain.NewID(), VideoVersionID: version, StartMS: 30000, EndMS: 33000, Text: "second"}}
	frame := domain.Frame{ID: domain.NewID(), VideoVersionID: version, TimestampMS: 31000, OCRText: "must not appear", OCRStatus: "FAILED"}
	segments, err := Segment(version, 40000, sentences, []domain.Frame{frame})
	if err != nil || len(segments) != 2 || segments[1].StartMS != 30000 || segments[1].OCRText != "" {
		t.Fatalf("bad segments: %+v %v", segments, err)
	}
	sentences[1].VideoVersionID = domain.NewID()
	if _, err = Segment(version, 40000, sentences, nil); err == nil {
		t.Fatal("foreign version sentence accepted")
	}
}

func TestIndexFailureNeverReturnsPublishableBuild(t *testing.T) {
	p, r, _, _, index, l := setup(t)
	r.segments = []domain.Segment{{ID: domain.NewID(), VideoVersionID: l.Job.VideoVersionID, StartMS: 0, EndMS: 1000, Text: "fixture", FrameIDs: json.RawMessage("[]")}}
	index.err = errors.New("partial bulk failure")
	out, err := p.BuildIndex(context.Background(), l)
	if err == nil || out.IndexBuildID != "" {
		t.Fatalf("failed index became publishable: %+v %v", out, err)
	}
	l.Job.Config = marshal(map[string]string{"embedding_profile": "changed"})
	index.docs = nil
	_, err = p.BuildIndex(context.Background(), l)
	if err == nil || index.docs != nil {
		t.Fatal("embedding snapshot mismatch accepted")
	}
}

func TestShortSpeechKeepsPrecedingVisualEvidence(t *testing.T) {
	version := domain.NewID()
	sentence := domain.Sentence{ID: domain.NewID(), VideoVersionID: version, StartMS: 500, EndMS: 2000, Text: "synthetic spoken introduction"}
	frame := domain.Frame{ID: domain.NewID(), VideoVersionID: version, TimestampMS: 0, OCRStatus: "SUCCEEDED", OCRText: "synthetic title"}
	segments, err := Segment(version, 3000, []domain.Sentence{sentence}, []domain.Frame{frame})
	if err != nil || len(segments) != 1 || segments[0].StartMS != 0 || segments[0].OCRText != "synthetic title" || !strings.Contains(string(segments[0].FrameIDs), frame.ID.String()) {
		t.Fatalf("visual context lost: %+v %v", segments, err)
	}
	if sentence.StartMS != 500 {
		t.Fatal("source sentence timestamp modified")
	}
}
