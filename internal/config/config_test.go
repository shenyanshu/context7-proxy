package config

import (
	"testing"
	"time"
)

// setenv 批量设置环境变量并注册清理：测试间互不泄漏。
func setenv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadDefaults(t *testing.T) {
	setenv(t, map[string]string{
		"MASTER_KEY": "secret",
		"PORT":       "", "DB_PATH": "", "CACHE_TTL": "", "UPSTREAM": "",
		"MCP_UPSTREAM": "",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 8080 || cfg.DBPath != "./data/proxy.db" ||
		cfg.CacheTTL != 24*time.Hour || cfg.Upstream != "https://context7.com" ||
		cfg.MCPUpstream != "https://mcp.context7.com" {
		t.Fatalf("cfg = %+v, want all defaults", cfg)
	}
}

func TestLoadOverrides(t *testing.T) {
	setenv(t, map[string]string{
		"MASTER_KEY": "k", "PORT": "9000", "DB_PATH": "/tmp/x.db",
		"CACHE_TTL": "1h30m", "UPSTREAM": "http://up.example",
		"MCP_UPSTREAM": "http://mcp.example",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 9000 || cfg.DBPath != "/tmp/x.db" ||
		cfg.CacheTTL != 90*time.Minute || cfg.Upstream != "http://up.example" ||
		cfg.MCPUpstream != "http://mcp.example" {
		t.Fatalf("cfg = %+v, want overridden values", cfg)
	}
}

// MASTER_KEY 为空是配置错误：必须 fail fast 返回错误而非空凭证启动。
func TestLoadMissingMasterKey(t *testing.T) {
	setenv(t, map[string]string{"MASTER_KEY": ""})
	if _, err := Load(); err == nil {
		t.Fatal("empty MASTER_KEY must be a config error")
	}
}

func TestLoadBadPortAndTTL(t *testing.T) {
	setenv(t, map[string]string{"MASTER_KEY": "k", "PORT": "not-a-port"})
	if _, err := Load(); err == nil {
		t.Fatal("bad PORT must fail with explicit error")
	}
	t.Setenv("PORT", "0")
	if _, err := Load(); err == nil {
		t.Fatal("PORT=0 out of range must fail")
	}
	t.Setenv("PORT", "")
	t.Setenv("CACHE_TTL", "not-a-duration")
	if _, err := Load(); err == nil {
		t.Fatal("bad CACHE_TTL must fail with explicit error")
	}
	t.Setenv("CACHE_TTL", "-1h")
	if _, err := Load(); err == nil {
		t.Fatal("negative CACHE_TTL must fail")
	}
}
