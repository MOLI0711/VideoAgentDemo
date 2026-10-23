package retrieval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/MOLI0711/MOLIVedio/internal/media"
	"github.com/MOLI0711/MOLIVedio/internal/pipeline"
	"github.com/MOLI0711/MOLIVedio/internal/provider"
	"github.com/MOLI0711/MOLIVedio/internal/retrieval"
	"github.com/MOLI0711/MOLIVedio/internal/storage"
)

// These explicit vectors test ES query behavior, not embedding model quality.
type integrationEmbedding struct{ values map[string][]float32 }

func (e *integrationEmbedding) Embed(_ context.Context, input []string) (provider.EmbeddingResponse, error) {
	out := provider.EmbeddingResponse{Model: "mock:es-fixture", Mock: true, Usage: provider.Usage{Mock: true}}
	for _, text := range input {
		v, ok := e.values[text]
		if !ok {
			return out, errors.New("unknown ES embedding fixture")
		}
		out.Vectors = append(out.Vectors, append([]float32(nil), v...))
	}
	return out, nil
}

type indexRepository struct {
	pipeline.Repository
	video    domain.Video
	segments []domain.Segment
}

func (r *indexRepository) GetVideo(_ context.Context, owner, id domain.ID) (*domain.Video, error) {
	if owner != r.video.OwnerID || id != r.video.ID {
		return nil, domain.ErrNotFound
	}
	return &r.video, nil
}
func (r *indexRepository) LoadSegments(context.Context, domain.ID) ([]domain.Segment, error) {
	return r.segments, nil
}

type unusedObjectStore struct{ storage.Store }
type unusedMediaEngine struct{ media.Engine }

func esRequest(t *testing.T, ctx context.Context, endpoint, method, path string, body []byte) ([]byte, int) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(endpoint, "/")+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return data, res.StatusCode
}

func TestElasticsearchIntegration(t *testing.T) {
	endpoint := os.Getenv("VA_TEST_ES_ENDPOINT")
	if endpoint == "" {
		t.Skip("set VA_TEST_ES_ENDPOINT for real Elasticsearch integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	index := "molivideo-integration-" + domain.NewID().String()
	model := &integrationEmbedding{values: map[string][]float32{"shared topic": {1, 0, 0, 0}}}
	client, err := retrieval.New(endpoint, "", 4, model)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.EnsureIndex(ctx, index); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, status := esRequest(t, context.Background(), endpoint, "DELETE", "/"+url.PathEscape(index), nil)
		if status != 200 && status != 404 {
			t.Errorf("remove only own index status=%d", status)
		}
	}()
	owner, video, version := domain.NewID(), domain.NewID(), domain.NewID()
	scope := retrieval.Scope{OwnerID: owner.String(), VideoID: video.String(), ContentVersion: version.String(), IndexName: index, IndexBuildID: "published-fixture"}
	doc := func(text string) retrieval.Document {
		model.values[text+"\n[OCR]\n"] = []float32{1, .1, .1, .1}
		return retrieval.Document{SegmentID: domain.NewID().String(), StartMS: 0, EndMS: 1000, Transcript: text, Title: "Explicit synthetic fixture", FrameIDs: []string{}}
	}
	own := []retrieval.Document{doc("shared topic alpha"), doc("shared topic beta")}
	if err = client.Build(ctx, scope, own); err != nil {
		t.Fatal(err)
	}
	foreign := []string{}
	for _, field := range []string{"owner", "video", "version", "build"} {
		s := scope
		switch field {
		case "owner":
			s.OwnerID = domain.NewID().String()
		case "video":
			s.VideoID = domain.NewID().String()
		case "version":
			s.ContentVersion = domain.NewID().String()
		case "build":
			s.IndexBuildID = "stale-fixture"
		}
		d := doc("shared topic foreign " + field)
		foreign = append(foreign, d.SegmentID)
		if err = client.Build(ctx, s, []retrieval.Document{d}); err != nil {
			t.Fatal(field, err)
		}
	}
	result, err := client.Search(ctx, scope, "shared topic", 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != "hybrid" || len(result.Documents) != 2 {
		t.Fatalf("hybrid filter failure: %+v", result)
	}
	allowed := map[string]bool{own[0].SegmentID: true, own[1].SegmentID: true}
	for _, d := range result.Documents {
		if !allowed[d.SegmentID] || d.OwnerID != scope.OwnerID || d.VideoID != scope.VideoID || d.ContentVersion != scope.ContentVersion || d.IndexBuildID != scope.IndexBuildID {
			t.Fatalf("foreign evidence returned: %+v", d)
		}
	}
	read, err := client.Read(ctx, scope, append(foreign, own[0].SegmentID))
	if err != nil || len(read) != 1 || read[0].SegmentID != own[0].SegmentID {
		t.Fatalf("read scope failure: %+v %v", read, err)
	}
	lexical, _ := retrieval.New(endpoint, "", 4, nil)
	fallback, err := lexical.Search(ctx, scope, "shared topic", 10)
	if err != nil || fallback.Mode != "lexical" || len(fallback.Documents) != 2 {
		t.Fatalf("lexical fallback lost filters: %+v %v", fallback, err)
	}
	incompatible, _ := retrieval.New(endpoint, "", 8, model)
	if err = incompatible.EnsureIndex(ctx, index); err == nil {
		t.Fatal("existing vector dimension mismatch accepted")
	}
	if err = client.EnsureIndex(ctx, index); err != nil {
		t.Fatal("matching existing mapping rejected", err)
	}

	// Force a real per-item 400: Lucene keyword terms cannot exceed 32766 bytes.
	// The first document succeeds, proving that a partial ES build is not atomic.
	partialTexts := []string{"partial valid", "partial invalid"}
	repo := &indexRepository{video: domain.Video{ID: video, OwnerID: owner, Title: "partial fixture"}}
	for i, text := range partialTexts {
		model.values[text+"\n[OCR]\n"] = []float32{1, .1, .1, .1}
		frames := []string{}
		if i == 1 {
			frames = []string{strings.Repeat("x", 40000)}
		}
		encoded, _ := json.Marshal(frames)
		repo.segments = append(repo.segments, domain.Segment{ID: domain.NewID(), VideoVersionID: version, StartMS: int64(i * 1000), EndMS: int64((i + 1) * 1000), Text: text, FrameIDs: encoded})
	}
	p, err := pipeline.New(pipeline.Deps{Repository: repo, Objects: &unusedObjectStore{}, Media: &unusedMediaEngine{}, Indexer: client, IndexName: index, EmbeddingProfile: "fixture-4", TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	lease := &domain.Lease{Job: domain.Job{ID: domain.NewID(), OwnerID: owner, VideoID: video, VideoVersionID: version, Config: json.RawMessage(`{"embedding_profile":"fixture-4"}`)}, Step: domain.Step{ID: domain.NewID(), StepType: "BUILD_INDEX"}, AttemptNo: 3}
	publication, err := p.BuildIndex(ctx, lease)
	if err == nil || publication.IndexBuildID != "" || publication.IndexName != "" {
		t.Fatalf("partial build became publishable: %+v err=%v", publication, err)
	}
	esRequest(t, ctx, endpoint, "POST", "/"+url.PathEscape(index)+"/_refresh", nil)
	partialScope := scope
	partialScope.IndexBuildID = lease.Step.ID.String() + "-3"
	query, _ := json.Marshal(map[string]any{"query": map[string]any{"bool": map[string]any{"filter": []any{map[string]any{"term": map[string]string{"index_build_id": partialScope.IndexBuildID}}}}}})
	body, status := esRequest(t, ctx, endpoint, "POST", "/"+url.PathEscape(index)+"/_count", query)
	var count struct {
		Count int `json:"count"`
	}
	if status != 200 || json.Unmarshal(body, &count) != nil || count.Count != 1 {
		t.Fatalf("fixture did not create expected partial failure: status=%d count=%d", status, count.Count)
	}
	visible, err := client.Search(ctx, scope, "shared topic", 10)
	if err != nil || len(visible.Documents) != 2 {
		t.Fatal("failed build polluted published scope", err)
	}
	t.Log("real ES verified: hybrid/lexical/read owner+video+version+build filters, dimensions, partial item failure and unpublished build isolation")
}
