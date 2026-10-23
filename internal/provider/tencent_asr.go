package provider

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type TencentASRConfig struct {
	SecretID          string
	SecretKey         string
	SessionToken      string
	Region            string
	EngineModel       string
	Endpoint          string
	HTTPClient        *http.Client
	Timeout           time.Duration
	MaxResponseBytes  int64
	AllowInsecureHTTP bool
	Clock             func() time.Time
}

type TencentASR struct {
	cfg      TencentASRConfig
	endpoint *url.URL
	client   *http.Client
}

func NewTencentASR(cfg TencentASRConfig) (*TencentASR, error) {
	if !safeIdentifier(cfg.SecretID) || cfg.SecretKey == "" || !safeIdentifier(cfg.EngineModel) {
		return nil, &Error{Code: "MISSING_ASR_CONFIGURATION"}
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://asr.tencentcloudapi.com"
	}
	endpoint, err := configuredURL(cfg.Endpoint, cfg.AllowInsecureHTTP)
	if err != nil {
		return nil, err
	}
	if endpoint.Path != "" && endpoint.Path != "/" {
		return nil, &Error{Code: "INVALID_PROVIDER_ENDPOINT"}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Timeout < 0 {
		return nil, &Error{Code: "INVALID_TIMEOUT"}
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = 16 << 20
	}
	if cfg.MaxResponseBytes < 1 {
		return nil, &Error{Code: "INVALID_LIMITS"}
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &TencentASR{cfg: cfg, endpoint: endpoint, client: boundedClient(cfg.HTTPClient, cfg.Timeout)}, nil
}

type asrEnvelope struct {
	Response struct {
		Data      json.RawMessage `json:"Data"`
		RequestID string          `json:"RequestId"`
		Error     *struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
	} `json:"Response"`
}

func (p *TencentASR) Submit(ctx context.Context, request ASRRequest) (ASRSubmission, error) {
	u, err := url.Parse(request.AudioURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(request.AudioURL) > 16384 {
		return ASRSubmission{}, &Error{Code: "INVALID_AUDIO_URL"}
	}
	if request.SizeBytes <= 0 || request.SizeBytes > 1<<30 || request.DurationMS <= 0 || request.DurationMS > (5*time.Hour).Milliseconds() {
		return ASRSubmission{}, &Error{Code: "UNSUPPORTED_AUDIO_SIZE_OR_DURATION"}
	}
	if err := ctx.Err(); err != nil {
		return ASRSubmission{}, &Error{Code: "CANCELLED", Cause: err}
	}
	body := map[string]any{"EngineModelType": p.cfg.EngineModel, "ChannelNum": 1, "ResTextFormat": 3, "SourceType": 0, "Url": request.AudioURL}
	env, _, err := p.call(ctx, "CreateRecTask", body)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && (e.StatusCode == 0 || e.StatusCode >= 500 || strings.HasPrefix(e.Code, "InternalError") || e.Code == "INVALID_PROVIDER_RESPONSE" || e.Code == "RESPONSE_READ_ERROR" || e.Code == "RESPONSE_TOO_LARGE") {
			// Once submitted, a lost response cannot prove the task was rejected.
			// Do not automatically retry this request; reconcile externally.
			return ASRSubmission{}, &Error{Code: "SUBMISSION_UNKNOWN", StatusCode: e.StatusCode, RequestID: e.RequestID, SubmissionUnknown: true, Retryable: false, Cause: err}
		}
		return ASRSubmission{}, err
	}
	var data struct {
		TaskID int64 `json:"TaskId"`
	}
	if json.Unmarshal(env.Response.Data, &data) != nil || data.TaskID <= 0 {
		return ASRSubmission{}, &Error{Code: "SUBMISSION_UNKNOWN", SubmissionUnknown: true, RequestID: env.Response.RequestID}
	}
	return ASRSubmission{JobID: strconv.FormatInt(data.TaskID, 10), RequestID: env.Response.RequestID}, nil
}

func (p *TencentASR) Poll(ctx context.Context, jobID string) (ASRResult, error) {
	id, err := strconv.ParseInt(jobID, 10, 64)
	if err != nil || id <= 0 {
		return ASRResult{}, &Error{Code: "INVALID_ASR_JOB_ID"}
	}
	env, raw, err := p.call(ctx, "DescribeTaskStatus", map[string]any{"TaskId": id})
	if err != nil {
		return ASRResult{}, err
	}
	var data struct {
		TaskID        int64   `json:"TaskId"`
		Status        *int    `json:"Status"`
		AudioDuration float64 `json:"AudioDuration"`
		ResultDetail  []struct {
			FinalSentence string `json:"FinalSentence"`
			StartMS       *int64 `json:"StartMs"`
			EndMS         *int64 `json:"EndMs"`
			SpeakerID     int    `json:"SpeakerId"`
		} `json:"ResultDetail"`
	}
	if json.Unmarshal(env.Response.Data, &data) != nil || data.Status == nil || data.TaskID != id || math.IsNaN(data.AudioDuration) || math.IsInf(data.AudioDuration, 0) || data.AudioDuration < 0 {
		return ASRResult{}, &Error{Code: "INVALID_PROVIDER_RESPONSE"}
	}
	out := ASRResult{JobID: jobID, RequestID: env.Response.RequestID, DurationMS: int64(math.Round(data.AudioDuration * 1000)), Raw: append(json.RawMessage(nil), raw...)}
	switch *data.Status {
	case 0:
		out.Status = ASRPending
	case 1:
		out.Status = ASRRunning
	case 2:
		out.Status = ASRSucceeded
	case 3:
		out.Status = ASRFailed
		out.ErrorCode = "RECOGNITION_FAILED"
	default:
		return ASRResult{}, &Error{Code: "INVALID_ASR_STATUS"}
	}
	if out.Status == ASRSucceeded {
		for _, sentence := range data.ResultDetail {
			if strings.TrimSpace(sentence.FinalSentence) == "" {
				continue
			}
			if sentence.StartMS == nil || sentence.EndMS == nil || *sentence.StartMS < 0 || *sentence.EndMS <= *sentence.StartMS {
				return ASRResult{}, &Error{Code: "ASR_TIMESTAMPS_MISSING"}
			}
			out.Sentences = append(out.Sentences, Sentence{StartMS: *sentence.StartMS, EndMS: *sentence.EndMS, Text: sentence.FinalSentence, SpeakerID: sentence.SpeakerID})
		}
		if len(out.Sentences) == 0 {
			out.Status = ASRFailed
			out.ErrorCode = "UNSUPPORTED_NO_SPEECH"
		}
	}
	return out, nil
}

func (p *TencentASR) call(ctx context.Context, action string, input any) (asrEnvelope, []byte, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return asrEnvelope{}, nil, &Error{Code: "INVALID_REQUEST", Cause: err}
	}
	now := p.cfg.Clock().UTC()
	timestamp := strconv.FormatInt(now.Unix(), 10)
	date := now.Format("2006-01-02")
	contentType := "application/json; charset=utf-8"
	canonical := "POST\n/\n\ncontent-type:" + contentType + "\nhost:" + p.endpoint.Host + "\n\ncontent-type;host\n" + sha256Hex(body)
	scope := date + "/asr/tc3_request"
	toSign := "TC3-HMAC-SHA256\n" + timestamp + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
	key := hmacSHA([]byte("TC3"+p.cfg.SecretKey), date)
	key = hmacSHA(key, "asr")
	key = hmacSHA(key, "tc3_request")
	signature := hex.EncodeToString(hmacSHA(key, toSign))
	credential := fmt.Sprintf("TC3-HMAC-SHA256 Credential=%s/%s, SignedHeaders=content-type;host, Signature=%s", p.cfg.SecretID, scope, signature)
	headers := http.Header{"Content-Type": []string{contentType}, "Authorization": []string{credential}, "X-Tc-Action": []string{action}, "X-Tc-Version": []string{"2019-06-14"}, "X-Tc-Timestamp": []string{timestamp}}
	if p.cfg.Region != "" {
		headers.Set("X-TC-Region", p.cfg.Region)
	}
	if p.cfg.SessionToken != "" {
		headers.Set("X-TC-Token", p.cfg.SessionToken)
	}
	raw, _, err := requestJSON(ctx, p.client, strings.TrimRight(p.endpoint.String(), "/")+"/", headers, body, p.cfg.MaxResponseBytes)
	if err != nil {
		return asrEnvelope{}, nil, err
	}
	var env asrEnvelope
	if json.Unmarshal(raw, &env) != nil || env.Response.RequestID == "" {
		return asrEnvelope{}, nil, &Error{Code: "INVALID_PROVIDER_RESPONSE"}
	}
	if env.Response.Error != nil {
		code := env.Response.Error.Code
		retry := strings.HasPrefix(code, "RequestLimitExceeded") || strings.HasPrefix(code, "InternalError") || strings.HasPrefix(code, "ResourceUnavailable")
		return env, raw, &Error{Code: code, StatusCode: 200, RequestID: env.Response.RequestID, Retryable: retry}
	}
	return env, raw, nil
}

func sha256Hex(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func hmacSHA(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(value))
	return mac.Sum(nil)
}
