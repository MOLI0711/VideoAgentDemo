package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRejectUnknownConfigAndUnsafeBudgets(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("unknown_option: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("unknown config accepted")
	}
	c := Default()
	c.Workflow.HeartbeatSeconds = c.Workflow.LeaseSeconds
	if err := c.Validate(); err == nil {
		t.Fatal("unsafe heartbeat accepted")
	}
}
func TestNoSecretInMissingCredentialError(t *testing.T) {
	c := Default()
	c.Model.Mode = "live"
	c.Model.Endpoint = "https://example.invalid"
	c.Model.ChatModel = "test"
	t.Setenv(c.Model.APIKeyEnv, "")
	if err := c.Validate(); err == nil || strings.Contains(err.Error(), "Bearer") {
		t.Fatal("invalid credential validation", err)
	}
}

func TestProductionRequiresVisitorRateLimiting(t *testing.T) {
	c := Default()
	c.App.Env = "production"
	c.App.CookieSecure = true
	c.Workflow.DispatchMode = "kafka"
	if err := c.Validate(); err == nil {
		t.Fatal("production without visitor rate limiting accepted")
	}
	c.Redis.Enabled = true
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
