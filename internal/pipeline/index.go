package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/MOLI0711/MOLIVedio/internal/provider"
	"github.com/MOLI0711/MOLIVedio/internal/retrieval"
	"github.com/MOLI0711/MOLIVedio/internal/workflow"
)

func (p *Pipeline) OCR(ctx context.Context, l *domain.Lease) (domain.StepResult, error) {
	dir, err := p.workdir(l)
	if err != nil {
		return domain.StepResult{}, err
	}
	defer os.RemoveAll(dir)
	frames, err := p.deps.Repository.LoadFrames(ctx, l.Job.VideoVersionID)
	if err != nil {
		return domain.StepResult{}, execution(err)
	}
	if len(frames) == 0 || len(frames) > 256 {
		return domain.StepResult{}, fail("INVALID_FRAME_COUNT", nil)
	}
	failed := 0
	for i := range frames {
		if err = ctx.Err(); err != nil {
			return domain.StepResult{}, err
		}
		f := &frames[i]
		asset, err := p.deps.Repository.GetAsset(ctx, l.Job.OwnerID, f.AssetID)
		if err != nil {
			return domain.StepResult{}, execution(err)
		}
		if asset.VideoVersionID != l.Job.VideoVersionID || asset.OwnerID != l.Job.OwnerID || asset.Kind != "FRAME" {
			return domain.StepResult{}, fail("FRAME_SCOPE_MISMATCH", nil)
		}
		path := filepath.Join(dir, fmt.Sprintf("frame-%06d.jpg", i))
		if err = p.download(ctx, *asset, path); err != nil {
			return domain.StepResult{}, err
		}
		result, ocrErr := p.deps.Media.OCR(ctx, path)
		_ = os.Remove(path)
		if ctx.Err() != nil {
			return domain.StepResult{}, ctx.Err()
		}
		if ocrErr != nil {
			f.OCRStatus = "FAILED"
			f.OCRText = ""
			failed++
			continue
		}
		if len(result.Text) > 64<<10 {
			return domain.StepResult{}, fail("OCR_TEXT_TOO_LARGE", nil)
		}
		f.OCRText = strings.TrimSpace(result.Text)
		f.OCRStatus = "SUCCEEDED"
	}
	return domain.StepResult{Frames: frames, Skipped: failed == len(frames), Metadata: marshal(map[string]any{"frame_count": len(frames), "failed_count": failed, "degraded": failed > 0})}, nil
}

func (p *Pipeline) BuildSegments(ctx context.Context, l *domain.Lease) (domain.StepResult, error) {
	sentences, err := p.deps.Repository.LoadSentences(ctx, l.Job.VideoVersionID)
	if err != nil {
		return domain.StepResult{}, execution(err)
	}
	frames, err := p.deps.Repository.LoadFrames(ctx, l.Job.VideoVersionID)
	if err != nil {
		return domain.StepResult{}, execution(err)
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
	segments, err := Segment(l.Job.VideoVersionID, meta.DurationMS, sentences, frames)
	if err != nil {
		return domain.StepResult{}, err
	}
	return domain.StepResult{Segments: segments, Metadata: marshal(map[string]any{"segment_count": len(segments), "segmenter_version": "sentence-window-v1", "target_duration_ms": 30000, "max_duration_ms": 90000, "max_text_runes": 1800})}, nil
}

// Segment uses complete ASR sentences, avoiding invented text or timestamps.
// A single long sentence is preserved rather than cut at a made-up timestamp.
// The rune cap is a local size heuristic, not a claim about model token counts.
func Segment(version domain.ID, duration int64, sentences []domain.Sentence, frames []domain.Frame) ([]domain.Segment, error) {
	if duration <= 0 || len(sentences) == 0 {
		return nil, fail("UNSUPPORTED_NO_SPEECH", nil)
	}
	sentences = append([]domain.Sentence(nil), sentences...)
	frames = append([]domain.Frame(nil), frames...)
	sort.SliceStable(sentences, func(i, j int) bool { return sentences[i].StartMS < sentences[j].StartMS })
	sort.SliceStable(frames, func(i, j int) bool { return frames[i].TimestampMS < frames[j].TimestampMS })
	seen := map[domain.ID]bool{}
	for _, s := range sentences {
		if s.VideoVersionID != version || s.ID == (domain.ID{}) || seen[s.ID] || s.StartMS < 0 || s.EndMS <= s.StartMS || s.EndMS > duration || strings.TrimSpace(s.Text) == "" {
			return nil, fail("INVALID_SENTENCE_EVIDENCE", nil)
		}
		seen[s.ID] = true
	}
	for _, f := range frames {
		if f.VideoVersionID != version || f.ID == (domain.ID{}) || f.TimestampMS < 0 || f.TimestampMS > duration {
			return nil, fail("INVALID_FRAME_EVIDENCE", nil)
		}
	}
	out := make([]domain.Segment, 0)
	for start := 0; start < len(sentences); {
		end := start + 1
		runes := utf8.RuneCountInString(sentences[start].Text)
		for end < len(sentences) {
			next := sentences[end]
			count := utf8.RuneCountInString(next.Text)
			if next.EndMS-sentences[start].StartMS > 90000 || runes+count > 1800 || next.StartMS-sentences[end-1].EndMS > 15000 {
				break
			}
			if sentences[end-1].EndMS-sentences[start].StartMS >= 30000 && runes >= 600 {
				break
			}
			runes += count
			end++
		}
		group := sentences[start:end]
		seg := domain.Segment{ID: domain.NewID(), VideoVersionID: version, Ordinal: len(out), StartMS: group[0].StartMS, EndMS: group[0].EndMS}
		ids := make([]domain.ID, 0, len(group))
		texts := make([]string, 0, len(group))
		for _, s := range group {
			ids = append(ids, s.ID)
			texts = append(texts, strings.TrimSpace(s.Text))
			seg.EndMS = max(seg.EndMS, s.EndMS)
		}
		seg.Text = strings.Join(texts, "\n")
		seg.SentenceIDs = marshal(ids)
		// A short spoken passage often starts just after the first sampled frame.
		// Keep its nearest preceding visual context when no sampled frame falls
		// inside the speech span; expand the segment to the real frame timestamp.
		inside := false
		previous := int64(-1)
		for _, f := range frames {
			if f.TimestampMS >= seg.StartMS && f.TimestampMS < seg.EndMS {
				inside = true
				break
			}
			if f.TimestampMS < seg.StartMS && seg.StartMS-f.TimestampMS <= 30000 {
				previous = f.TimestampMS
			}
		}
		if !inside && previous >= 0 {
			seg.StartMS = previous
		}
		frameIDs := []domain.ID{}
		ocr := []string{}
		seenText := map[string]bool{}
		for _, f := range frames {
			if f.TimestampMS < seg.StartMS || f.TimestampMS >= seg.EndMS {
				continue
			}
			frameIDs = append(frameIDs, f.ID)
			text := strings.TrimSpace(f.OCRText)
			if f.OCRStatus == "SUCCEEDED" && text != "" && !seenText[text] {
				ocr = append(ocr, text)
				seenText[text] = true
			}
		}
		seg.FrameIDs = marshal(frameIDs)
		seg.OCRText = strings.Join(ocr, "\n")
		if len(seg.Text)+len(seg.OCRText) > 256<<10 {
			return nil, fail("SEGMENT_TEXT_TOO_LARGE", nil)
		}
		out = append(out, seg)
		start = end
	}
	return out, nil
}

func (p *Pipeline) BuildIndex(ctx context.Context, l *domain.Lease) (domain.StepResult, error) {
	if p.deps.Indexer == nil {
		return domain.StepResult{}, fail("SEARCH_DISABLED", nil)
	}
	s, err := config(l)
	if err != nil {
		return domain.StepResult{}, err
	}
	index := s.IndexName
	if index == "" {
		index = p.deps.IndexName
	}
	if index == "" || (s.IndexName != "" && p.deps.IndexName != "" && s.IndexName != p.deps.IndexName) || (s.EmbeddingProfile != "" && s.EmbeddingProfile != p.deps.EmbeddingProfile) {
		return domain.StepResult{}, fail("EMBEDDING_SNAPSHOT_MISMATCH", nil)
	}
	video, err := p.deps.Repository.GetVideo(ctx, l.Job.OwnerID, l.Job.VideoID)
	if err != nil {
		return domain.StepResult{}, execution(err)
	}
	segments, err := p.deps.Repository.LoadSegments(ctx, l.Job.VideoVersionID)
	if err != nil {
		return domain.StepResult{}, execution(err)
	}
	if len(segments) == 0 {
		return domain.StepResult{}, fail("NO_SEGMENTS", nil)
	}
	// Attempt-specific builds make stale workers harmless: only the fenced SQL
	// completion transaction can publish this build ID as the active version.
	build := fmt.Sprintf("%s-%d", l.Step.ID, l.AttemptNo)
	scope := retrieval.Scope{OwnerID: l.Job.OwnerID.String(), VideoID: l.Job.VideoID.String(), ContentVersion: l.Job.VideoVersionID.String(), IndexName: index, IndexBuildID: build}
	docs := make([]retrieval.Document, 0, len(segments))
	for _, seg := range segments {
		if seg.VideoVersionID != l.Job.VideoVersionID {
			return domain.StepResult{}, fail("SEGMENT_SCOPE_MISMATCH", nil)
		}
		var ids []string
		if json.Unmarshal(seg.FrameIDs, &ids) != nil {
			return domain.StepResult{}, fail("INVALID_FRAME_REFERENCES", nil)
		}
		docs = append(docs, retrieval.Document{OwnerID: scope.OwnerID, VideoID: scope.VideoID, ContentVersion: scope.ContentVersion, IndexBuildID: build, SegmentID: seg.ID.String(), StartMS: seg.StartMS, EndMS: seg.EndMS, Transcript: seg.Text, OCRText: seg.OCRText, Title: video.Title, FrameIDs: ids})
	}
	if err = p.deps.Indexer.Build(ctx, scope, docs); err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) {
			return domain.StepResult{}, execution(err)
		}
		return domain.StepResult{}, &workflow.ExecutionError{Code: "INDEX_BUILD_FAILED", Cause: err, Retryable: true}
	}
	return domain.StepResult{IndexName: index, IndexBuildID: build, Metadata: marshal(map[string]any{"document_count": len(docs), "embedding_profile": p.deps.EmbeddingProfile, "index_build_id": build})}, nil
}
