// Package media runs bounded local media tools. It never invokes a shell or
// accepts model-generated executable arguments.
package media

import (
	"context"
	"time"
)

type Metadata struct {
	DurationMS   int64  `json:"duration_ms"`
	StartMS      int64  `json:"start_ms"`
	VideoStartMS int64  `json:"video_start_ms"`
	AudioStartMS int64  `json:"audio_start_ms"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	VideoCodec   string `json:"video_codec"`
	AudioCodec   string `json:"audio_codec"`
	HasAudio     bool   `json:"has_audio"`
	Size         int64  `json:"size"`
}

type Audio struct {
	Path       string `json:"path"`
	StartMS    int64  `json:"start_ms"`
	DurationMS int64  `json:"duration_ms"`
	Size       int64  `json:"size"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
}

type Frame struct {
	Path        string `json:"path"`
	TimestampMS int64  `json:"timestamp_ms"`
	Size        int64  `json:"size"`
}

type OCRResult struct {
	Text     string `json:"text"`
	Language string `json:"language"`
}

type Engine interface {
	Probe(context.Context, string) (Metadata, error)
	ExtractAudio(context.Context, string, string) (Audio, error)
	ExtractFrames(context.Context, string, string) ([]Frame, error)
	OCR(context.Context, string) (OCRResult, error)
}

type Options struct {
	FFprobePath    string
	FFmpegPath     string
	TesseractPath  string
	OCRLanguage    string
	Timeout        time.Duration
	MaxInputBytes  int64
	MaxOutputBytes int64
	MaxDuration    time.Duration
	FrameInterval  time.Duration
	MaxFrames      int
	MaxFrameWidth  int
	MaxPixels      int64
}

type Error struct {
	Operation string
	Code      string
	Cause     error
}

func (e *Error) Error() string { return "media " + e.Operation + ": " + e.Code }
func (e *Error) Unwrap() error { return e.Cause }
