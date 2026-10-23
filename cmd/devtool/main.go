package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/MOLI0711/MOLIVedio/internal/app"
	"github.com/MOLI0711/MOLIVedio/internal/config"
	db "github.com/MOLI0711/MOLIVedio/internal/repository/mysql"
	"github.com/MOLI0711/MOLIVedio/internal/storage"
	"github.com/twmb/franz-go/pkg/kgo"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "configs/local.yaml", "configuration path")
	action := flag.String("action", "check", "init-env, migrate, seed, init-storage, check, web-config, or provider-check")
	flag.Parse()
	if *action == "init-env" {
		return initEnv()
	}
	c, e := config.Load(*path)
	if e != nil {
		return e
	}
	if *action == "web-config" {
		host, port, err := net.SplitHostPort(c.App.HTTPAddr)
		if err != nil {
			return fmt.Errorf("invalid app.http_addr")
		}
		if host == "0.0.0.0" || host == "::" || host == "" {
			host = "127.0.0.1"
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"api_target": "http://" + net.JoinHostPort(host, port), "public_origin": c.App.PublicOrigin})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if *action == "migrate" || *action == "seed" {
		dsn, e := c.DSN()
		if e != nil {
			return e
		}
		s, e := db.Open(ctx, dsn)
		if e != nil {
			return fmt.Errorf("MySQL unavailable")
		}
		defer s.Close()
		if *action == "migrate" {
			if e = s.Migrate(ctx); e != nil {
				return e
			}
			fmt.Println("Schema migrations applied.")
			return nil
		}
		fmt.Println("Passwordless visitors are enabled. Each visitor receives a default published agent automatically; no demo account or seed password is needed.")
		return nil
	}
	r, e := app.Open(ctx, c)
	if e != nil {
		return e
	}
	defer r.Close()
	switch *action {
	case "init-storage":
		if e = r.Objects.EnsureBucket(ctx); e != nil {
			return e
		}
		if e = r.Objects.ConfigureCORS(ctx, c.App.PublicOrigin); e != nil {
			var remote *storage.Error
			if !errors.As(e, &remote) || remote.Code != "NotImplemented" {
				return e
			}
			if er := verifyStorageCORS(ctx, c); er != nil {
				return er
			}
			fmt.Println("This S3 server configures CORS at startup; browser preflight verified.")
		}
		fmt.Println("Private object bucket and browser CORS initialized.")
	case "check":
		for _, tool := range []struct{ path, arg string }{{c.Media.FFmpeg, "-version"}, {c.Media.FFprobe, "-version"}, {c.Media.Tesseract, "--version"}} {
			if e = exec.CommandContext(ctx, tool.path, tool.arg).Run(); e != nil {
				return fmt.Errorf("media binary cannot execute: %s", tool.path)
			}
		}
		languages, e := exec.CommandContext(ctx, c.Media.Tesseract, "--list-langs").Output()
		if e != nil {
			return fmt.Errorf("cannot inspect OCR language packs")
		}
		installed := strings.Fields(string(languages))
		for _, language := range strings.Split(c.Media.Languages, "+") {
			found := false
			for _, item := range installed {
				found = found || language == item
			}
			if !found {
				return fmt.Errorf("OCR language pack unavailable: %s", language)
			}
		}
		if e = r.Objects.Ping(ctx); e != nil {
			return fmt.Errorf("S3 bucket is unavailable; run storage-init and check configuration")
		}
		if c.Workflow.DispatchMode == "kafka" {
			client, err := kgo.NewClient(kgo.SeedBrokers(c.Kafka.Brokers...))
			if err != nil {
				return fmt.Errorf("invalid Kafka configuration")
			}
			probeCtx, probeCancel := context.WithTimeout(ctx, 10*time.Second)
			err = client.Ping(probeCtx)
			probeCancel()
			client.Close()
			if err != nil {
				return fmt.Errorf("Kafka is unavailable")
			}
		}
		if r.Search != nil {
			if e = r.Search.Ping(ctx); e != nil {
				return e
			}
		}
		fmt.Printf("Local checks passed: MySQL, S3, media binaries and OCR languages; Redis=%t dispatch=%s search=%t.\n", c.Redis.Enabled, c.Workflow.DispatchMode, c.Search.Enabled)
		fmt.Printf("Provider configuration: model=%s ASR=%s. No cloud model was called.\n", c.Model.Mode, c.ASR.Mode)
	case "provider-check":
		if r.Model == nil {
			return fmt.Errorf("model is disabled")
		}
		embedding, e := r.Model.Embed(ctx, []string{"Connectivity check; no user data."})
		if e != nil {
			return e
		}
		if len(embedding.Vectors) != 1 || len(embedding.Vectors[0]) != c.Search.Dimensions {
			return fmt.Errorf("embedding dimensions mismatch")
		}
		fmt.Printf("Embedding check passed: dimensions=%d mock=%t; chat/tool/vision/ASR still require fixture validation.\n", len(embedding.Vectors[0]), embedding.Mock)
	default:
		return fmt.Errorf("unknown action")
	}
	return nil
}
func secret() string {
	b := make([]byte, 24)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func initEnv() error {
	if e := os.MkdirAll(".runtime", 0700); e != nil {
		return e
	}
	path := filepath.Join(".runtime", "dev.env")
	if _, e := os.Stat(path); e == nil {
		return fmt.Errorf("%s already exists; preserving credentials", path)
	}
	password, access, key := secret(), secret(), secret()
	data := fmt.Sprintf("MYSQL_ROOT_PASSWORD=%s\nMYSQL_DATABASE=video_agent\nVA_MYSQL_DSN=root:%s@tcp(127.0.0.1:13306)/video_agent?parseTime=true&loc=UTC\nVA_S3_ACCESS_KEY=%s\nVA_S3_SECRET_KEY=%s\n", password, password, access, key)
	if e := os.WriteFile(path, []byte(data), 0600); e != nil {
		return e
	}
	identity := map[string]any{"identities": []any{map[string]any{"name": "video-agent", "credentials": []any{map[string]string{"accessKey": access, "secretKey": key}}, "actions": []string{"Admin", "Read", "Write", "List", "Tagging"}}}}
	b, _ := json.MarshalIndent(identity, "", "  ")
	if e := os.WriteFile(".runtime/s3.json", b, 0600); e != nil {
		return e
	}
	fmt.Println("Created private .runtime/dev.env and .runtime/s3.json. No secrets printed.")
	return nil
}

// The probe runs inside the worker network; the browser endpoint may point at
// host loopback and cannot be contacted from this container. No signed URL is
// rewritten here: this is an unsigned OPTIONS check against the same S3 server.
func verifyStorageCORS(ctx context.Context, c config.Config) error {
	req, err := http.NewRequestWithContext(ctx, "OPTIONS", c.Storage.InternalEndpoint+"/"+c.Storage.Bucket, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Origin", c.App.PublicOrigin)
	req.Header.Set("Access-Control-Request-Method", "PUT")
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("S3 CORS preflight failed; check the internal storage endpoint")
	}
	defer res.Body.Close()
	origin := res.Header.Get("Access-Control-Allow-Origin")
	if res.StatusCode < 200 || res.StatusCode >= 300 || (origin != "*" && origin != c.App.PublicOrigin) {
		return fmt.Errorf("this S3 server needs startup CORS configuration; set SeaweedFS -s3.allowedOrigins to app.public_origin")
	}
	return nil
}
