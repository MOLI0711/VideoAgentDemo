package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type S3Config struct {
	Endpoint       string
	PublicEndpoint string // Presign this endpoint directly; never rewrite a signed host.
	Region         string
	Bucket         string
	AccessKey      string
	SecretKey      string
	SessionToken   string
	PathStyle      bool
	HTTPClient     *http.Client
	Timeout        time.Duration
	MaxObjectBytes int64
}

type S3 struct {
	client   *s3.Client
	signer   *s3.PresignClient
	bucket   string
	maxBytes int64
}

// NewS3 does not load ~/.aws, ambient environment credentials, or instance metadata.
func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" || strings.ContainsAny(cfg.Bucket, "/\\ \t\r\n") || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, &Error{Operation: "configure", Code: "MISSING_CONFIGURATION"}
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.PublicEndpoint == "" {
		cfg.PublicEndpoint = cfg.Endpoint
	}
	for _, endpoint := range []string{cfg.Endpoint, cfg.PublicEndpoint} {
		u, err := url.Parse(endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return nil, &Error{Operation: "configure", Code: "INVALID_ENDPOINT"}
		}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Minute
	}
	if cfg.Timeout < 0 {
		return nil, &Error{Code: "INVALID_TIMEOUT"}
	}
	if cfg.MaxObjectBytes == 0 {
		cfg.MaxObjectBytes = 2 << 30
	}
	if cfg.MaxObjectBytes < 1 {
		return nil, &Error{Code: "INVALID_SIZE_LIMIT"}
	}
	client := &http.Client{}
	if cfg.HTTPClient != nil {
		copy := *cfg.HTTPClient
		client = &copy
	}
	client.Timeout = cfg.Timeout
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	credentials := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: cfg.AccessKey, SecretAccessKey: cfg.SecretKey, SessionToken: cfg.SessionToken, Source: "explicit"}, nil
	})
	makeClient := func(endpoint string) *s3.Client {
		return s3.New(s3.Options{Region: cfg.Region, Credentials: credentials, BaseEndpoint: aws.String(strings.TrimRight(endpoint, "/")), UsePathStyle: cfg.PathStyle, HTTPClient: client, RetryMaxAttempts: 1, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired})
	}
	return &S3{client: makeClient(cfg.Endpoint), signer: s3.NewPresignClient(makeClient(cfg.PublicEndpoint)), bucket: cfg.Bucket, maxBytes: cfg.MaxObjectBytes}, nil
}

// NewCOS uses Tencent COS's documented S3-compatible API and explicit keys.
// Bucket must include its APPID suffix, for example project-1250000000.
func NewCOS(cfg S3Config) (*S3, error) {
	if cfg.Region == "" {
		return nil, &Error{Operation: "configure", Code: "MISSING_COS_REGION"}
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://cos." + cfg.Region + ".myqcloud.com"
	}
	cfg.PathStyle = false
	return NewS3(cfg)
}

func validKey(key string) error {
	if key == "" || len(key) > 1024 || strings.HasPrefix(key, "/") || strings.ContainsAny(key, "\\\x00\r\n") {
		return &Error{Code: "INVALID_OBJECT_KEY"}
	}
	for _, part := range strings.Split(key, "/") {
		if part == ".." || part == "." {
			return &Error{Code: "INVALID_OBJECT_KEY"}
		}
	}
	return nil
}
func validUpload(key, id string) error {
	if err := validKey(key); err != nil {
		return err
	}
	if id == "" || len(id) > 2048 || strings.ContainsAny(id, "\x00\r\n") {
		return &Error{Code: "INVALID_UPLOAD_ID"}
	}
	return nil
}
func validExpiry(exp time.Duration) error {
	if exp < time.Second || exp > 24*time.Hour {
		return &Error{Code: "INVALID_SIGNED_URL_EXPIRY"}
	}
	return nil
}
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	code := "UPSTREAM_ERROR"
	retry := true
	var api interface{ ErrorCode() string }
	if errors.As(err, &api) {
		code = api.ErrorCode()
	}
	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) {
		retry = status.HTTPStatusCode() == 429 || status.HTTPStatusCode() >= 500
	}
	if errors.Is(err, context.Canceled) {
		code = "CANCELLED"
		retry = false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		code = "TIMEOUT"
	}
	return &Error{Operation: op, Code: code, Retryable: retry, Cause: err}
}

func (s *S3) CreateMultipart(ctx context.Context, key, contentType string) (string, error) {
	if err := validKey(key); err != nil {
		return "", err
	}
	out, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &s.bucket, Key: &key, ContentType: &contentType})
	if err != nil {
		return "", wrap("create_multipart", err)
	}
	if aws.ToString(out.UploadId) == "" {
		return "", &Error{Operation: "create_multipart", Code: "INVALID_UPSTREAM_RESPONSE"}
	}
	return *out.UploadId, nil
}
func (s *S3) SignPart(ctx context.Context, key, id string, number int32, expiry time.Duration) (SignedURL, error) {
	if err := validUpload(key, id); err != nil {
		return SignedURL{}, err
	}
	if number < 1 || number > 10000 {
		return SignedURL{}, &Error{Code: "INVALID_PART_NUMBER"}
	}
	if err := validExpiry(expiry); err != nil {
		return SignedURL{}, err
	}
	out, err := s.signer.PresignUploadPart(ctx, &s3.UploadPartInput{Bucket: &s.bucket, Key: &key, UploadId: &id, PartNumber: &number}, func(o *s3.PresignOptions) { o.Expires = expiry })
	if err != nil {
		return SignedURL{}, wrap("sign_part", err)
	}
	return SignedURL{URL: out.URL, Method: out.Method, Headers: out.SignedHeader, ExpiresAt: time.Now().UTC().Add(expiry)}, nil
}
func (s *S3) ListParts(ctx context.Context, key, id string) ([]Part, error) {
	if err := validUpload(key, id); err != nil {
		return nil, err
	}
	var result []Part
	paginator := s3.NewListPartsPaginator(s.client, &s3.ListPartsInput{Bucket: &s.bucket, Key: &key, UploadId: &id})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, wrap("list_parts", err)
		}
		for _, part := range page.Parts {
			result = append(result, Part{Number: aws.ToInt32(part.PartNumber), ETag: aws.ToString(part.ETag), Size: aws.ToInt64(part.Size)})
		}
		if len(result) > 10000 {
			return nil, &Error{Operation: "list_parts", Code: "TOO_MANY_PARTS"}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	return result, nil
}
func (s *S3) Complete(ctx context.Context, key, id string, parts []Part) (ObjectInfo, error) {
	if err := validUpload(key, id); err != nil {
		return ObjectInfo{}, err
	}
	if len(parts) == 0 || len(parts) > 10000 {
		return ObjectInfo{}, &Error{Code: "INVALID_PARTS"}
	}
	ordered := append([]Part(nil), parts...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Number < ordered[j].Number })
	actual, err := s.ListParts(ctx, key, id)
	if err != nil {
		return ObjectInfo{}, err
	}
	byNumber := make(map[int32]Part, len(actual))
	for _, part := range actual {
		byNumber[part.Number] = part
	}
	completed := make([]types.CompletedPart, 0, len(parts))
	var total int64
	for i, part := range ordered {
		stored, ok := byNumber[part.Number]
		if part.Number < 1 || part.Number > 10000 || (i > 0 && part.Number == ordered[i-1].Number) || !ok || strings.Trim(part.ETag, "\"") != strings.Trim(stored.ETag, "\"") || part.ETag == "" {
			return ObjectInfo{}, &Error{Code: "INVALID_PARTS"}
		}
		if stored.Size < 0 || stored.Size > s.maxBytes-total {
			return ObjectInfo{}, &Error{Code: "OBJECT_TOO_LARGE"}
		}
		total += stored.Size
		completed = append(completed, types.CompletedPart{PartNumber: aws.Int32(part.Number), ETag: aws.String(stored.ETag)})
	}
	_, err = s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &s.bucket, Key: &key, UploadId: &id, MultipartUpload: &types.CompletedMultipartUpload{Parts: completed}})
	if err != nil {
		return ObjectInfo{}, wrap("complete", err)
	}
	return s.Head(ctx, key)
}
func (s *S3) Abort(ctx context.Context, key, id string) error {
	if err := validUpload(key, id); err != nil {
		return err
	}
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &s.bucket, Key: &key, UploadId: &id})
	return wrap("abort", err)
}
func (s *S3) Head(ctx context.Context, key string) (ObjectInfo, error) {
	if err := validKey(key); err != nil {
		return ObjectInfo{}, err
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return ObjectInfo{}, wrap("head", err)
	}
	size := aws.ToInt64(out.ContentLength)
	if size < 0 || size > s.maxBytes {
		return ObjectInfo{}, &Error{Operation: "head", Code: "OBJECT_TOO_LARGE"}
	}
	return ObjectInfo{Key: key, Size: size, ETag: aws.ToString(out.ETag), ContentType: aws.ToString(out.ContentType)}, nil
}
func (s *S3) Put(ctx context.Context, key, contentType string, body io.Reader, size int64) (ObjectInfo, error) {
	if err := validKey(key); err != nil {
		return ObjectInfo{}, err
	}
	if body == nil || size < 0 || size > s.maxBytes {
		return ObjectInfo{}, &Error{Code: "INVALID_BODY_SIZE"}
	}
	// Preserve seekability for SigV4. Small JSON/image streams may be buffered;
	// large media must use a file/ReadSeeker instead of unbounded heap buffering.
	if seeker, ok := body.(io.ReadSeeker); ok {
		cur, err := seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			return ObjectInfo{}, wrap("put", err)
		}
		end, err := seeker.Seek(0, io.SeekEnd)
		if err != nil {
			return ObjectInfo{}, wrap("put", err)
		}
		if _, err = seeker.Seek(cur, io.SeekStart); err != nil {
			return ObjectInfo{}, wrap("put", err)
		}
		if end-cur != size {
			return ObjectInfo{}, &Error{Code: "BODY_SIZE_MISMATCH"}
		}
	} else {
		if size > 16<<20 {
			return ObjectInfo{}, &Error{Code: "NON_SEEKABLE_BODY_TOO_LARGE"}
		}
		data, err := io.ReadAll(io.LimitReader(body, size+1))
		if err != nil {
			return ObjectInfo{}, wrap("put", err)
		}
		if int64(len(data)) != size {
			return ObjectInfo{}, &Error{Code: "BODY_SIZE_MISMATCH"}
		}
		body = bytes.NewReader(data)
	}
	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.bucket, Key: &key, ContentType: &contentType, ContentLength: &size, Body: body})
	if err != nil {
		return ObjectInfo{}, wrap("put", err)
	}
	return ObjectInfo{Key: key, Size: size, ETag: aws.ToString(out.ETag), ContentType: contentType}, nil
}
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	if err := validKey(key); err != nil {
		return nil, ObjectInfo{}, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return nil, ObjectInfo{}, wrap("get", err)
	}
	size := aws.ToInt64(out.ContentLength)
	if out.ContentLength == nil || size < 0 || size > s.maxBytes {
		_ = out.Body.Close()
		return nil, ObjectInfo{}, &Error{Operation: "get", Code: "OBJECT_TOO_LARGE"}
	}
	return out.Body, ObjectInfo{Key: key, Size: size, ETag: aws.ToString(out.ETag), ContentType: aws.ToString(out.ContentType)}, nil
}
func (s *S3) SignGet(ctx context.Context, key string, expiry time.Duration) (SignedURL, error) {
	if err := validKey(key); err != nil {
		return SignedURL{}, err
	}
	if err := validExpiry(expiry); err != nil {
		return SignedURL{}, err
	}
	out, err := s.signer.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key}, func(o *s3.PresignOptions) { o.Expires = expiry })
	if err != nil {
		return SignedURL{}, wrap("sign_get", err)
	}
	return SignedURL{URL: out.URL, Method: out.Method, Headers: out.SignedHeader, ExpiresAt: time.Now().UTC().Add(expiry)}, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	return wrap("delete", err)
}

// EnsureBucket is an explicit local-development provisioning operation; normal
// application startup never creates or changes remote buckets automatically.
func (s *S3) EnsureBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &s.bucket})
	if err == nil {
		return nil
	}
	var status interface{ HTTPStatusCode() int }
	if !errors.As(err, &status) || status.HTTPStatusCode() != http.StatusNotFound {
		return wrap("head_bucket", err)
	}
	input := &s3.CreateBucketInput{Bucket: &s.bucket}
	region := s.client.Options().Region
	if region != "us-east-1" {
		input.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(region)}
	}
	_, err = s.client.CreateBucket(ctx, input)
	return wrap("create_bucket", err)
}

func (s *S3) ConfigureCORS(ctx context.Context, origin string) error {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return &Error{Operation: "cors", Code: "INVALID_ORIGIN"}
	}
	origin = u.Scheme + "://" + u.Host
	_, err = s.client.PutBucketCors(ctx, &s3.PutBucketCorsInput{Bucket: &s.bucket, CORSConfiguration: &types.CORSConfiguration{CORSRules: []types.CORSRule{{AllowedOrigins: []string{origin}, AllowedMethods: []string{"GET", "HEAD", "PUT"}, AllowedHeaders: []string{"*"}, ExposeHeaders: []string{"ETag", "Content-Length", "Content-Type"}, MaxAgeSeconds: aws.Int32(3600)}}}})
	return wrap("configure_cors", err)
}

// Ping verifies access to the configured bucket without creating objects.
func (s *S3) Ping(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	return wrap("head_bucket", err)
}
