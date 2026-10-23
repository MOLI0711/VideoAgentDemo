package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// MockModelScript must be provided by a test/dev fixture. There are no built-in
// invented transcripts, answers, embeddings, or automatic paid-provider fallback.
type MockModelScript struct {
	Chat       []ChatResponse
	Vision     []ChatResponse
	Embeddings map[string][]float32
}
type MockModel struct {
	mu                     sync.Mutex
	script                 MockModelScript
	chatIndex, visionIndex int
}

func NewMockModel(script MockModelScript) (*MockModel, error) {
	if len(script.Chat) == 0 && len(script.Vision) == 0 && len(script.Embeddings) == 0 {
		return nil, &Error{Code: "MOCK_FIXTURE_REQUIRED"}
	}
	return &MockModel{script: script}, nil
}
func (m *MockModel) Chat(ctx context.Context, _ ChatRequest) (ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return ChatResponse{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.chatIndex >= len(m.script.Chat) {
		return ChatResponse{}, &Error{Code: "MOCK_SCRIPT_EXHAUSTED"}
	}
	out := m.script.Chat[m.chatIndex]
	m.chatIndex++
	out.Mock = true
	out.Usage.Mock = true
	out.Model = "mock:" + out.Model
	out.ToolCalls = append([]ToolCall(nil), out.ToolCalls...)
	return out, nil
}
func (m *MockModel) Vision(ctx context.Context, _ VisionRequest) (ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return ChatResponse{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.visionIndex >= len(m.script.Vision) {
		return ChatResponse{}, &Error{Code: "MOCK_SCRIPT_EXHAUSTED"}
	}
	out := m.script.Vision[m.visionIndex]
	m.visionIndex++
	out.Mock = true
	out.Usage.Mock = true
	out.Model = "mock:" + out.Model
	return out, nil
}
func (m *MockModel) Embed(ctx context.Context, input []string) (EmbeddingResponse, error) {
	if err := ctx.Err(); err != nil {
		return EmbeddingResponse{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := EmbeddingResponse{Model: "mock:fixture", Mock: true, Usage: Usage{Mock: true}}
	for _, text := range input {
		vector, ok := m.script.Embeddings[text]
		if !ok {
			return EmbeddingResponse{}, &Error{Code: "MOCK_FIXTURE_NOT_FOUND"}
		}
		out.Vectors = append(out.Vectors, append([]float32(nil), vector...))
	}
	return out, nil
}

type MockASRScript struct {
	// Exact URL match prevents a fixture transcript being applied to an arbitrary upload.
	ByAudioURL map[string][]ASRResult
	// SHA-256 fixtures survive presigned URL expiration and attempt path changes.
	// The pipeline supplies this digest from the extracted audio's stored checksum.
	ByAudioSHA256 map[string][]ASRResult
	SubmitError   error
}
type MockASR struct {
	mu       sync.Mutex
	script   MockASRScript
	byID     map[string][]ASRResult
	position map[string]int
}

func NewMockASR(script MockASRScript) (*MockASR, error) {
	if len(script.ByAudioURL) == 0 && len(script.ByAudioSHA256) == 0 && script.SubmitError == nil {
		return nil, &Error{Code: "MOCK_FIXTURE_REQUIRED"}
	}
	m := &MockASR{script: script, byID: make(map[string][]ASRResult), position: make(map[string]int)}
	for input, states := range script.ByAudioURL {
		if input == "" || len(states) == 0 {
			return nil, &Error{Code: "INVALID_MOCK_FIXTURE"}
		}
		m.byID[mockJobID(input)] = states
	}
	for digest, states := range script.ByAudioSHA256 {
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size || len(states) == 0 {
			return nil, &Error{Code: "INVALID_MOCK_FIXTURE"}
		}
		m.byID[mockJobID("sha256:"+digest)] = states
	}
	return m, nil
}
func mockJobID(input string) string {
	sum := sha256.Sum256([]byte(input))
	return fmt.Sprintf("mock-%x", sum[:12])
}
func (m *MockASR) Submit(ctx context.Context, request ASRRequest) (ASRSubmission, error) {
	if err := ctx.Err(); err != nil {
		return ASRSubmission{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.script.SubmitError != nil {
		return ASRSubmission{}, m.script.SubmitError
	}
	input := request.AudioURL
	if _, ok := m.script.ByAudioSHA256[request.AudioSHA256]; ok && request.AudioSHA256 != "" {
		input = "sha256:" + request.AudioSHA256
	} else if _, ok := m.script.ByAudioURL[request.AudioURL]; !ok {
		return ASRSubmission{}, &Error{Code: "MOCK_FIXTURE_NOT_FOUND"}
	}
	return ASRSubmission{JobID: mockJobID(input), RequestID: "mock-request", Mock: true}, nil
}
func (m *MockASR) Poll(ctx context.Context, id string) (ASRResult, error) {
	if err := ctx.Err(); err != nil {
		return ASRResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	states, ok := m.byID[id]
	if !ok {
		return ASRResult{}, &Error{Code: "MOCK_JOB_NOT_FOUND"}
	}
	position := m.position[id]
	if position >= len(states) {
		position = len(states) - 1
	} else {
		m.position[id]++
	}
	out := states[position]
	out.JobID = id
	out.Mock = true
	out.Sentences = append([]Sentence(nil), out.Sentences...)
	out.Raw = append([]byte(nil), out.Raw...)
	return out, nil
}
