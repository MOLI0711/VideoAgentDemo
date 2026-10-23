package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testModel(t *testing.T, server *httptest.Server, mutate func(*ModelConfig)) *OpenAICompatible {
	t.Helper()
	cfg := ModelConfig{BaseURL: server.URL + "/v1", APIKey: "test-not-a-real-key", ChatModel: "test-chat", VisionModel: "test-vision", EmbeddingModel: "test-embedding", EmbeddingDimensions: 2, AllowInsecureHTTP: true, Timeout: time.Second}
	if mutate != nil {
		mutate(&cfg)
	}
	model, err := NewOpenAICompatible(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func TestCompatibleChatToolsEmbeddingAndVision(t *testing.T) {
	var visionSeen bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-not-a-real-key" {
			t.Errorf("missing explicit credentials")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "request-1")
		switch r.URL.Path {
		case "/v1/embeddings":
			io.WriteString(w, `{"model":"test-embedding","data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}],"usage":{"prompt_tokens":3,"total_tokens":3}}`)
		case "/v1/chat/completions":
			if string(body["model"]) == `"test-vision"` {
				visionSeen = true
				if !strings.Contains(string(body["messages"]), "data:image/png;base64,") {
					t.Error("image bytes were not sent as image input")
				}
				io.WriteString(w, `{"model":"test-vision","choices":[{"message":{"content":"fixture image observation"},"finish_reason":"stop"}]}`)
				return
			}
			if !strings.Contains(string(body["tools"]), `"parameters":{"type":"object"}`) {
				t.Errorf("tool schema missing: %s", body["tools"])
			}
			io.WriteString(w, `{"model":"test-chat","choices":[{"message":{"content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"search_segments","arguments":"{\"query\":\"goroutine\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	model := testModel(t, server, nil)
	chat, err := model.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "fixture"}}, Tools: []ToolDefinition{{Name: "search_segments", Parameters: json.RawMessage(`{"type":"object"}`)}}})
	if err != nil || len(chat.ToolCalls) != 1 || chat.ToolCalls[0].ID != "call-1" || !chat.Usage.Known || chat.Usage.TotalTokens != 9 {
		t.Fatalf("chat=%+v err=%v", chat, err)
	}
	embed, err := model.Embed(context.Background(), []string{"one", "two"})
	if err != nil || len(embed.Vectors) != 2 || embed.Vectors[0][0] != 1 || embed.Vectors[1][1] != 1 {
		t.Fatalf("embed=%+v err=%v", embed, err)
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aZt8AAAAASUVORK5CYII=")
	vision, err := model.Vision(context.Background(), VisionRequest{Prompt: "test", Images: []Image{{Data: png, MIMEType: "image/png"}}})
	if err != nil || !visionSeen || vision.Content == "" || vision.Usage.Known {
		t.Fatalf("vision=%+v err=%v", vision, err)
	}
}

func TestProviderBoundsAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body, code string
		max        int64
		retry      bool
	}{
		{"rate", 429, `{"error":{"message":"do not leak secrets"}}`, "RATE_LIMITED", 1024, true},
		{"large", 200, strings.Repeat("x", 128), "RESPONSE_TOO_LARGE", 32, false},
		{"invalid", 200, `not json`, "INVALID_PROVIDER_RESPONSE", 1024, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) }))
			defer server.Close()
			model := testModel(t, server, func(c *ModelConfig) { c.MaxResponseBytes = tc.max })
			_, err := model.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
			var pe *Error
			if !errors.As(err, &pe) || pe.Code != tc.code || pe.Retryable != tc.retry {
				t.Fatalf("error=%+v", err)
			}
			if strings.Contains(err.Error(), "secrets") {
				t.Fatal("upstream response leaked")
			}
		})
	}
}

func TestProviderDoesNotFollowRedirectOrInventEmbeddings(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	model := testModel(t, server, nil)
	_, err := model.Embed(context.Background(), []string{"x"})
	if err == nil || targetCalls.Load() != 0 {
		t.Fatalf("redirect followed: calls=%d err=%v", targetCalls.Load(), err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"index":0,"embedding":[0,0]}]}`)
	}))
	defer bad.Close()
	_, err = testModel(t, bad, nil).Embed(context.Background(), []string{"x"})
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != "ZERO_EMBEDDING_VECTOR" {
		t.Fatalf("error=%v", err)
	}
}

func TestTencentASRSubmitPollAndSignedRequest(t *testing.T) {
	var polls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-TC-Version") != "2019-06-14" || !strings.Contains(r.Header.Get("Authorization"), "Credential=test-secret-id/2026-10-22/asr/tc3_request") {
			t.Error("missing TC3 authentication")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.Header.Get("X-TC-Action") == "CreateRecTask" {
			if body["ResTextFormat"] != float64(3) || body["SourceType"] != float64(0) {
				t.Error("wrong ASR timestamp mode")
			}
			io.WriteString(w, `{"Response":{"RequestId":"submit-1","Data":{"TaskId":12345}}}`)
			return
		}
		polls++
		if polls == 1 {
			io.WriteString(w, `{"Response":{"RequestId":"poll-1","Data":{"TaskId":12345,"Status":0}}}`)
			return
		}
		io.WriteString(w, `{"Response":{"RequestId":"poll-2","Data":{"TaskId":12345,"Status":2,"AudioDuration":2.5,"ResultDetail":[{"FinalSentence":"测试录音。","StartMs":100,"EndMs":2300,"SpeakerId":0}]}}}`)
	}))
	defer server.Close()
	asr, err := NewTencentASR(TencentASRConfig{Endpoint: server.URL, AllowInsecureHTTP: true, SecretID: "test-secret-id", SecretKey: "test-secret-key", EngineModel: "16k_zh", Clock: func() time.Time { return time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	submission, err := asr.Submit(context.Background(), ASRRequest{AudioURL: "https://example.test/audio.wav", SizeBytes: 100, DurationMS: 2500})
	if err != nil || submission.JobID != "12345" {
		t.Fatalf("submission=%+v err=%v", submission, err)
	}
	pending, err := asr.Poll(context.Background(), submission.JobID)
	if err != nil || pending.Status != ASRPending {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	result, err := asr.Poll(context.Background(), submission.JobID)
	if err != nil || result.Status != ASRSucceeded || result.Sentences[0].StartMS != 100 || result.Sentences[0].EndMS != 2300 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestASRResponseLossIsSubmissionUnknownWithoutRetry(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer server.Close()
	asr, err := NewTencentASR(TencentASRConfig{Endpoint: server.URL, AllowInsecureHTTP: true, SecretID: "fixture", SecretKey: "fixture", EngineModel: "16k_zh"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = asr.Submit(context.Background(), ASRRequest{AudioURL: "https://example.test/audio.wav", SizeBytes: 100, DurationMS: 1000})
	var pe *Error
	if !errors.As(err, &pe) || !pe.SubmissionUnknown || pe.Retryable || requests.Load() != 1 {
		t.Fatalf("requests=%d error=%+v", requests.Load(), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = asr.Submit(ctx, ASRRequest{AudioURL: "https://example.test/audio.wav", SizeBytes: 100, DurationMS: 1000})
	if !errors.As(err, &pe) || pe.SubmissionUnknown || requests.Load() != 1 {
		t.Fatalf("cancel error=%v requests=%d", err, requests.Load())
	}
}

func TestMocksRequireExplicitMatchingFixtures(t *testing.T) {
	if _, err := NewMockModel(MockModelScript{}); err == nil {
		t.Fatal("empty model mock accepted")
	}
	if _, err := NewMockASR(MockASRScript{}); err == nil {
		t.Fatal("empty ASR mock accepted")
	}
	model, _ := NewMockModel(MockModelScript{Chat: []ChatResponse{{Content: "fixture only"}}})
	out, err := model.Chat(context.Background(), ChatRequest{})
	if err != nil || !out.Mock || !out.Usage.Mock {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if _, err = model.Chat(context.Background(), ChatRequest{}); err == nil {
		t.Fatal("exhausted mock generated an answer")
	}
	asr, _ := NewMockASR(MockASRScript{ByAudioURL: map[string][]ASRResult{"fixture://known": {{Status: ASRSucceeded, Sentences: []Sentence{{StartMS: 0, EndMS: 1000, Text: "synthetic fixture"}}}}}})
	if _, err = asr.Submit(context.Background(), ASRRequest{AudioURL: "fixture://other"}); err == nil {
		t.Fatal("unmatched upload received fake transcript")
	}
	job, _ := asr.Submit(context.Background(), ASRRequest{AudioURL: "fixture://known"})
	result, err := asr.Poll(context.Background(), job.JobID)
	if err != nil || !result.Mock {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestMockASRMatchesOnlyExplicitAudioDigest(t *testing.T) {
	digest := strings.Repeat("a", 64)
	m, err := NewMockASR(MockASRScript{ByAudioSHA256: map[string][]ASRResult{digest: {{Status: ASRSucceeded, Sentences: []Sentence{{StartMS: 0, EndMS: 1000, Text: "explicit synthetic fixture"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := m.Submit(context.Background(), ASRRequest{AudioURL: "https://first.example/audio?expired", AudioSHA256: digest})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Submit(context.Background(), ASRRequest{AudioURL: "https://other.example/audio?renewed", AudioSHA256: digest})
	if err != nil || a.JobID != b.JobID {
		t.Fatal("fixture digest did not survive URL refresh", err)
	}
	if _, err = m.Submit(context.Background(), ASRRequest{AudioSHA256: strings.Repeat("b", 64)}); err == nil {
		t.Fatal("unknown audio received fixture transcript")
	}
}

func TestASRInternalErrorIsUnknownSubmission(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"Response":{"RequestId":"fixture","Error":{"Code":"InternalError","Message":"fixture"}}}`)
	}))
	defer server.Close()
	asr, err := NewTencentASR(TencentASRConfig{Endpoint: server.URL, AllowInsecureHTTP: true, SecretID: "fixture", SecretKey: "fixture", EngineModel: "16k_zh"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = asr.Submit(context.Background(), ASRRequest{AudioURL: "https://example.test/audio.wav", SizeBytes: 100, DurationMS: 1000})
	var pe *Error
	if !errors.As(err, &pe) || !pe.SubmissionUnknown || pe.Retryable {
		t.Fatalf("internal error may have accepted a paid job: %v", err)
	}
}
