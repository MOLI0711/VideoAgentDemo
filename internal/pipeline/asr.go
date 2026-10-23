package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/MOLI0711/MOLIVedio/internal/provider"
)

func (p *Pipeline) audio(ctx context.Context, l *domain.Lease) (domain.Asset, error) {
	assets, err := p.deps.Repository.ListAssets(ctx, l.Job.OwnerID, l.Job.VideoID, l.Job.VideoVersionID, "AUDIO")
	if err != nil {
		return domain.Asset{}, execution(err)
	}
	if len(assets) != 1 {
		return domain.Asset{}, fail("AUDIO_ASSET_MISSING_OR_AMBIGUOUS", nil)
	}
	a := assets[0]
	if a.OwnerID != l.Job.OwnerID || a.VideoVersionID != l.Job.VideoVersionID || a.EndMS <= a.StartMS {
		return domain.Asset{}, fail("AUDIO_ASSET_INVALID", nil)
	}
	return a, nil
}

func (p *Pipeline) Transcribe(ctx context.Context, l *domain.Lease) (domain.StepResult, error) {
	if p.deps.ASRMode == "disabled" || p.deps.ASR == nil {
		return domain.StepResult{}, fail("ASR_DISABLED", nil)
	}
	s, err := config(l)
	if err != nil {
		return domain.StepResult{}, err
	}
	if s.ASRMode != "" && s.ASRMode != p.deps.ASRMode {
		return domain.StepResult{}, fail("ASR_SNAPSHOT_MISMATCH", nil)
	}
	audio, err := p.audio(ctx, l)
	if err != nil {
		return domain.StepResult{}, err
	}
	// The persisted provider job is authoritative after any restart. Never submit
	// a replacement job because an execution attempt or presigned URL changed.
	if l.Step.CurrentExternalJobID != (domain.ID{}) {
		ext, err := p.deps.Repository.GetExternalJob(ctx, l.Step.CurrentExternalJobID)
		if err != nil {
			return domain.StepResult{}, execution(err)
		}
		return p.resumeASR(ctx, l, audio, ext)
	}
	dir, err := p.workdir(l)
	if err != nil {
		return domain.StepResult{}, err
	}
	defer os.RemoveAll(dir)
	stageStore := p.deps.ASRStore
	stageKey := audio.ObjectKey
	if stageStore == nil {
		stageStore = p.deps.Objects
	} else {
		path := filepath.Join(dir, "audio.wav")
		if err = p.download(ctx, audio, path); err != nil {
			return domain.StepResult{}, err
		}
		f, err := os.Open(path)
		if err != nil {
			return domain.StepResult{}, execution(err)
		}
		stageKey = prefix(l) + "asr-input.wav"
		_, err = stageStore.Put(ctx, stageKey, "audio/wav", f, audio.Size)
		closeErr := f.Close()
		if err != nil {
			return domain.StepResult{}, execution(err)
		}
		if closeErr != nil {
			return domain.StepResult{}, execution(closeErr)
		}
	}
	signed, err := stageStore.SignGet(ctx, stageKey, 24*time.Hour)
	if err != nil {
		return domain.StepResult{}, execution(err)
	}
	if p.deps.ASRMode == "live" && !cloudURL(signed.URL) {
		return domain.StepResult{}, fail("ASR_STAGING_NOT_PUBLIC_HTTPS", nil)
	}
	// Do not persist signed bearer URLs. The staged key is enough to audit input.
	request := marshal(map[string]any{"audio_asset_id": audio.ID, "staged_key": stageKey, "start_ms": audio.StartMS, "duration_ms": audio.EndMS - audio.StartMS, "size_bytes": audio.Size, "mock": p.deps.ASRMode == "mock"})
	ext, created, err := p.deps.Repository.BeginExternal(ctx, *l, p.deps.ASRProviderRef, request)
	if err != nil {
		return domain.StepResult{}, execution(err)
	}
	if !created {
		return p.resumeASR(ctx, l, audio, ext)
	}
	if err = ctx.Err(); err != nil {
		return domain.StepResult{}, err
	}
	submitted, err := p.deps.ASR.Submit(ctx, provider.ASRRequest{AudioURL: signed.URL, AudioSHA256: strings.TrimPrefix(audio.Checksum, "sha256:"), ClientRequestID: ext.ClientRequestID, DurationMS: audio.EndMS - audio.StartMS, SizeBytes: audio.Size})
	if err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) && pe.SubmissionUnknown {
			return domain.StepResult{}, domain.ErrSubmissionUnknown
		}
		// A definite provider rejection is terminal for this submission generation.
		// Creating a new paid job requires a new explicitly requested processing job.
		if errors.As(err, &pe) {
			return domain.StepResult{}, fail(pe.Code, pe)
		}
		return domain.StepResult{}, domain.ErrSubmissionUnknown
	}
	if submitted.JobID == "" || submitted.Mock != (p.deps.ASRMode == "mock") {
		return domain.StepResult{}, domain.ErrSubmissionUnknown
	}
	return p.waitFor(ext.ID, submitted.JobID), nil
}

func cloudURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()) {
		return false
	}
	return true
}

func (p *Pipeline) waitFor(id domain.ID, jobID string) domain.StepResult {
	return domain.StepResult{ExternalWait: &domain.ExternalWait{ExternalJobID: id, ProviderJobID: jobID, PollAfter: p.deps.PollInterval}}
}

func (p *Pipeline) resumeASR(ctx context.Context, l *domain.Lease, audio domain.Asset, ext *domain.ExternalJob) (domain.StepResult, error) {
	if ext == nil || ext.StepID != l.Step.ID || ext.ProviderRef != p.deps.ASRProviderRef {
		return domain.StepResult{}, fail("EXTERNAL_JOB_SCOPE_MISMATCH", nil)
	}
	if ext.Status == "SUBMISSION_UNKNOWN" || ext.Status == "UNKNOWN" || ext.ProviderJobID == "" {
		return domain.StepResult{}, domain.ErrSubmissionUnknown
	}
	if !ext.DeadlineAt.IsZero() && !time.Now().Before(ext.DeadlineAt) {
		return domain.StepResult{}, fail("ASR_DEADLINE_EXCEEDED", nil)
	}
	if ext.Status == "FAILED" || ext.Status == "CANCELLED" {
		return domain.StepResult{}, fail("ASR_TERMINAL_FAILURE", nil)
	}
	result, err := p.deps.ASR.Poll(ctx, ext.ProviderJobID)
	if err != nil {
		if ctx.Err() != nil {
			return domain.StepResult{}, ctx.Err()
		}
		var pe *provider.Error
		if errors.As(err, &pe) && pe.Retryable {
			return p.waitFor(ext.ID, ext.ProviderJobID), nil
		}
		return domain.StepResult{}, execution(err)
	}
	if result.JobID != ext.ProviderJobID || result.Mock != (p.deps.ASRMode == "mock") {
		return domain.StepResult{}, fail("ASR_RESPONSE_MISMATCH", nil)
	}
	switch result.Status {
	case provider.ASRPending, provider.ASRRunning:
		return p.waitFor(ext.ID, ext.ProviderJobID), nil
	case provider.ASRFailed:
		code := result.ErrorCode
		if code == "" {
			code = "ASR_FAILED"
		}
		return domain.StepResult{}, fail(code, nil)
	case provider.ASRSucceeded:
	default:
		return domain.StepResult{}, fail("INVALID_ASR_STATUS", nil)
	}
	if len(result.Sentences) == 0 {
		return domain.StepResult{}, fail("UNSUPPORTED_NO_SPEECH", nil)
	}
	version, err := p.deps.Repository.GetVersion(ctx, l.Job.OwnerID, l.Job.VideoVersionID)
	if err != nil {
		return domain.StepResult{}, execution(err)
	}
	var meta struct {
		DurationMS int64 `json:"duration_ms"`
	}
	if json.Unmarshal(version.Metadata, &meta) != nil || meta.DurationMS <= 0 {
		return domain.StepResult{}, fail("VIDEO_METADATA_MISSING", nil)
	}
	out := domain.StepResult{}
	textBytes := 0
	for _, s := range result.Sentences {
		start, end := audio.StartMS+s.StartMS, audio.StartMS+s.EndMS
		// ASR times are relative to extracted audio. Offset once into the original
		// video timeline; never infer times from text length or frame index.
		if s.StartMS < 0 || s.EndMS <= s.StartMS || s.EndMS > audio.EndMS-audio.StartMS+250 || start < 0 || end > meta.DurationMS+250 {
			return domain.StepResult{}, fail("ASR_TIMESTAMP_OUT_OF_RANGE", nil)
		}
		end = min(end, meta.DurationMS)
		if end <= start {
			return domain.StepResult{}, fail("ASR_TIMESTAMP_OUT_OF_RANGE", nil)
		}
		text := strings.TrimSpace(s.Text)
		textBytes += len(text)
		if len(text) > 64<<10 || textBytes > 4<<20 {
			return domain.StepResult{}, fail("ASR_TEXT_TOO_LARGE", nil)
		}
		if text != "" {
			out.Sentences = append(out.Sentences, domain.Sentence{ID: domain.NewID(), VideoVersionID: l.Job.VideoVersionID, StartMS: start, EndMS: end, Text: text})
		}
	}
	if len(out.Sentences) == 0 {
		return domain.StepResult{}, fail("UNSUPPORTED_NO_SPEECH", nil)
	}
	dir, err := p.workdir(l)
	if err != nil {
		return domain.StepResult{}, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "transcript.json")
	if err = os.WriteFile(path, marshal(result), 0600); err != nil {
		return domain.StepResult{}, execution(err)
	}
	asset, err := p.upload(ctx, l, path, "transcript.json", "TRANSCRIPT", "application/json", audio.StartMS, audio.EndMS, marshal(map[string]any{"provider_job_id": ext.ProviderJobID, "mock": result.Mock}))
	if err != nil {
		return domain.StepResult{}, err
	}
	out.Assets = []domain.Asset{asset}
	out.Metadata = marshal(map[string]any{"sentence_count": len(out.Sentences), "audio_start_ms": audio.StartMS, "mock": result.Mock})
	return out, nil
}
