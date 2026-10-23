//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/config"
	"github.com/MOLI0711/MOLIVedio/internal/domain"
	db "github.com/MOLI0711/MOLIVedio/internal/repository/mysql"
	"github.com/MOLI0711/MOLIVedio/internal/storage"
	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type objectsFixture struct {
	mu            sync.Mutex
	completed     map[string]bool
	completeCalls int
	createCalls   int
	abortCalls    int
}

func (s *objectsFixture) CreateMultipart(context.Context, string, string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	return domain.NewID().String(), nil
}
func (*objectsFixture) SignPart(context.Context, string, string, int32, time.Duration) (storage.SignedURL, error) {
	return storage.SignedURL{URL: "https://fixture.invalid/part", Method: "PUT"}, nil
}
func (s *objectsFixture) ListParts(_ context.Context, key, _ string) ([]storage.Part, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.completed[key] {
		return nil, &storage.Error{Code: "NoSuchUpload"}
	}
	return []storage.Part{{Number: 1, ETag: "fixture-etag", Size: 12}}, nil
}
func (s *objectsFixture) Complete(_ context.Context, key, _ string, _ []storage.Part) (storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completeCalls++
	s.completed[key] = true
	return storage.ObjectInfo{Key: key, Size: 12, ETag: "fixture-etag"}, nil
}
func (s *objectsFixture) Abort(context.Context, string, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.abortCalls++
	return nil
}
func (s *objectsFixture) Head(_ context.Context, key string) (storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.completed[key] {
		return storage.ObjectInfo{}, &storage.Error{Code: "NotFound"}
	}
	return storage.ObjectInfo{Key: key, Size: 12, ETag: "fixture-etag"}, nil
}
func (*objectsFixture) Put(context.Context, string, string, io.Reader, int64) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, fmt.Errorf("unexpected fixture Put")
}
func (*objectsFixture) Get(context.Context, string) (io.ReadCloser, storage.ObjectInfo, error) {
	return nil, storage.ObjectInfo{}, fmt.Errorf("unexpected fixture Get")
}
func (*objectsFixture) SignGet(context.Context, string, time.Duration) (storage.SignedURL, error) {
	return storage.SignedURL{URL: "https://fixture.invalid/object", ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func apiStore(t *testing.T) *db.Store {
	t.Helper()
	dsn := os.Getenv("VA_MYSQL_TEST_ADMIN_DSN")
	if dsn == "" {
		t.Skip("set VA_MYSQL_TEST_ADMIN_DSN for real MySQL HTTP tests")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	cfg.DBName = ""
	admin, err := sqlx.Connect("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	name := "molivideo_test_api_" + strings.ReplaceAll(domain.NewID().String(), "-", "")
	if _, err = admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal(err)
	}
	cfg.DBName = name
	s, err := db.Open(context.Background(), cfg.FormatDSN())
	if err != nil {
		admin.Exec("DROP DATABASE `" + name + "`")
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		if _, err := admin.Exec("DROP DATABASE `" + name + "`"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	if err = s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

type browserIdentity struct {
	cookie *http.Cookie
	csrf   string
}

func apiCall(a *Server, who browserIdentity, method, path string, body any, key string) *httptest.ResponseRecorder {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", a.Config.App.PublicOrigin)
	if who.cookie != nil {
		r.AddCookie(who.cookie)
	}
	if who.csrf != "" {
		r.Header.Set("X-CSRF-Token", who.csrf)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}
func expectStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP %d want %d: %s", w.Code, status, w.Body.String())
	}
}
func decodeResult(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func browserGuest(t *testing.T, a *Server) (browserIdentity, *domain.User) {
	t.Helper()
	w := apiCall(a, browserIdentity{}, "POST", "/v1/auth/guest", map[string]any{}, "")
	expectStatus(t, w, 200)
	var payload struct {
		User domain.User `json:"user"`
		CSRF string      `json:"csrf_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil || !payload.User.IsGuest || payload.CSRF == "" {
		t.Fatalf("invalid guest bootstrap: %s error=%v", w.Body.String(), err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].Name != "va_session" {
		t.Fatal("guest bootstrap did not set one HttpOnly session cookie")
	}
	if strings.Contains(w.Body.String(), "password_hash") || strings.Contains(w.Body.String(), "!guest-session-only") {
		t.Fatal("guest bootstrap exposed credential storage")
	}
	return browserIdentity{cookie: cookies[0], csrf: payload.CSRF}, &payload.User
}

func finishFixturePipeline(t *testing.T, s *db.Store, job *domain.Job) {
	t.Helper()
	ctx := context.Background()
	for _, typ := range []string{"PROBE", "EXTRACT_AUDIO", "EXTRACT_FRAMES", "ASR", "OCR", "BUILD_SEGMENTS", "BUILD_INDEX"} {
		if _, err := s.DispatchLocal(ctx, 100); err != nil {
			t.Fatal(err)
		}
		kind := "media"
		if typ == "BUILD_INDEX" {
			kind = "agent"
		}
		l, err := s.ClaimReadyTypes(ctx, kind, "http-fixture", time.Minute, []string{typ})
		if err != nil {
			t.Fatal(err)
		}
		if l.Job.ID != job.ID {
			t.Fatal("fixture claimed unexpected task")
		}
		result := domain.StepResult{}
		if typ == "BUILD_INDEX" {
			result.IndexName = "fixture-index"
			result.IndexBuildID = domain.NewID().String()
		}
		if err = s.CompleteStep(ctx, *l, result); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHTTPMySQLSecurityAndTaskLifecycle(t *testing.T) {
	store := apiStore(t)
	ctx := context.Background()
	objects := &objectsFixture{completed: map[string]bool{}}
	a := New(config.Default(), store, objects, nil)
	anon := browserIdentity{}
	expectStatus(t, apiCall(a, anon, "GET", "/v1/videos", nil, ""), 401)
	aliceBrowser, alice := browserGuest(t, a)
	bobBrowser, bob := browserGuest(t, a)
	if alice.ID == bob.ID || aliceBrowser.cookie.Value == bobBrowser.cookie.Value {
		t.Fatal("independent visitors share an identity")
	}
	for _, who := range []browserIdentity{aliceBrowser, bobBrowser} {
		w := apiCall(a, who, "GET", "/v1/videos", nil, "")
		expectStatus(t, w, 200)
		if len(decodeResult(t, w)["items"].([]any)) != 0 {
			t.Fatal("new guest received another owner's or demo videos")
		}
		w = apiCall(a, who, "GET", "/v1/agents", nil, "")
		expectStatus(t, w, 200)
		items := decodeResult(t, w)["items"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["name"] != "视频助手" || items[0].(map[string]any)["status"] != "ACTIVE" {
			t.Fatal("guest did not receive one usable default assistant")
		}
	}
	aliceAgents, err := store.ListAgents(ctx, alice.ID)
	if err != nil || len(aliceAgents) != 1 {
		t.Fatal("missing guest assistant", err)
	}
	agent := aliceAgents[0]
	versions, err := store.ListAgentVersions(ctx, alice.ID, agent.ID)
	if err != nil || len(versions) != 1 || versions[0].Status != "PUBLISHED" || versions[0].ID != agent.ActiveVersionID {
		t.Fatal("default assistant has no published active version", err)
	}
	expectStatus(t, apiCall(a, bobBrowser, "GET", "/v1/agents/"+agent.ID.String()+"/versions", nil, ""), 404)
	reused := apiCall(a, aliceBrowser, "POST", "/v1/auth/guest", map[string]any{}, "")
	expectStatus(t, reused, 200)
	if decodeResult(t, reused)["user"].(map[string]any)["id"] != alice.ID.String() || decodeResult(t, reused)["csrf_token"] != aliceBrowser.csrf {
		t.Fatal("refresh replaced a valid guest owner")
	}
	var users int
	if err = store.DB.Get(&users, "SELECT COUNT(*) FROM users"); err != nil || users != 2 {
		t.Fatal("repeated guest bootstrap created another owner", err)
	}
	request := map[string]any{"title": "Lecture", "filename": "lecture.mp4", "content_type": "video/mp4", "size_bytes": 12}
	missingCSRF := aliceBrowser
	missingCSRF.csrf = ""
	expectStatus(t, apiCall(a, missingCSRF, "POST", "/v1/uploads", request, "upload-key-1"), 403)
	if objects.createCalls != 0 {
		t.Fatal("CSRF failure reached external storage")
	}
	w := apiCall(a, aliceBrowser, "POST", "/v1/uploads", request, "upload-key-1")
	expectStatus(t, w, 201)
	created := decodeResult(t, w)
	uploadID := created["upload_id"].(string)
	videoID := created["video_id"].(string)
	w = apiCall(a, aliceBrowser, "POST", "/v1/uploads", request, "upload-key-1")
	expectStatus(t, w, 201)
	if decodeResult(t, w)["upload_id"] != uploadID {
		t.Fatal("repeated HTTP upload created a different resource")
	}
	if objects.abortCalls != 1 {
		t.Fatal("unused duplicate multipart not cleaned")
	}
	expectStatus(t, apiCall(a, bobBrowser, "GET", "/v1/uploads/"+uploadID+"/parts", nil, ""), 404)
	parts := map[string]any{"parts": []map[string]any{{"part_number": 1, "etag": "fixture-etag", "size_bytes": 12}}}
	w = apiCall(a, aliceBrowser, "POST", "/v1/uploads/"+uploadID+"/complete", parts, "")
	expectStatus(t, w, 202)
	jobID := decodeResult(t, w)["job_id"].(string)
	w = apiCall(a, aliceBrowser, "POST", "/v1/uploads/"+uploadID+"/complete", parts, "")
	expectStatus(t, w, 202)
	if decodeResult(t, w)["job_id"] != jobID || objects.completeCalls != 1 {
		t.Fatal("completion replay repeated remote merge or task creation")
	}
	expectStatus(t, apiCall(a, bobBrowser, "GET", "/v1/videos/"+videoID, nil, ""), 404)
	expectStatus(t, apiCall(a, bobBrowser, "GET", "/v1/jobs/"+jobID, nil, ""), 404)
	expectStatus(t, apiCall(a, bobBrowser, "POST", "/v1/jobs/"+jobID+"/cancel", map[string]any{}, ""), 404)
	vid, err := parseFixtureID(videoID)
	if err != nil {
		t.Fatal(err)
	}
	jobUUID, err := parseFixtureID(jobID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.GetJob(ctx, alice.ID, jobUUID)
	if err != nil {
		t.Fatal(err)
	}
	finishFixturePipeline(t, store, job)
	otherUpload, err := store.CreateUpload(ctx, bob.ID, domain.CreateUpload{Title: "Other", Filename: "b.mp4", Size: 12, MultipartID: "other", ObjectKey: "fixture/other", ContentType: "video/mp4"})
	if err != nil {
		t.Fatal(err)
	}
	otherJob, err := store.CompleteUpload(ctx, bob.ID, otherUpload.ID, domain.Asset{ObjectKey: otherUpload.ObjectKey, Size: 12}, nil)
	if err != nil {
		t.Fatal(err)
	}
	finishFixturePipeline(t, store, otherJob)
	expectStatus(t, apiCall(a, aliceBrowser, "GET", "/v1/videos/"+videoID+"/segments?content_version="+otherJob.VideoVersionID.String(), nil, ""), 404)
	sessionBody := map[string]any{"video_id": videoID, "agent_version_id": versions[0].ID.String(), "content_version": otherJob.VideoVersionID.String()}
	expectStatus(t, apiCall(a, aliceBrowser, "POST", "/v1/sessions", sessionBody, ""), 404)
	sessionBody["content_version"] = job.VideoVersionID.String()
	w = apiCall(a, aliceBrowser, "POST", "/v1/sessions", sessionBody, "")
	expectStatus(t, w, 201)
	sessionID := decodeResult(t, w)["id"].(string)
	newJob, err := store.Reprocess(ctx, alice.ID, vid, "reprocess-new-version", nil)
	if err != nil {
		t.Fatal(err)
	}
	finishFixturePipeline(t, store, newJob)
	w = apiCall(a, aliceBrowser, "POST", "/v1/sessions/"+sessionID+"/runs", map[string]string{"question": "Explain the lecture"}, "run-key-1")
	expectStatus(t, w, 202)
	runBody := decodeResult(t, w)
	if runBody["content_version"] != job.VideoVersionID.String() {
		t.Fatal("old session switched to newest video version")
	}
	runID := runBody["run_id"].(string)
	expectStatus(t, apiCall(a, bobBrowser, "GET", "/v1/runs/"+runID, nil, ""), 404)
	w = apiCall(a, aliceBrowser, "POST", "/v1/videos/"+videoID+"/summaries", map[string]string{"content_version": job.VideoVersionID.String(), "agent_version_id": versions[0].ID.String()}, "summary-key-1")
	expectStatus(t, w, 202)
	summaryID, err := parseFixtureID(decodeResult(t, w)["job_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	summary, err := store.GetJob(ctx, alice.ID, summaryID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.VideoVersionID != job.VideoVersionID {
		t.Fatal("summary ignored pinned content version")
	}
	expectStatus(t, apiCall(a, aliceBrowser, "DELETE", "/v1/videos/"+videoID, nil, ""), 200)
	expectStatus(t, apiCall(a, aliceBrowser, "GET", "/v1/runs/"+runID, nil, ""), 404)
	rid, err := parseFixtureID(runID)
	if err != nil {
		t.Fatal(err)
	}
	var status string
	if err = store.DB.Get(&status, "SELECT status FROM agent_runs WHERE id=?", rid); err != nil || status != domain.Cancelled {
		t.Fatalf("delete did not atomically cancel run: status=%s error=%v", status, err)
	}
	expectStatus(t, apiCall(a, aliceBrowser, "POST", "/v1/auth/logout", map[string]any{}, ""), 200)
	expectStatus(t, apiCall(a, aliceBrowser, "GET", "/v1/videos", nil, ""), 401)
}

func parseFixtureID(s string) (domain.ID, error) { return uuid.Parse(s) }

type guestLimiterFixture func(context.Context, string) (bool, error)

func (f guestLimiterFixture) Allow(ctx context.Context, key string) (bool, error) {
	return f(ctx, key)
}

func TestGuestBootstrapOriginLimitsAndExistingIdentity(t *testing.T) {
	store := apiStore(t)
	ctx := context.Background()
	objects := &objectsFixture{completed: map[string]bool{}}
	limiterCalls := []string{}
	limiter := guestLimiterFixture(func(_ context.Context, key string) (bool, error) {
		limiterCalls = append(limiterCalls, key)
		return false, nil
	})
	a := New(config.Default(), store, objects, limiter)
	badOrigin := httptest.NewRequest("POST", "/v1/auth/guest", strings.NewReader(`{}`))
	badOrigin.Header.Set("Content-Type", "application/json")
	badOrigin.Header.Set("Origin", "https://unrelated.invalid")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, badOrigin)
	expectStatus(t, w, 403)
	if len(limiterCalls) != 0 {
		t.Fatal("foreign origin passed bootstrap boundary")
	}
	w = apiCall(a, browserIdentity{}, "POST", "/v1/auth/guest", map[string]any{}, "")
	expectStatus(t, w, 429)
	if len(limiterCalls) != 1 || limiterCalls[0] != "guest:192.0.2.1" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("guest initialization did not use direct client IP limit: %v", limiterCalls)
	}
	var count int
	if err := store.DB.Get(&count, "SELECT COUNT(*) FROM users"); err != nil || count != 0 {
		t.Fatal("rejected bootstrap created an identity", err)
	}
	legacy, err := store.SeedUser(ctx, "existing-account", "!test-only-existing-user")
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.Repeat("a", 64)
	if err = store.CreateAuthSession(ctx, legacy.ID, digest(raw), time.Hour); err != nil {
		t.Fatal(err)
	}
	w = apiCall(a, browserIdentity{cookie: &http.Cookie{Name: "va_session", Value: raw}}, "POST", "/v1/auth/guest", map[string]any{}, "")
	expectStatus(t, w, 200)
	user := decodeResult(t, w)["user"].(map[string]any)
	if user["id"] != legacy.ID.String() || user["is_guest"] != false || len(limiterCalls) != 1 {
		t.Fatal("existing valid owner was replaced or subjected to new-guest quota")
	}
	if err = store.DB.Get(&count, "SELECT COUNT(*) FROM users"); err != nil || count != 1 {
		t.Fatal("existing session created another owner", err)
	}
}
