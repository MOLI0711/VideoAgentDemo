package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func executable(t *testing.T, dir, name, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
func inputFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("synthetic media fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

const probeFixture = `printf '%s' '{"streams":[{"codec_type":"video","codec_name":"h264","width":640,"height":480,"start_time":"10"},{"codec_type":"audio","codec_name":"aac","start_time":"10.25"}],"format":{"duration":"60","start_time":"10"}}'
`

func TestMediaUsesActualPTSAndKeepsAudioOffset(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	dir := t.TempDir()
	t.Setenv("MEDIA_ARGS_PATH", filepath.Join(dir, "args.txt"))
	probe := executable(t, dir, "fake-ffprobe", probeFixture)
	ffmpeg := executable(t, dir, "fake-ffmpeg", `for arg; do printf '%s\n' "$arg"; last=$arg; done > "$MEDIA_ARGS_PATH"
case "$last" in
  *.wav) printf 'fixture-wave' > "$last" ;;
  *) outdir=$(dirname "$last")
     printf 'frame-one' > "$outdir/frame-000001.jpg"
     printf 'frame-two' > "$outdir/frame-000002.jpg"
     printf '[Parsed_showinfo_2] n:   0 pts: 10000 pts_time:10 pos:1\n[Parsed_showinfo_2] n: 1 pts: 12500 pts_time:12.5 pos:2\n' >&2 ;;
esac
`)
	ocr := executable(t, dir, "fake-tesseract", `printf 'synthetic OCR fixture\n'`)
	processor, err := New(Options{FFprobePath: probe, FFmpegPath: ffmpeg, TesseractPath: ocr})
	if err != nil {
		t.Fatal(err)
	}
	input := inputFile(t, dir, "input;touch-should-not-run.mp4")
	meta, err := processor.Probe(context.Background(), input)
	if err != nil || meta.StartMS != 10000 || meta.AudioStartMS != 250 || meta.DurationMS != 60000 {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
	audio, err := processor.ExtractAudio(context.Background(), input, filepath.Join(dir, "audio.wav"))
	if err != nil || audio.StartMS != 250 || audio.SampleRate != 16000 {
		t.Fatalf("audio=%+v err=%v", audio, err)
	}
	frames, err := processor.ExtractFrames(context.Background(), input, filepath.Join(dir, "frames"))
	if err != nil || len(frames) != 2 || frames[0].TimestampMS != 0 || frames[1].TimestampMS != 2500 {
		t.Fatalf("frames=%+v err=%v", frames, err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
	if !strings.Contains(string(args), input+"\n") || !strings.Contains(string(args), "-copyts\n") {
		t.Fatalf("not fixed argv: %s", args)
	}
	result, err := processor.OCR(context.Background(), frames[0].Path)
	if err != nil || result.Text != "synthetic OCR fixture" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err = processor.ExtractAudio(context.Background(), input, audio.Path); err == nil {
		t.Fatal("existing output overwritten")
	}
}

func TestMediaRejectsMissingFrameTimesAndOversizedInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	dir := t.TempDir()
	input := inputFile(t, dir, "input.mp4")
	processor, _ := New(Options{MaxInputBytes: 1})
	_, err := processor.Probe(context.Background(), input)
	var me *Error
	if !errors.As(err, &me) || me.Code != "INPUT_TOO_LARGE" {
		t.Fatalf("error=%v", err)
	}
	processor, _ = New(Options{FFprobePath: executable(t, dir, "probe", probeFixture), FFmpegPath: executable(t, dir, "ffmpeg", `for arg; do last=$arg; done
outdir=$(dirname "$last")
printf 'frame' > "$outdir/frame-000001.jpg"
`)})
	_, err = processor.ExtractFrames(context.Background(), input, filepath.Join(dir, "frames"))
	if !errors.As(err, &me) || me.Code != "FRAME_TIMESTAMPS_MISSING" {
		t.Fatalf("error=%v", err)
	}
}

func TestMediaTimeoutCancelsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	dir := t.TempDir()
	input := inputFile(t, dir, "input.mp4")
	processor, _ := New(Options{FFprobePath: executable(t, dir, "probe", `sleep 30 &
wait
`), Timeout: 80 * time.Millisecond})
	start := time.Now()
	_, err := processor.Probe(context.Background(), input)
	var me *Error
	if !errors.As(err, &me) || me.Code != "TOOL_TIMEOUT" {
		t.Fatalf("error=%v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancel failed to terminate tool and its output pipes")
	}
}

func TestProbeUsesStreamSpanForNonzeroMP4Origin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	dir := t.TempDir()
	probe := executable(t, dir, "probe-span", `printf '%s' '{"streams":[{"codec_type":"video","codec_name":"mpeg4","width":640,"height":360,"start_time":"5","duration":"4"},{"codec_type":"audio","codec_name":"aac","start_time":"4.936","duration":"4.064"}],"format":{"duration":"9","start_time":"4.936"}}'`)
	p, err := New(Options{FFprobePath: probe, MaxDuration: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	m, err := p.Probe(context.Background(), inputFile(t, dir, "offset.mp4"))
	if err != nil || m.DurationMS != 4064 || m.StartMS != 4936 || m.VideoStartMS != 64 {
		t.Fatalf("metadata=%+v err=%v", m, err)
	}
}

func TestNoAudioCannotYieldFabricatedTranscript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	dir := t.TempDir()
	input := inputFile(t, dir, "silent.mp4")
	processor, _ := New(Options{FFprobePath: executable(t, dir, "probe", `printf '%s' '{"streams":[{"codec_type":"video","codec_name":"h264","width":640,"height":480}],"format":{"duration":"3","start_time":"0"}}'`)})
	_, err := processor.ExtractAudio(context.Background(), input, filepath.Join(dir, "audio.wav"))
	var me *Error
	if !errors.As(err, &me) || me.Code != "UNSUPPORTED_NO_SPEECH" {
		t.Fatalf("error=%v", err)
	}
}
