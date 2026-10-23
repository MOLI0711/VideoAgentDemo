// Package provider implements explicit cloud adapters and opt-in scripted mocks.
package provider

import (
	"context"
	"encoding/json"
)

type Error struct {
	Code              string
	StatusCode        int
	RequestID         string
	Retryable         bool
	SubmissionUnknown bool
	Cause             error
}

func (e *Error) Error() string { return "provider: " + e.Code }
func (e *Error) Unwrap() error { return e.Cause }

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	Known        bool  `json:"known"`
	Mock         bool  `json:"mock"`
}

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type Image struct {
	URL      string `json:"url,omitempty"`
	Data     []byte `json:"-"`
	MIMEType string `json:"mime_type,omitempty"`
}
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	Images     []Image    `json:"-"`
}
type ChatRequest struct {
	Messages    []Message
	Tools       []ToolDefinition
	MaxTokens   int
	Temperature *float64
	JSONMode    bool
}
type ChatResponse struct {
	Content      string     `json:"content"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	Model        string     `json:"model"`
	RequestID    string     `json:"request_id"`
	FinishReason string     `json:"finish_reason"`
	Usage        Usage      `json:"usage"`
	Mock         bool       `json:"mock"`
}
type EmbeddingResponse struct {
	Vectors   [][]float32 `json:"vectors"`
	Model     string      `json:"model"`
	RequestID string      `json:"request_id"`
	Usage     Usage       `json:"usage"`
	Mock      bool        `json:"mock"`
}
type VisionRequest struct {
	Prompt    string
	Images    []Image
	MaxTokens int
	JSONMode  bool
}
type Model interface {
	Chat(context.Context, ChatRequest) (ChatResponse, error)
	Embed(context.Context, []string) (EmbeddingResponse, error)
	Vision(context.Context, VisionRequest) (ChatResponse, error)
}

type ASRRequest struct {
	// AudioSHA256 is computed by the trusted media pipeline, used only to select
	// explicit local fixtures. Cloud providers do not treat it as idempotency.
	AudioSHA256     string
	AudioURL        string
	ClientRequestID string // audit correlation only; NOT provider idempotency.
	DurationMS      int64
	SizeBytes       int64
}
type ASRSubmission struct {
	JobID     string `json:"job_id"`
	RequestID string `json:"request_id"`
	Mock      bool   `json:"mock"`
}
type Sentence struct {
	StartMS   int64  `json:"start_ms"`
	EndMS     int64  `json:"end_ms"`
	Text      string `json:"text"`
	SpeakerID int    `json:"speaker_id,omitempty"`
}
type ASRStatus string

const (
	ASRPending   ASRStatus = "PENDING"
	ASRRunning   ASRStatus = "RUNNING"
	ASRSucceeded ASRStatus = "SUCCEEDED"
	ASRFailed    ASRStatus = "FAILED"
)

type ASRResult struct {
	JobID      string          `json:"job_id"`
	Status     ASRStatus       `json:"status"`
	Sentences  []Sentence      `json:"sentences,omitempty"`
	DurationMS int64           `json:"duration_ms"`
	RequestID  string          `json:"request_id"`
	ErrorCode  string          `json:"error_code,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"`
	Mock       bool            `json:"mock"`
}
type ASR interface {
	Submit(context.Context, ASRRequest) (ASRSubmission, error)
	Poll(context.Context, string) (ASRResult, error)
}
