package storage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Explicit opt-in. This test touches only its random object prefix and never
// clears buckets or user objects. It uses the credentials passed for this task.
func TestS3Integration(t *testing.T) {
	endpoint := os.Getenv("VA_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set VA_TEST_S3_ENDPOINT for the real S3 integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s, err := NewS3(S3Config{Endpoint: endpoint, Region: "us-east-1", Bucket: "video-agent-dev", AccessKey: os.Getenv("VA_S3_ACCESS_KEY"), SecretKey: os.Getenv("VA_S3_SECRET_KEY"), PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("VA_TEST_S3_CORS_PRECONFIGURED") != "true" {
		if err = s.ConfigureCORS(ctx, "http://localhost:5173"); err != nil {
			t.Fatal(err)
		}
	}
	key := "integration/" + uuid.NewString() + "/multipart.bin"
	defer func() {
		if err := s.Delete(context.Background(), key); err != nil {
			t.Errorf("remove own fixture: %v", err)
		}
	}()
	uploadID, err := s.CreateMultipart(ctx, key, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Abort(context.Background(), key, uploadID)
	data := bytes.Repeat([]byte("real multipart fixture\n"), 300000)
	boundary := 5 << 20
	parts := []Part{}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	for i, payload := range [][]byte{data[:boundary], data[boundary:]} {
		signed, err := s.SignPart(ctx, key, uploadID, int32(i+1), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(ctx, "PUT", signed.URL, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header = signed.Headers.Clone()
		req.Header.Set("Origin", "http://127.0.0.1:5173")
		res, err := httpClient.Do(req)
		if err != nil {
			t.Fatal("signed part upload transport failed")
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("signed part HTTP status %d", res.StatusCode)
		}
		etag := res.Header.Get("ETag")
		if etag == "" {
			t.Fatal("missing exposed multipart ETag")
		}
		exposed := strings.ToLower(res.Header.Get("Access-Control-Expose-Headers"))
		if !strings.Contains(exposed, "etag") && exposed != "*" {
			t.Fatalf("browser cannot read multipart ETag: allowed-origin=%q expose=%q", res.Header.Get("Access-Control-Allow-Origin"), exposed)
		}
		parts = append(parts, Part{Number: int32(i + 1), ETag: etag, Size: int64(len(payload))})
	}
	listed, err := s.ListParts(ctx, key, uploadID)
	if err != nil || len(listed) != 2 {
		t.Fatalf("list real multipart parts: %v count=%d", err, len(listed))
	}
	info, err := s.Complete(ctx, key, uploadID, parts)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(data)) {
		t.Fatal("complete size mismatch")
	}
	body, info, err := s.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(body)
	body.Close()
	if err != nil || !bytes.Equal(got, data) || info.Size != int64(len(data)) {
		t.Fatal("real S3 body mismatch", err)
	}
	signed, err := s.SignGet(ctx, key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, "OPTIONS", signed.URL, nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "GET")
	res, err := httpClient.Do(req)
	if err != nil {
		t.Fatal("CORS transport failed")
	}
	res.Body.Close()
	origin := res.Header.Get("Access-Control-Allow-Origin")
	// Presigned object requests omit browser cookies. A legacy S3 gateway may
	// return '*' for OPTIONS; that is valid for these requests, never for sessions.
	if res.StatusCode >= 300 || (origin != "http://localhost:5173" && origin != "*") {
		t.Fatalf("CORS preflight failed: status=%d origin=%q", res.StatusCode, res.Header.Get("Access-Control-Allow-Origin"))
	}
	t.Logf("S3 CORS preflight allowed-origin=%q; browser object requests must omit credentials", origin)
	unsigned := strings.SplitN(signed.URL, "?", 2)[0]
	req, _ = http.NewRequestWithContext(ctx, "GET", unsigned, nil)
	res, err = httpClient.Do(req)
	if err != nil {
		t.Fatal("unsigned GET transport failed")
	}
	res.Body.Close()
	if res.StatusCode < 400 {
		t.Fatal("private object readable without signature")
	}
	abortKey := "integration/" + uuid.NewString() + "/abort.bin"
	abortID, err := s.CreateMultipart(ctx, abortKey, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Abort(ctx, abortKey, abortID); err != nil {
		t.Fatal(err)
	}
}
