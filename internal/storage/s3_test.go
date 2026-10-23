package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestMultipartObjectRoundTripAndPublicSigning(t *testing.T) {
	var uploaded []byte
	var completed bool
	var aborted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=fixture-access/") {
			t.Error("expected explicit SigV4 credential")
		}
		q := r.URL.Query()
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case r.Method == "POST" && q.Has("uploads"):
			io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>fixture-bucket</Bucket><Key>owner/video.mp4</Key><UploadId>fixture-upload</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == "GET" && q.Get("uploadId") != "":
			io.WriteString(w, `<ListPartsResult><Bucket>fixture-bucket</Bucket><Key>owner/video.mp4</Key><UploadId>fixture-upload</UploadId><IsTruncated>false</IsTruncated><Part><PartNumber>1</PartNumber><ETag>"etag-one"</ETag><Size>6</Size></Part></ListPartsResult>`)
		case r.Method == "POST" && q.Get("uploadId") != "":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "etag-one") {
				t.Error("completion ETag not preserved")
			}
			completed = true
			io.WriteString(w, `<CompleteMultipartUploadResult><Bucket>fixture-bucket</Bucket><Key>owner/video.mp4</Key><ETag>"final-etag"</ETag></CompleteMultipartUploadResult>`)
		case r.Method == "DELETE":
			aborted = true
			w.WriteHeader(204)
		case r.Method == "HEAD":
			w.Header().Set("Content-Length", "6")
			w.Header().Set("ETag", `"final-etag"`)
			w.Header().Set("Content-Type", "video/mp4")
		case r.Method == "PUT":
			uploaded, _ = io.ReadAll(r.Body)
			w.Header().Set("ETag", `"put-etag"`)
		case r.Method == "GET":
			w.Header().Set("Content-Length", "6")
			w.Header().Set("Content-Type", "video/mp4")
			io.WriteString(w, "abcdef")
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	store, err := NewS3(S3Config{Endpoint: server.URL, PublicEndpoint: "https://objects.example.test", Bucket: "fixture-bucket", AccessKey: "fixture-access", SecretKey: "fixture-secret", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, err := store.CreateMultipart(ctx, "owner/video.mp4", "video/mp4")
	if err != nil || id != "fixture-upload" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	signed, err := store.SignPart(ctx, "owner/video.mp4", id, 1, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(signed.URL)
	if u.Host != "objects.example.test" || u.Query().Get("partNumber") != "1" || u.Query().Get("X-Amz-Signature") == "" {
		t.Fatalf("bad presigned URL host/parameters")
	}
	parts, err := store.ListParts(ctx, "owner/video.mp4", id)
	if err != nil || len(parts) != 1 || parts[0].Size != 6 {
		t.Fatalf("parts=%+v err=%v", parts, err)
	}
	info, err := store.Complete(ctx, "owner/video.mp4", id, parts)
	if err != nil || !completed || info.Size != 6 {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	_, err = store.Put(ctx, "owner/file.txt", "text/plain", bytes.NewReader([]byte("abcdef")), 6)
	if err != nil || string(uploaded) != "abcdef" {
		t.Fatalf("uploaded=%q err=%v", uploaded, err)
	}
	body, info, err := store.Get(ctx, "owner/video.mp4")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "abcdef" || info.Size != 6 {
		t.Fatalf("data=%q info=%+v", data, info)
	}
	if err = store.Abort(ctx, "owner/video.mp4", id); err != nil || !aborted {
		t.Fatalf("abort=%v", err)
	}
	get, err := store.SignGet(ctx, "owner/video.mp4", time.Minute)
	if err != nil || get.Method != "GET" {
		t.Fatalf("get=%+v err=%v", get, err)
	}
}

func TestStorageValidatesBeforeNetworkAndNeverUsesDefaultCredentials(t *testing.T) {
	if _, err := NewS3(S3Config{Endpoint: "http://localhost:8333", Bucket: "bucket"}); err == nil {
		t.Fatal("ambient credentials allowed")
	}
	store, err := NewS3(S3Config{Endpoint: "http://127.0.0.1:1", Bucket: "bucket", AccessKey: "fixture", SecretKey: "fixture", PathStyle: true, MaxObjectBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Put(context.Background(), "owner/file", "text/plain", bytes.NewReader([]byte("too large")), 9)
	var se *Error
	if !errors.As(err, &se) || se.Code != "INVALID_BODY_SIZE" {
		t.Fatalf("error=%v", err)
	}
	_, err = store.SignPart(context.Background(), "../other", "id", 1, time.Minute)
	if !errors.As(err, &se) || se.Code != "INVALID_OBJECT_KEY" {
		t.Fatalf("error=%v", err)
	}
	_, err = store.SignPart(context.Background(), "owner/file", "id", 0, time.Minute)
	if !errors.As(err, &se) || se.Code != "INVALID_PART_NUMBER" {
		t.Fatalf("error=%v", err)
	}
	_, err = store.Put(context.Background(), "owner/file", "text/plain", bytes.NewReader([]byte("abc")), 2)
	if !errors.As(err, &se) || se.Code != "BODY_SIZE_MISMATCH" {
		t.Fatalf("error=%v", err)
	}
}

func TestMultipartCompletionUsesServerSizes(t *testing.T) {
	var completed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			completed = true
			t.Error("oversized upload completed")
		}
		io.WriteString(w, `<ListPartsResult><IsTruncated>false</IsTruncated><Part><PartNumber>1</PartNumber><ETag>"etag"</ETag><Size>20</Size></Part></ListPartsResult>`)
	}))
	defer server.Close()
	store, _ := NewS3(S3Config{Endpoint: server.URL, Bucket: "bucket", AccessKey: "fixture", SecretKey: "fixture", PathStyle: true, MaxObjectBytes: 10})
	_, err := store.Complete(context.Background(), "owner/file", "upload", []Part{{Number: 1, ETag: "etag", Size: 1}})
	var se *Error
	if !errors.As(err, &se) || se.Code != "OBJECT_TOO_LARGE" || completed {
		t.Fatalf("error=%v completed=%v", err, completed)
	}
}
