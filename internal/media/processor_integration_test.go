package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This opt-in test requires actual tools. In a container setup only the dedicated
// VA_TEST_MEDIA_ROOT fixture directory needs to be mounted, never the repository.
func TestMediaIntegration(t *testing.T) {
	root := os.Getenv("VA_TEST_MEDIA_ROOT")
	if root == "" {
		t.Skip("set VA_TEST_MEDIA_ROOT and optional tool paths for real media integration")
	}
	dir, err := os.MkdirTemp(root, "processor-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	tool := func(name string) string {
		if v := os.Getenv("VA_TEST_" + strings.ToUpper(name)); v != "" {
			return v
		}
		return name
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	source := filepath.Join(dir, "synthetic-offset.mp4")
	// Synthetic image and sine tone are deliberately not an ASR accuracy fixture.
	args := []string{"-nostdin", "-n", "-f", "lavfi", "-i", "color=c=white:s=640x360:r=7:d=4", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=16000:duration=4", "-vf", "drawtext=fontfile=/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf:text='SYNTHETIC FIXTURE 42':fontcolor=black:fontsize=32:x=40:y=150", "-c:v", "mpeg4", "-bf", "0", "-c:a", "aac", "-output_ts_offset", "5", source}
	cmd := exec.CommandContext(ctx, tool("ffmpeg"), args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate synthetic media: %v: %s", err, string(output))
	}
	p, err := New(Options{FFmpegPath: tool("ffmpeg"), FFprobePath: tool("ffprobe"), TesseractPath: tool("tesseract"), OCRLanguage: "eng", FrameInterval: time.Second, MaxFrames: 20, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := p.Probe(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if !metadata.HasAudio || metadata.StartMS < 4000 || metadata.Width != 640 || metadata.DurationMS < 3900 || metadata.DurationMS > 4300 {
		t.Fatalf("unexpected actual metadata: %+v", metadata)
	}
	audio, err := p.ExtractAudio(ctx, source, filepath.Join(dir, "audio.wav"))
	if err != nil {
		t.Fatal(err)
	}
	if audio.SampleRate != 16000 || audio.Channels != 1 || audio.DurationMS < 3900 || audio.DurationMS > 4200 {
		t.Fatalf("wrong extracted audio: %+v", audio)
	}
	frames, err := p.ExtractFrames(ctx, source, filepath.Join(dir, "frames"))
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < 3 || frames[0].TimestampMS < 0 || frames[0].TimestampMS > 250 {
		t.Fatalf("wrong real frame PTS: %+v", frames)
	}
	for i := 1; i < len(frames); i++ {
		if frames[i].TimestampMS <= frames[i-1].TimestampMS {
			t.Fatal("frame timestamps are not increasing")
		}
	}
	ocr, err := p.OCR(ctx, frames[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ocr.Text, "42") || !strings.Contains(strings.ToUpper(ocr.Text), "FIXTURE") {
		t.Fatalf("synthetic OCR assertion failed: %q", ocr.Text)
	}
	t.Logf("real tools verified: source_start_ms=%d, duration_ms=%d, audio_start_ms=%d, audio_duration_ms=%d, frames=%d, first_pts_ms=%d", metadata.StartMS, metadata.DurationMS, audio.StartMS, audio.DurationMS, len(frames), frames[0].TimestampMS)
}
