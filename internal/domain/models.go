package domain

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

type ID = uuid.UUID

func NewID() ID { return uuid.New() }

var (
	ErrNotFound          = errors.New("not found")
	ErrConflict          = errors.New("conflict")
	ErrLeaseLost         = errors.New("execution lease lost")
	ErrInvalid           = errors.New("invalid input")
	ErrNotReady          = errors.New("video not ready")
	ErrBudgetExceeded    = errors.New("budget exceeded")
	ErrSubmissionUnknown = errors.New("external submission outcome unknown")
	ErrCommitUnknown     = errors.New("database commit outcome unknown")
)

const (
	Blocked         = "BLOCKED"
	Queued          = "QUEUED"
	Ready           = "READY"
	Running         = "RUNNING"
	Succeeded       = "SUCCEEDED"
	Failed          = "FAILED"
	RetryWait       = "RETRY_WAIT"
	WaitingExternal = "WAITING_EXTERNAL"
	Cancelling      = "CANCELLING"
	Cancelled       = "CANCELLED"
	Skipped         = "SKIPPED"
)

func Terminal(s string) bool { return s == Succeeded || s == Failed || s == Cancelled }

type User struct {
	ID           ID        `db:"id" json:"id"`
	Username     string    `db:"username" json:"username"`
	PasswordHash string    `db:"password_hash" json:"-"`
	IsGuest      bool      `db:"is_guest" json:"is_guest"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

type Video struct {
	ID                ID         `db:"id" json:"id"`
	OwnerID           ID         `db:"owner_id" json:"owner_id"`
	Title             string     `db:"title" json:"title"`
	Status            string     `db:"status" json:"status"`
	SourceAssetID     ID         `db:"source_asset_id" json:"source_asset_id"`
	ActiveVersionID   ID         `db:"active_version_id" json:"active_content_version"`
	DesiredGeneration int64      `db:"desired_generation" json:"desired_generation"`
	DurationMS        int64      `db:"duration_ms" json:"duration_ms"`
	CreatedAt         time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt         time.Time  `db:"updated_at" json:"updated_at"`
	DeletedAt         *time.Time `db:"deleted_at" json:"deleted_at,omitempty"`
}

type VideoVersion struct {
	ID           ID              `db:"id" json:"id"`
	VideoID      ID              `db:"video_id" json:"video_id"`
	Generation   int64           `db:"generation" json:"generation"`
	Status       string          `db:"status" json:"status"`
	Config       json.RawMessage `db:"config" json:"config"`
	IndexName    string          `db:"index_name" json:"index_name"`
	IndexBuildID string          `db:"index_build_id" json:"index_build_id"`
	Metadata     json.RawMessage `db:"metadata" json:"metadata"`
	CreatedAt    time.Time       `db:"created_at" json:"created_at"`
}

type Upload struct {
	ID          ID        `db:"id" json:"id"`
	OwnerID     ID        `db:"owner_id" json:"owner_id"`
	VideoID     ID        `db:"video_id" json:"video_id"`
	MultipartID string    `db:"multipart_id" json:"multipart_id"`
	ObjectKey   string    `db:"object_key" json:"object_key"`
	Filename    string    `db:"filename" json:"filename"`
	ContentType string    `db:"content_type" json:"content_type"`
	Size        int64     `db:"size" json:"size"`
	Status      string    `db:"status" json:"status"`
	JobID       ID        `db:"job_id" json:"job_id"`
	ExpiresAt   time.Time `db:"expires_at" json:"expires_at"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
}

type CreateUpload struct {
	Title          string `json:"title"`
	Filename       string `json:"filename"`
	ContentType    string `json:"content_type"`
	Size           int64  `json:"size"`
	MultipartID    string `json:"multipart_id"`
	ObjectKey      string `json:"object_key"`
	IdempotencyKey string `json:"-"`
}

type Asset struct {
	ID             ID              `db:"id" json:"id"`
	OwnerID        ID              `db:"owner_id" json:"owner_id"`
	VideoID        ID              `db:"video_id" json:"video_id"`
	VideoVersionID ID              `db:"video_version_id" json:"video_version_id"`
	StepID         ID              `db:"step_id" json:"step_id"`
	AttemptNo      uint64          `db:"attempt_no" json:"attempt_no"`
	Kind           string          `db:"kind" json:"kind"`
	StorageBackend string          `db:"storage_backend" json:"storage_backend"`
	ObjectKey      string          `db:"object_key" json:"object_key"`
	ContentType    string          `db:"content_type" json:"content_type"`
	Size           int64           `db:"size" json:"size"`
	Checksum       string          `db:"checksum" json:"checksum"`
	StartMS        int64           `db:"start_ms" json:"start_ms"`
	EndMS          int64           `db:"end_ms" json:"end_ms"`
	Metadata       json.RawMessage `db:"metadata" json:"metadata"`
	CreatedAt      time.Time       `db:"created_at" json:"created_at"`
}

type Job struct {
	ID             ID              `db:"id" json:"id"`
	OwnerID        ID              `db:"owner_id" json:"owner_id"`
	VideoID        ID              `db:"video_id" json:"video_id"`
	VideoVersionID ID              `db:"video_version_id" json:"video_version_id"`
	Type           string          `db:"type" json:"type"`
	Status         string          `db:"status" json:"status"`
	Config         json.RawMessage `db:"config" json:"config"`
	ErrorCode      string          `db:"error_code" json:"error_code"`
	ErrorMessage   string          `db:"error_message" json:"error_message"`
	DeadlineAt     time.Time       `db:"deadline_at" json:"deadline_at"`
	CreatedAt      time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time       `db:"updated_at" json:"updated_at"`
	Steps          []Step          `db:"-" json:"steps,omitempty"`
}

type Step struct {
	ID                   ID              `db:"id" json:"id"`
	JobID                ID              `db:"job_id" json:"job_id"`
	StepKey              string          `db:"step_key" json:"step_key"`
	StepType             string          `db:"step_type" json:"step_type"`
	WorkerKind           string          `db:"worker_kind" json:"worker_kind"`
	Status               string          `db:"status" json:"status"`
	DispatchSeq          uint64          `db:"dispatch_seq" json:"dispatch_seq"`
	AttemptNo            uint64          `db:"attempt_no" json:"attempt_no"`
	RetryCount           int             `db:"retry_count" json:"retry_count"`
	LeaseOwner           string          `db:"lease_owner" json:"lease_owner"`
	LeaseUntil           *time.Time      `db:"lease_until" json:"lease_until,omitempty"`
	NextRunAt            time.Time       `db:"next_run_at" json:"next_run_at"`
	NextPollAt           *time.Time      `db:"next_poll_at" json:"next_poll_at,omitempty"`
	CurrentExternalJobID ID              `db:"current_external_job_id" json:"current_external_job_id"`
	Result               json.RawMessage `db:"result" json:"result"`
	ErrorCode            string          `db:"error_code" json:"error_code"`
	CreatedAt            time.Time       `db:"created_at" json:"created_at"`
}

type Lease struct {
	Job       Job       `json:"job"`
	Step      Step      `json:"step"`
	WorkerID  string    `json:"worker_id"`
	AttemptNo uint64    `json:"attempt_no"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Sentence struct {
	ID             ID     `db:"id" json:"id"`
	VideoVersionID ID     `db:"video_version_id" json:"video_version_id"`
	StartMS        int64  `db:"start_ms" json:"start_ms"`
	EndMS          int64  `db:"end_ms" json:"end_ms"`
	Text           string `db:"text" json:"text"`
}

type Frame struct {
	ID             ID     `db:"id" json:"id"`
	VideoVersionID ID     `db:"video_version_id" json:"video_version_id"`
	AssetID        ID     `db:"asset_id" json:"asset_id"`
	TimestampMS    int64  `db:"timestamp_ms" json:"timestamp_ms"`
	OCRText        string `db:"ocr_text" json:"ocr_text"`
	OCRStatus      string `db:"ocr_status" json:"ocr_status"`
}

type Segment struct {
	ID             ID              `db:"id" json:"id"`
	VideoVersionID ID              `db:"video_version_id" json:"video_version_id"`
	Ordinal        int             `db:"ordinal" json:"ordinal"`
	StartMS        int64           `db:"start_ms" json:"start_ms"`
	EndMS          int64           `db:"end_ms" json:"end_ms"`
	Text           string          `db:"text" json:"text"`
	OCRText        string          `db:"ocr_text" json:"ocr_text"`
	SentenceIDs    json.RawMessage `db:"sentence_ids" json:"sentence_ids"`
	FrameIDs       json.RawMessage `db:"frame_ids" json:"frame_ids"`
}

type StepResult struct {
	Skipped      bool            `json:"skipped,omitempty"`
	Assets       []Asset         `json:"assets,omitempty"`
	Sentences    []Sentence      `json:"sentences,omitempty"`
	Frames       []Frame         `json:"frames,omitempty"`
	Segments     []Segment       `json:"segments,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	IndexName    string          `json:"index_name,omitempty"`
	IndexBuildID string          `json:"index_build_id,omitempty"`
	DurationMS   int64           `json:"duration_ms,omitempty"`
	Answer       json.RawMessage `json:"answer,omitempty"`
	ExternalWait *ExternalWait   `json:"external_wait,omitempty"`
}

type ExternalJob struct {
	ID               ID              `db:"id" json:"id"`
	StepID           ID              `db:"step_id" json:"step_id"`
	SubmitGeneration uint64          `db:"submit_generation" json:"submit_generation"`
	ProviderRef      string          `db:"provider_ref" json:"provider_ref"`
	ClientRequestID  string          `db:"client_request_id" json:"client_request_id"`
	ProviderJobID    string          `db:"provider_job_id" json:"provider_job_id"`
	Status           string          `db:"status" json:"status"`
	Request          json.RawMessage `db:"request" json:"request"`
	DeadlineAt       time.Time       `db:"deadline_at" json:"deadline_at"`
	CreatedAt        time.Time       `db:"created_at" json:"created_at"`
}

type ExternalWait struct {
	ExternalJobID ID            `json:"external_job_id"`
	ProviderJobID string        `json:"provider_job_id"`
	PollAfter     time.Duration `json:"-"`
}

type DispatchEvent struct {
	SchemaVersion int    `json:"schema_version"`
	EventID       ID     `json:"event_id"`
	JobID         ID     `json:"job_id"`
	StepID        ID     `json:"step_id"`
	DispatchSeq   uint64 `json:"dispatch_seq"`
	WorkerKind    string `json:"worker_kind"`
}

type OutboxEvent struct {
	EventID           ID              `db:"event_id" json:"event_id"`
	StepID            ID              `db:"step_id" json:"step_id"`
	DispatchSeq       uint64          `db:"dispatch_seq" json:"dispatch_seq"`
	Payload           json.RawMessage `db:"payload" json:"payload"`
	Status            string          `db:"status" json:"status"`
	PublishAttempt    uint64          `db:"publish_attempt" json:"publish_attempt"`
	PublishLeaseUntil *time.Time      `db:"publish_lease_until" json:"publish_lease_until,omitempty"`
	NextPublishAt     time.Time       `db:"next_publish_at" json:"next_publish_at"`
}

type Agent struct {
	ID              ID        `db:"id" json:"id"`
	OwnerID         ID        `db:"owner_id" json:"owner_id"`
	Name            string    `db:"name" json:"name"`
	Description     string    `db:"description" json:"description"`
	Status          string    `db:"status" json:"status"`
	ActiveVersionID ID        `db:"active_version_id" json:"active_version_id"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
}

type AgentVersion struct {
	ID        ID              `db:"id" json:"id"`
	AgentID   ID              `db:"agent_id" json:"agent_id"`
	VersionNo int             `db:"version_no" json:"version_no"`
	Status    string          `db:"status" json:"status"`
	Config    json.RawMessage `db:"config" json:"config"`
	CreatedAt time.Time       `db:"created_at" json:"created_at"`
}

type Session struct {
	ID             ID        `db:"id" json:"id"`
	OwnerID        ID        `db:"owner_id" json:"owner_id"`
	VideoID        ID        `db:"video_id" json:"video_id"`
	VideoVersionID ID        `db:"video_version_id" json:"video_version_id"`
	AgentVersionID ID        `db:"agent_version_id" json:"agent_version_id"`
	ActiveRunID    ID        `db:"active_run_id" json:"active_run_id"`
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
}

type Message struct {
	ID        ID        `db:"id" json:"id"`
	SessionID ID        `db:"session_id" json:"session_id"`
	RunID     ID        `db:"run_id" json:"run_id"`
	Seq       int64     `db:"seq" json:"seq"`
	Role      string    `db:"role" json:"role"`
	Content   string    `db:"content" json:"content"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type Run struct {
	ID             ID              `db:"id" json:"id"`
	JobID          ID              `db:"job_id" json:"job_id"`
	SessionID      ID              `db:"session_id" json:"session_id"`
	OwnerID        ID              `db:"owner_id" json:"owner_id"`
	VideoVersionID ID              `db:"video_version_id" json:"video_version_id"`
	AgentVersionID ID              `db:"agent_version_id" json:"agent_version_id"`
	IndexName      string          `db:"index_name" json:"index_name"`
	IndexBuildID   string          `db:"index_build_id" json:"index_build_id"`
	Status         string          `db:"status" json:"status"`
	Question       string          `db:"question" json:"question"`
	Answer         json.RawMessage `db:"answer" json:"answer"`
	NextEventSeq   int64           `db:"next_event_seq" json:"latest_event_seq"`
	CheckpointSeq  int64           `db:"checkpoint_seq" json:"checkpoint_seq"`
	BudgetUsed     int             `db:"budget_used" json:"budget_used"`
	CreatedAt      time.Time       `db:"created_at" json:"created_at"`
}

type RunEvent struct {
	RunID     ID              `db:"run_id" json:"run_id"`
	Seq       int64           `db:"seq" json:"seq"`
	AttemptNo uint64          `db:"attempt_no" json:"attempt_no"`
	Type      string          `db:"type" json:"type"`
	Payload   json.RawMessage `db:"payload" json:"payload"`
	CreatedAt time.Time       `db:"created_at" json:"created_at"`
}
type Checkpoint struct {
	RunID     ID              `db:"run_id" json:"run_id"`
	Seq       int64           `db:"seq" json:"seq"`
	AttemptNo uint64          `db:"attempt_no" json:"attempt_no"`
	State     json.RawMessage `db:"state" json:"state"`
	CreatedAt time.Time       `db:"created_at" json:"created_at"`
}
type ToolInvocation struct {
	ID            ID              `db:"id" json:"id"`
	RunID         ID              `db:"run_id" json:"run_id"`
	LogicalCallID string          `db:"logical_call_id" json:"logical_call_id"`
	ToolName      string          `db:"tool_name" json:"tool_name"`
	Args          json.RawMessage `db:"args" json:"args"`
	Status        string          `db:"status" json:"status"`
	Result        json.RawMessage `db:"result" json:"result"`
	AttemptNo     uint64          `db:"attempt_no" json:"attempt_no"`
}

// ModelReservation describes one physical text-model request. Reusing CallKey
// only reconciles its existing reservation and never authorizes another request.
type ModelReservation struct {
	CallKey        string `json:"call_key"`
	RequestHash    string `json:"request_hash"`
	ProviderRef    string `json:"provider_ref"`
	ModelID        string `json:"model_id"`
	ReservedTokens int    `json:"reserved_tokens"`
	MaxTokens      int    `json:"max_tokens"`
}

type ModelCall struct {
	ID             ID        `db:"id" json:"id"`
	RunID          ID        `db:"run_id" json:"run_id"`
	JobID          ID        `db:"job_id" json:"job_id"`
	StepID         ID        `db:"step_id" json:"step_id"`
	CallKey        string    `db:"call_key" json:"call_key"`
	RequestHash    string    `db:"request_hash" json:"request_hash"`
	AttemptNo      uint64    `db:"attempt_no" json:"attempt_no"`
	ProviderRef    string    `db:"provider_ref" json:"provider_ref"`
	ModelID        string    `db:"model_id" json:"model_id"`
	Status         string    `db:"status" json:"status"`
	ReservedTokens int       `db:"reserved_tokens" json:"reserved_tokens"`
	ActualTokens   int       `db:"actual_tokens" json:"actual_tokens"`
	UsageKnown     bool      `db:"usage_known" json:"usage_known"`
	BudgetLimit    int       `db:"budget_limit" json:"budget_limit"`
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
}
