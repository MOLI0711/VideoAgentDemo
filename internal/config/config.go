package config

import (
	"fmt"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	MockFixturePath string `yaml:"mock_fixture_path"`
	ASRStage        struct {
		Endpoint     string `yaml:"endpoint"`
		Bucket       string `yaml:"bucket"`
		Region       string `yaml:"region"`
		AccessKeyEnv string `yaml:"access_key_env"`
		SecretKeyEnv string `yaml:"secret_key_env"`
	} `yaml:"asr_stage"`
	App struct {
		Env          string `yaml:"env"`
		HTTPAddr     string `yaml:"http_addr"`
		PublicOrigin string `yaml:"public_origin"`
		CookieSecure bool   `yaml:"cookie_secure"`
	} `yaml:"app"`
	MySQL struct {
		DSNEnv  string `yaml:"dsn_env"`
		MaxOpen int    `yaml:"max_open"`
	} `yaml:"mysql"`
	Redis struct {
		Addr        string `yaml:"addr"`
		PasswordEnv string `yaml:"password_env"`
		Enabled     bool   `yaml:"enabled"`
	} `yaml:"redis"`
	Kafka struct {
		Brokers    []string `yaml:"brokers"`
		MediaTopic string   `yaml:"media_topic"`
		AgentTopic string   `yaml:"agent_topic"`
	} `yaml:"kafka"`
	Storage struct {
		InternalEndpoint string `yaml:"internal_endpoint"`
		BrowserEndpoint  string `yaml:"browser_endpoint"`
		Bucket           string `yaml:"bucket"`
		Region           string `yaml:"region"`
		AccessKeyEnv     string `yaml:"access_key_env"`
		SecretKeyEnv     string `yaml:"secret_key_env"`
		PathStyle        bool   `yaml:"path_style"`
	} `yaml:"storage"`
	Search struct {
		Endpoint         string `yaml:"endpoint"`
		APIKeyEnv        string `yaml:"api_key_env"`
		IndexName        string `yaml:"index_name"`
		EmbeddingProfile string `yaml:"embedding_profile"`
		Dimensions       int    `yaml:"dimensions"`
		Enabled          bool   `yaml:"enabled"`
	} `yaml:"search"`
	Model Model `yaml:"model"`
	ASR   struct {
		Mode         string `yaml:"mode"`
		SecretIDEnv  string `yaml:"secret_id_env"`
		SecretKeyEnv string `yaml:"secret_key_env"`
		Region       string `yaml:"region"`
		Engine       string `yaml:"engine"`
	} `yaml:"asr"`
	Media struct {
		FFmpeg    string `yaml:"ffmpeg"`
		FFprobe   string `yaml:"ffprobe"`
		Tesseract string `yaml:"tesseract"`
		Languages string `yaml:"languages"`
		TempDir   string `yaml:"temp_dir"`
	} `yaml:"media"`
	Workflow struct {
		DispatchMode      string `yaml:"dispatch_mode"`
		LeaseSeconds      int    `yaml:"lease_seconds"`
		HeartbeatSeconds  int    `yaml:"heartbeat_seconds"`
		MaxFailureRetries int    `yaml:"max_failure_retries"`
		WorkerConcurrency int    `yaml:"worker_concurrency"`
	} `yaml:"workflow"`
	Limits struct {
		UploadMaxBytes     int64 `yaml:"upload_max_bytes"`
		VideoMaxDurationMS int64 `yaml:"video_max_duration_ms"`
		UploadPartBytes    int64 `yaml:"upload_part_bytes"`
		UploadParallelism  int   `yaml:"upload_parallelism"`
	} `yaml:"limits"`
	Agent struct {
		MaxModelIterations      int `yaml:"max_model_iterations"`
		MaxToolCalls            int `yaml:"max_tool_calls"`
		MaxVisionCalls          int `yaml:"max_vision_calls"`
		MaxFramesPerCall        int `yaml:"max_frames_per_call"`
		ExecutionTimeoutSeconds int `yaml:"execution_timeout_seconds"`
	} `yaml:"agent"`
}

type Model struct {
	Mode           string `yaml:"mode"`
	Endpoint       string `yaml:"endpoint"`
	APIKeyEnv      string `yaml:"api_key_env"`
	ChatModel      string `yaml:"chat_model"`
	VisionModel    string `yaml:"vision_model"`
	EmbeddingModel string `yaml:"embedding_model"`
}

func Default() Config {
	var c Config
	c.App.Env, c.App.HTTPAddr, c.App.PublicOrigin = "local", "127.0.0.1:8080", "http://localhost:5173"
	c.MySQL.DSNEnv, c.MySQL.MaxOpen = "VA_MYSQL_DSN", 20
	c.Redis.Addr, c.Redis.PasswordEnv = "127.0.0.1:16379", "VA_REDIS_PASSWORD"
	c.Kafka.Brokers, c.Kafka.MediaTopic, c.Kafka.AgentTopic = []string{"127.0.0.1:19092"}, "video.media.dispatch", "video.agent.dispatch"
	c.Storage.InternalEndpoint, c.Storage.BrowserEndpoint = "http://127.0.0.1:18333", "http://127.0.0.1:18333"
	c.Storage.Bucket, c.Storage.Region, c.Storage.PathStyle = "video-agent-dev", "us-east-1", true
	c.Storage.AccessKeyEnv, c.Storage.SecretKeyEnv = "VA_S3_ACCESS_KEY", "VA_S3_SECRET_KEY"
	c.Search.Endpoint, c.Search.IndexName, c.Search.EmbeddingProfile = "http://127.0.0.1:19200", "video-segments-embed01-map01", "embed01"
	c.Model.Mode, c.ASR.Mode = "disabled", "disabled"
	c.Model.APIKeyEnv, c.ASR.SecretIDEnv, c.ASR.SecretKeyEnv = "VA_MODEL_API_KEY", "VA_ASR_SECRET_ID", "VA_ASR_SECRET_KEY"
	c.ASR.Region, c.ASR.Engine = "ap-shanghai", "16k_zh"
	c.ASRStage.AccessKeyEnv, c.ASRStage.SecretKeyEnv = "VA_COS_SECRET_ID", "VA_COS_SECRET_KEY"
	c.Media.FFmpeg, c.Media.FFprobe, c.Media.Tesseract = "ffmpeg", "ffprobe", "tesseract"
	c.Media.Languages, c.Media.TempDir = "chi_sim+eng", ".runtime/media"
	c.Workflow.DispatchMode, c.Workflow.LeaseSeconds, c.Workflow.HeartbeatSeconds = "db_dev", 120, 30
	c.Workflow.MaxFailureRetries, c.Workflow.WorkerConcurrency = 3, 2
	c.Limits.UploadMaxBytes, c.Limits.VideoMaxDurationMS, c.Limits.UploadPartBytes, c.Limits.UploadParallelism = 2<<30, 7200000, 8<<20, 3
	c.Agent.MaxModelIterations, c.Agent.MaxToolCalls, c.Agent.MaxVisionCalls, c.Agent.MaxFramesPerCall, c.Agent.ExecutionTimeoutSeconds = 6, 8, 2, 4, 180
	return c
}

func Load(path string) (Config, error) {
	c := Default()
	f, err := os.Open(path)
	if err != nil {
		return c, fmt.Errorf("open configuration: %w", err)
	}
	defer f.Close()
	d := yaml.NewDecoder(f)
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("decode configuration: %w", err)
	}
	if v := os.Getenv("VA_HTTP_ADDR"); v != "" {
		c.App.HTTPAddr = v
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if c.MySQL.DSNEnv == "" {
		return fmt.Errorf("mysql.dsn_env is required")
	}
	if c.Workflow.LeaseSeconds < 3 || c.Workflow.HeartbeatSeconds < 1 || c.Workflow.HeartbeatSeconds*2 >= c.Workflow.LeaseSeconds {
		return fmt.Errorf("heartbeat must be positive and less than half the lease")
	}
	if c.Workflow.WorkerConcurrency < 1 || c.Workflow.WorkerConcurrency > 32 {
		return fmt.Errorf("worker_concurrency must be in [1,32]")
	}
	if c.Workflow.DispatchMode != "db_dev" && c.Workflow.DispatchMode != "kafka" {
		return fmt.Errorf("invalid dispatch_mode")
	}
	if c.Limits.UploadPartBytes < 5<<20 || c.Limits.UploadMaxBytes < c.Limits.UploadPartBytes {
		return fmt.Errorf("invalid upload size limits")
	}
	if c.Agent.MaxToolCalls < 1 || c.Agent.MaxModelIterations < 1 || c.Agent.MaxVisionCalls < 0 || c.Agent.MaxFramesPerCall < 1 || c.Agent.MaxFramesPerCall > 8 {
		return fmt.Errorf("invalid agent budgets")
	}
	for _, raw := range []string{c.App.PublicOrigin, c.Storage.InternalEndpoint, c.Storage.BrowserEndpoint} {
		u, e := url.Parse(raw)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("invalid configured HTTP endpoint")
		}
	}
	if c.Model.Mode != "disabled" && c.Model.Mode != "mock" && c.Model.Mode != "live" {
		return fmt.Errorf("invalid model mode")
	}
	if c.ASR.Mode != "disabled" && c.ASR.Mode != "mock" && c.ASR.Mode != "live" {
		return fmt.Errorf("invalid ASR mode")
	}
	if c.Model.Mode == "live" && (c.Model.Endpoint == "" || c.Model.ChatModel == "" || os.Getenv(c.Model.APIKeyEnv) == "") {
		return fmt.Errorf("live model requires endpoint, chat_model and configured credential")
	}
	if c.ASR.Mode == "live" && (os.Getenv(c.ASR.SecretIDEnv) == "" || os.Getenv(c.ASR.SecretKeyEnv) == "") {
		return fmt.Errorf("live ASR requires configured credentials")
	}
	if c.ASR.Mode == "live" && (c.ASRStage.Bucket == "" || c.ASRStage.Region == "" || os.Getenv(c.ASRStage.AccessKeyEnv) == "" || os.Getenv(c.ASRStage.SecretKeyEnv) == "") {
		return fmt.Errorf("live ASR requires a private COS staging bucket and explicit credentials")
	}
	if (c.Model.Mode == "mock" || c.ASR.Mode == "mock") && c.MockFixturePath == "" {
		return fmt.Errorf("mock mode requires an explicit fixture file")
	}
	if c.Search.Enabled && c.Search.Dimensions < 1 {
		return fmt.Errorf("enabled search requires verified dimensions")
	}
	if c.App.Env == "production" && (!c.App.CookieSecure || !c.Redis.Enabled || c.Model.Mode == "mock" || c.ASR.Mode == "mock" || c.Workflow.DispatchMode != "kafka") {
		return fmt.Errorf("production requires secure cookies, Redis rate limiting, Kafka and non-mock providers")
	}
	return nil
}

func (c Config) DSN() (string, error) {
	v := os.Getenv(c.MySQL.DSNEnv)
	if v == "" {
		return "", fmt.Errorf("required credential environment variable %s is unset", c.MySQL.DSNEnv)
	}
	return v, nil
}
func (c Config) Lease() time.Duration { return time.Duration(c.Workflow.LeaseSeconds) * time.Second }
