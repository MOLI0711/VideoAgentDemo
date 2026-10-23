// Package app assembles runtime adapters. Construction does not silently enable mock providers.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/MOLI0711/MOLIVedio/internal/agent"
	"github.com/MOLI0711/MOLIVedio/internal/config"
	"github.com/MOLI0711/MOLIVedio/internal/media"
	"github.com/MOLI0711/MOLIVedio/internal/pipeline"
	"github.com/MOLI0711/MOLIVedio/internal/provider"
	db "github.com/MOLI0711/MOLIVedio/internal/repository/mysql"
	"github.com/MOLI0711/MOLIVedio/internal/retrieval"
	"github.com/MOLI0711/MOLIVedio/internal/storage"
	"github.com/MOLI0711/MOLIVedio/internal/workflow"
	"github.com/redis/go-redis/v9"
	"os"
	"time"
)

type Runtime struct {
	Config  config.Config
	Store   *db.Store
	Objects *storage.S3
	Stage   storage.Store
	Model   provider.Model
	ASR     provider.ASR
	Search  *retrieval.Client
	Redis   *redis.Client
}

func Open(ctx context.Context, c config.Config) (*Runtime, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	dsn, e := c.DSN()
	if e != nil {
		return nil, e
	}
	s, e := db.Open(ctx, dsn)
	if e != nil {
		return nil, fmt.Errorf("MySQL connection failed (check %s)", c.MySQL.DSNEnv)
	}
	s.DB.SetMaxOpenConns(c.MySQL.MaxOpen)
	s.DB.SetMaxIdleConns(5)
	s.DB.SetConnMaxLifetime(5 * time.Minute)
	r := &Runtime{Config: c, Store: s}
	ok := false
	defer func() {
		if !ok {
			r.Close()
		}
	}()
	r.Objects, e = storage.NewS3(storage.S3Config{Endpoint: c.Storage.InternalEndpoint, PublicEndpoint: c.Storage.BrowserEndpoint, Region: c.Storage.Region, Bucket: c.Storage.Bucket, AccessKey: os.Getenv(c.Storage.AccessKeyEnv), SecretKey: os.Getenv(c.Storage.SecretKeyEnv), PathStyle: c.Storage.PathStyle, MaxObjectBytes: c.Limits.UploadMaxBytes})
	if e != nil {
		return nil, e
	}
	var fixture struct {
		Model provider.MockModelScript `json:"model"`
		ASR   provider.MockASRScript   `json:"asr"`
	}
	if c.Model.Mode == "mock" || c.ASR.Mode == "mock" {
		b, e := os.ReadFile(c.MockFixturePath)
		if e != nil {
			return nil, fmt.Errorf("read explicit mock fixture: %w", e)
		}
		if e = json.Unmarshal(b, &fixture); e != nil {
			return nil, fmt.Errorf("invalid mock fixture")
		}
	}
	switch c.Model.Mode {
	case "mock":
		r.Model, e = provider.NewMockModel(fixture.Model)
	case "live":
		r.Model, e = provider.NewOpenAICompatible(provider.ModelConfig{BaseURL: c.Model.Endpoint, APIKey: os.Getenv(c.Model.APIKeyEnv), ChatModel: c.Model.ChatModel, VisionModel: c.Model.VisionModel, EmbeddingModel: c.Model.EmbeddingModel, EmbeddingDimensions: c.Search.Dimensions})
	}
	if e != nil {
		return nil, e
	}
	switch c.ASR.Mode {
	case "mock":
		r.ASR, e = provider.NewMockASR(fixture.ASR)
	case "live":
		r.ASR, e = provider.NewTencentASR(provider.TencentASRConfig{SecretID: os.Getenv(c.ASR.SecretIDEnv), SecretKey: os.Getenv(c.ASR.SecretKeyEnv), Region: c.ASR.Region, EngineModel: c.ASR.Engine})
	}
	if e != nil {
		return nil, e
	}
	if c.ASR.Mode == "live" {
		r.Stage, e = storage.NewCOS(storage.S3Config{Endpoint: c.ASRStage.Endpoint, Region: c.ASRStage.Region, Bucket: c.ASRStage.Bucket, AccessKey: os.Getenv(c.ASRStage.AccessKeyEnv), SecretKey: os.Getenv(c.ASRStage.SecretKeyEnv), MaxObjectBytes: c.Limits.UploadMaxBytes})
		if e != nil {
			return nil, e
		}
	}
	if c.Search.Enabled {
		if r.Model == nil {
			return nil, fmt.Errorf("search requires an embedding model")
		}
		r.Search, e = retrieval.New(c.Search.Endpoint, os.Getenv(c.Search.APIKeyEnv), c.Search.Dimensions, r.Model)
		if e != nil {
			return nil, e
		}
	}
	if c.Redis.Enabled {
		r.Redis = redis.NewClient(&redis.Options{Addr: c.Redis.Addr, Password: os.Getenv(c.Redis.PasswordEnv), DialTimeout: 3 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second})
		if e = r.Redis.Ping(ctx).Err(); e != nil {
			return nil, fmt.Errorf("Redis is enabled but unavailable")
		}
	}
	ok = true
	return r, nil
}
func (r *Runtime) Close() {
	if r.Redis != nil {
		r.Redis.Close()
	}
	if r.Store != nil {
		r.Store.Close()
	}
}
func (r *Runtime) Worker(kind string) (*workflow.Engine, error) {
	if kind != "media" && kind != "agent" {
		return nil, fmt.Errorf("worker kind must be media or agent")
	}
	c := r.Config
	mediaEngine, e := media.New(media.Options{FFmpegPath: c.Media.FFmpeg, FFprobePath: c.Media.FFprobe, TesseractPath: c.Media.Tesseract, OCRLanguage: c.Media.Languages, MaxInputBytes: c.Limits.UploadMaxBytes, MaxDuration: time.Duration(c.Limits.VideoMaxDurationMS) * time.Millisecond})
	if e != nil {
		return nil, e
	}
	var indexer pipeline.Indexer
	if r.Search != nil {
		indexer = r.Search
	}
	p, e := pipeline.New(pipeline.Deps{Repository: r.Store, Objects: r.Objects, ASRStore: r.Stage, Media: mediaEngine, ASR: r.ASR, Indexer: indexer, ASRMode: c.ASR.Mode, ASRProviderRef: "tencent-asr:" + c.ASR.Engine, TempDir: c.Media.TempDir, IndexName: c.Search.IndexName, EmbeddingProfile: c.Search.EmbeddingProfile, MaxInputBytes: c.Limits.UploadMaxBytes, MaxDuration: time.Duration(c.Limits.VideoMaxDurationMS) * time.Millisecond, PollInterval: 10 * time.Second})
	if e != nil {
		return nil, e
	}
	engine := workflow.New(r.Store, workflow.Options{WorkerKind: kind, Concurrency: c.Workflow.WorkerConcurrency, LeaseDuration: c.Lease(), HeartbeatInterval: time.Duration(c.Workflow.HeartbeatSeconds) * time.Second, MaxRetries: c.Workflow.MaxFailureRetries, DispatchMode: c.Workflow.DispatchMode, RecoverExpired: true})
	p.Register(engine)
	if kind == "agent" {
		var search agent.Searcher
		if r.Search != nil {
			search = r.Search
		}
		aw := &agent.Worker{ProviderRef: c.Model.Mode, ChatModelID: c.Model.ChatModel, Store: r.Store, Objects: r.Objects, Model: r.Model, Search: search, MaxFrames: c.Agent.MaxFramesPerCall, Timeout: time.Duration(c.Agent.ExecutionTimeoutSeconds) * time.Second}
		aw.Register(engine)
	}
	return engine, nil
}

type RateLimiter struct {
	Client *redis.Client
	Limit  int
}

func (l *RateLimiter) Allow(ctx context.Context, key string) (bool, error) {
	hash := sha256.Sum256([]byte(key))
	key = "va:rate:" + hex.EncodeToString(hash[:])
	limit := l.Limit
	if limit <= 0 {
		limit = 60
	}
	script := redis.NewScript(`local n=redis.call('INCR',KEYS[1]); if n==1 then redis.call('EXPIRE',KEYS[1],60) end; return n`)
	n, e := script.Run(ctx, l.Client, []string{key}).Int()
	return n <= limit, e
}
