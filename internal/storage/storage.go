// Package storage provides explicit-credential, bucket-scoped object storage.
package storage

import (
	"context"
	"io"
	"net/http"
	"time"
)

type Part struct {
	Number int32  `json:"number"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size"`
}

type SignedURL struct {
	URL       string      `json:"url"`
	Method    string      `json:"method"`
	Headers   http.Header `json:"headers,omitempty"`
	ExpiresAt time.Time   `json:"expires_at"`
}

type ObjectInfo struct {
	Key         string `json:"key"`
	Size        int64  `json:"size"`
	ETag        string `json:"etag"`
	ContentType string `json:"content_type"`
}

// Store never chooses object keys or authorizes users. Callers supply only keys
// whose ownership they have checked, and persist multipart IDs before returning.
type Store interface {
	CreateMultipart(context.Context, string, string) (string, error)
	SignPart(context.Context, string, string, int32, time.Duration) (SignedURL, error)
	ListParts(context.Context, string, string) ([]Part, error)
	Complete(context.Context, string, string, []Part) (ObjectInfo, error)
	Abort(context.Context, string, string) error
	Head(context.Context, string) (ObjectInfo, error)
	Put(context.Context, string, string, io.Reader, int64) (ObjectInfo, error)
	Get(context.Context, string) (io.ReadCloser, ObjectInfo, error)
	SignGet(context.Context, string, time.Duration) (SignedURL, error)
}

type Error struct {
	Operation string
	Code      string
	Retryable bool
	Cause     error
}

func (e *Error) Error() string { return "storage " + e.Operation + ": " + e.Code }
func (e *Error) Unwrap() error { return e.Cause }
