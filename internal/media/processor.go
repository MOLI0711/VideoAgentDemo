package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Processor struct{ opts Options }

func New(opts Options) (*Processor, error) {
	if opts.FFprobePath == "" {
		opts.FFprobePath = "ffprobe"
	}
	if opts.FFmpegPath == "" {
		opts.FFmpegPath = "ffmpeg"
	}
	if opts.TesseractPath == "" {
		opts.TesseractPath = "tesseract"
	}
	if opts.OCRLanguage == "" {
		opts.OCRLanguage = "chi_sim+eng"
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_+]+$`).MatchString(opts.OCRLanguage) {
		return nil, &Error{Code: "INVALID_OCR_LANGUAGE"}
	}
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Minute
	}
	if opts.MaxInputBytes == 0 {
		opts.MaxInputBytes = 2 << 30
	}
	if opts.MaxOutputBytes == 0 {
		opts.MaxOutputBytes = 1 << 30
	}
	if opts.MaxDuration == 0 {
		opts.MaxDuration = 2 * time.Hour
	}
	if opts.FrameInterval == 0 {
		opts.FrameInterval = 30 * time.Second
	}
	if opts.MaxFrames == 0 {
		opts.MaxFrames = 256
	}
	if opts.MaxFrameWidth == 0 {
		opts.MaxFrameWidth = 1280
	}
	if opts.MaxPixels == 0 {
		opts.MaxPixels = 3840 * 2160
	}
	if opts.Timeout < 0 || opts.MaxInputBytes < 1 || opts.MaxOutputBytes < 1 || opts.MaxDuration < 0 || opts.FrameInterval < 0 || opts.MaxFrames < 1 || opts.MaxFrames > 10000 || opts.MaxFrameWidth < 16 || opts.MaxPixels < 1 {
		return nil, &Error{Code: "INVALID_OPTIONS"}
	}
	return &Processor{opts: opts}, nil
}

func (p *Processor) localFile(path string) (string, os.FileInfo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, &Error{Code: "INVALID_PATH", Cause: err}
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", nil, &Error{Code: "INPUT_UNAVAILABLE", Cause: err}
	}
	if !st.Mode().IsRegular() || st.Size() == 0 {
		return "", nil, &Error{Code: "INVALID_INPUT_FILE"}
	}
	if st.Size() > p.opts.MaxInputBytes {
		return "", nil, &Error{Code: "INPUT_TOO_LARGE"}
	}
	return abs, st, nil
}

type probeJSON struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		StartTime string `json:"start_time"`
		Duration  string `json:"duration"`
	} `json:"streams"`
	Format struct {
		Duration  string `json:"duration"`
		StartTime string `json:"start_time"`
	} `json:"format"`
}

func milliseconds(s string) (int64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 1e12 {
		return 0, errors.New("invalid media time")
	}
	return int64(math.Round(v * 1000)), nil
}

func (p *Processor) Probe(ctx context.Context, path string) (Metadata, error) {
	abs, st, err := p.localFile(path)
	if err != nil {
		return Metadata{}, err
	}
	out, _, err := p.run(ctx, "probe", p.opts.FFprobePath, "-v", "error", "-show_format", "-show_streams", "-of", "json", "-i", abs)
	if err != nil {
		return Metadata{}, err
	}
	var doc probeJSON
	if json.Unmarshal(out, &doc) != nil {
		return Metadata{}, &Error{Operation: "probe", Code: "INVALID_TOOL_RESPONSE"}
	}
	duration, _ := milliseconds(doc.Format.Duration)
	start, _ := milliseconds(doc.Format.StartTime)
	meta := Metadata{DurationMS: duration, StartMS: start, Size: st.Size()}
	var streamEnd int64
	videoDurationKnown, audioDurationKnown := false, false
	for _, s := range doc.Streams {
		switch s.CodecType {
		case "video":
			if meta.VideoCodec != "" {
				continue
			}
			meta.VideoCodec = s.CodecName
			meta.Width = s.Width
			meta.Height = s.Height
			if t, e := milliseconds(s.StartTime); e == nil {
				meta.VideoStartMS = t - start
			}
			if d, e := milliseconds(s.Duration); e == nil && d > 0 {
				streamEnd = max(streamEnd, meta.VideoStartMS+d)
				videoDurationKnown = true
			}
		case "audio":
			if meta.HasAudio {
				continue
			}
			meta.HasAudio = true
			meta.AudioCodec = s.CodecName
			if t, e := milliseconds(s.StartTime); e == nil {
				meta.AudioStartMS = t - start
			}
			if d, e := milliseconds(s.Duration); e == nil && d > 0 {
				streamEnd = max(streamEnd, meta.AudioStartMS+d)
				audioDurationKnown = true
			}
		}
	}
	// MP4 with a nonzero edit-list origin can report format.duration as an end
	// timestamp. Complete stream durations give the actual playable relative span.
	// Formats without these fields retain their container duration fallback.
	if videoDurationKnown && (!meta.HasAudio || audioDurationKnown) {
		meta.DurationMS = streamEnd
	}
	if meta.DurationMS <= 0 || meta.DurationMS > p.opts.MaxDuration.Milliseconds() {
		return Metadata{}, &Error{Operation: "probe", Code: "INVALID_DURATION"}
	}
	if meta.VideoCodec == "" || meta.Width < 1 || meta.Height < 1 || int64(meta.Width)*int64(meta.Height) > p.opts.MaxPixels {
		return Metadata{}, &Error{Operation: "probe", Code: "UNSUPPORTED_VIDEO"}
	}
	return meta, nil
}

func (p *Processor) ExtractAudio(ctx context.Context, input, output string) (Audio, error) {
	meta, err := p.Probe(ctx, input)
	if err != nil {
		return Audio{}, err
	}
	if !meta.HasAudio {
		return Audio{}, &Error{Operation: "audio", Code: "UNSUPPORTED_NO_SPEECH"}
	}
	in, _ := filepath.Abs(input)
	out, err := prepareOutput(output)
	if err != nil {
		return Audio{}, err
	}
	_, _, err = p.run(ctx, "audio", p.opts.FFmpegPath, "-hide_banner", "-loglevel", "error", "-nostdin", "-n", "-i", in, "-map", "0:a:0", "-vn", "-af", "asetpts=PTS-STARTPTS,aresample=async=1:first_pts=0", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-fs", strconv.FormatInt(p.opts.MaxOutputBytes+1, 10), "-f", "wav", out)
	if err != nil {
		_ = os.Remove(out)
		return Audio{}, err
	}
	st, err := os.Stat(out)
	if err != nil {
		return Audio{}, &Error{Operation: "audio", Code: "OUTPUT_MISSING", Cause: err}
	}
	if st.Size() == 0 || st.Size() > p.opts.MaxOutputBytes {
		_ = os.Remove(out)
		return Audio{}, &Error{Operation: "audio", Code: "OUTPUT_TOO_LARGE"}
	}
	data, _, err := p.run(ctx, "audio_probe", p.opts.FFprobePath, "-v", "error", "-show_entries", "format=duration", "-of", "json", "-i", out)
	if err != nil {
		return Audio{}, err
	}
	var doc probeJSON
	if json.Unmarshal(data, &doc) != nil {
		return Audio{}, &Error{Operation: "audio", Code: "INVALID_TOOL_RESPONSE"}
	}
	duration, err := milliseconds(doc.Format.Duration)
	if err != nil || duration <= 0 {
		return Audio{}, &Error{Operation: "audio", Code: "INVALID_DURATION"}
	}
	return Audio{Path: out, StartMS: meta.AudioStartMS, DurationMS: duration, Size: st.Size(), SampleRate: 16000, Channels: 1}, nil
}

func prepareOutput(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", &Error{Code: "INVALID_OUTPUT_PATH", Cause: err}
	}
	if _, err = os.Lstat(abs); err == nil {
		return "", &Error{Code: "OUTPUT_ALREADY_EXISTS"}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", &Error{Code: "OUTPUT_UNAVAILABLE", Cause: err}
	}
	if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return "", &Error{Code: "OUTPUT_UNAVAILABLE", Cause: err}
	}
	return abs, nil
}

var ptsPattern = regexp.MustCompile(`\bn:\s*\d+\s+pts:.*?\bpts_time:([-+0-9.eE]+)`)

func (p *Processor) ExtractFrames(ctx context.Context, input, outDir string) ([]Frame, error) {
	meta, err := p.Probe(ctx, input)
	if err != nil {
		return nil, err
	}
	in, _ := filepath.Abs(input)
	dir, err := filepath.Abs(outDir)
	if err != nil {
		return nil, &Error{Code: "INVALID_OUTPUT_PATH", Cause: err}
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, &Error{Code: "OUTPUT_UNAVAILABLE", Cause: err}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "frame-*.jpg"))
	if len(files) != 0 {
		return nil, &Error{Code: "OUTPUT_ALREADY_EXISTS"}
	}
	// A duration-dependent minimum gap prevents scene changes from exhausting
	// the frame cap near the start and silently omitting the end of a long video.
	minGap := float64(meta.DurationMS) / 1000 / float64(p.opts.MaxFrames)
	interval := math.Max(p.opts.FrameInterval.Seconds(), minGap)
	filter := fmt.Sprintf("select=eq(n\\,0)+gte(t-prev_selected_t\\,%.6f)+gt(scene\\,0.35)*gte(t-prev_selected_t\\,%.6f),scale=w='min(%d,iw)':h=-2,showinfo", interval, minGap, p.opts.MaxFrameWidth)
	_, logs, err := p.run(ctx, "frames", p.opts.FFmpegPath, "-hide_banner", "-loglevel", "info", "-nostdin", "-n", "-copyts", "-i", in, "-map", "0:v:0", "-an", "-vf", filter, "-fps_mode", "vfr", "-frames:v", strconv.Itoa(p.opts.MaxFrames), "-q:v", "3", filepath.Join(dir, "frame-%06d.jpg"))
	if err != nil {
		return nil, err
	}
	return p.readFrames(dir, logs, meta)
}

func (p *Processor) readFrames(dir string, logs []byte, meta Metadata) ([]Frame, error) {
	pts := ptsPattern.FindAllSubmatch(logs, -1)
	files, err := filepath.Glob(filepath.Join(dir, "frame-*.jpg"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 || len(files) != len(pts) || len(files) > p.opts.MaxFrames {
		return nil, &Error{Operation: "frames", Code: "FRAME_TIMESTAMPS_MISSING"}
	}
	result := make([]Frame, 0, len(files))
	var total int64
	for i, path := range files {
		t, err := milliseconds(string(pts[i][1]))
		if err != nil {
			return nil, &Error{Operation: "frames", Code: "INVALID_TIMESTAMP"}
		}
		t -= meta.StartMS
		if t < 0 && t >= -1 {
			t = 0
		}
		if t < 0 || t > meta.DurationMS+1000 {
			return nil, &Error{Operation: "frames", Code: "INVALID_TIMESTAMP"}
		}
		st, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		total += st.Size()
		if st.Size() == 0 || total > p.opts.MaxOutputBytes {
			return nil, &Error{Operation: "frames", Code: "OUTPUT_TOO_LARGE"}
		}
		result = append(result, Frame{Path: path, TimestampMS: t, Size: st.Size()})
	}
	return result, nil
}

func (p *Processor) OCR(ctx context.Context, path string) (OCRResult, error) {
	abs, _, err := p.localFile(path)
	if err != nil {
		return OCRResult{}, err
	}
	out, _, err := p.run(ctx, "ocr", p.opts.TesseractPath, abs, "stdout", "-l", p.opts.OCRLanguage, "--psm", "3")
	if err != nil {
		return OCRResult{}, err
	}
	return OCRResult{Text: strings.TrimSpace(string(out)), Language: p.opts.OCRLanguage}, nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
	cancel   context.CancelFunc
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	if b.Len()+len(data) > b.limit {
		b.exceeded = true
		b.cancel()
		return 0, io.ErrShortWrite
	}
	return b.Buffer.Write(data)
}

func (p *Processor) run(parent context.Context, op, executable string, args ...string) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(parent, p.opts.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	configureCancellation(cmd)
	stdout := &limitedBuffer{limit: 4 << 20, cancel: cancel}
	stderr := &limitedBuffer{limit: 4 << 20, cancel: cancel}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if stdout.exceeded || stderr.exceeded {
		return nil, nil, &Error{Operation: op, Code: "TOOL_OUTPUT_TOO_LARGE"}
	}
	if err != nil {
		code := "EXEC_FAILED"
		if errors.Is(parent.Err(), context.Canceled) {
			code = "CANCELLED"
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = "TOOL_TIMEOUT"
		}
		return nil, nil, &Error{Operation: op, Code: code, Cause: err}
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}
