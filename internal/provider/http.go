package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func configuredURL(raw string, allowHTTP bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(allowHTTP && u.Scheme == "http")) {
		return nil, &Error{Code: "INVALID_PROVIDER_ENDPOINT"}
	}
	return u, nil
}
func boundedClient(input *http.Client, timeout time.Duration) *http.Client {
	client := &http.Client{}
	if input != nil {
		copy := *input
		client = &copy
	}
	client.Timeout = timeout
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func requestJSON(ctx context.Context, client *http.Client, endpoint string, headers http.Header, body []byte, maxResponse int64) ([]byte, http.Header, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, &Error{Code: "CANCELLED", Cause: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, &Error{Code: "INVALID_REQUEST", Cause: err}
	}
	req.Header = headers.Clone()
	resp, err := client.Do(req)
	if err != nil {
		code := "TRANSPORT_ERROR"
		retry := true
		if errors.Is(ctx.Err(), context.Canceled) {
			code = "CANCELLED"
			retry = false
		}
		if errors.Is(err, context.DeadlineExceeded) {
			code = "TIMEOUT"
		}
		// Error() never includes err's URL (which may contain credentials).
		return nil, nil, &Error{Code: code, Retryable: retry, Cause: err}
	}
	defer resp.Body.Close()
	requestID := resp.Header.Get("X-Request-Id")
	if requestID == "" {
		requestID = resp.Header.Get("Request-Id")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, resp.Header, &Error{Code: "RESPONSE_READ_ERROR", StatusCode: resp.StatusCode, Retryable: true, RequestID: requestID, Cause: err}
	}
	if int64(len(data)) > maxResponse {
		return nil, resp.Header, &Error{Code: "RESPONSE_TOO_LARGE", StatusCode: resp.StatusCode, RequestID: requestID}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := "UPSTREAM_HTTP_ERROR"
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			code = "AUTHENTICATION_FAILED"
		}
		if resp.StatusCode == 429 {
			code = "RATE_LIMITED"
		}
		return nil, resp.Header, &Error{Code: code, StatusCode: resp.StatusCode, Retryable: resp.StatusCode == 429 || resp.StatusCode >= 500, RequestID: requestID}
	}
	if !json.Valid(data) {
		return nil, resp.Header, &Error{Code: "INVALID_PROVIDER_RESPONSE", StatusCode: resp.StatusCode, RequestID: requestID}
	}
	return data, resp.Header, nil
}

func safeIdentifier(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00")
}
