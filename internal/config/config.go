// Package config 解析环境变量为运行配置：只做环境读取与解析，
// 不引入多余抽象（配置文件/热加载等），单进程部署环境变量即全部事实来源。
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// 默认值集中维护，禁止散落在解析逻辑里。
const (
	defaultPort     = 8080
	defaultDBPath   = "./data/proxy.db"
	defaultUpstream = "https://context7.com"
	defaultCacheTTL = 24 * time.Hour
	// MCP 透明转发的官方端点：REST 文档 API 与 MCP 服务是两个不同入口，
	// 各自独立可配置（官方域名不同），不共用 UPSTREAM
	defaultMCPUpstream = "https://mcp.context7.com"
)

// Config 是装配所需的全部配置。
type Config struct {
	Port        int
	MasterKey   string
	DBPath      string
	CacheTTL    time.Duration
	Upstream    string
	MCPUpstream string
}

// Load 从环境变量构造配置。MASTER_KEY 为空是配置错误：fail fast，
// 绝不带着空鉴权凭证启动（等于把管理与查询入口裸奔到公网）。
func Load() (Config, error) {
	masterKey := os.Getenv("MASTER_KEY")
	if masterKey == "" {
		return Config{}, errors.New("MASTER_KEY is required and must not be empty")
	}

	port, err := parsePort(os.Getenv("PORT"))
	if err != nil {
		return Config{}, err
	}
	ttl, err := parseTTL(os.Getenv("CACHE_TTL"))
	if err != nil {
		return Config{}, err
	}

	dbPath := envOr("DB_PATH", defaultDBPath)
	upstream := envOr("UPSTREAM", defaultUpstream)
	mcpUpstream := envOr("MCP_UPSTREAM", defaultMCPUpstream)
	return Config{Port: port, MasterKey: masterKey, DBPath: dbPath,
		CacheTTL: ttl, Upstream: upstream, MCPUpstream: mcpUpstream}, nil
}

// parsePort 解析 PORT：空用默认值，非法（非数字/超端口范围）给明确错误。
func parsePort(v string) (int, error) {
	if v == "" {
		return defaultPort, nil
	}
	port, err := strconv.Atoi(v)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("PORT must be a valid port number (1-65535), got %q", v)
	}
	return port, nil
}

// parseTTL 解析 CACHE_TTL：空用默认值，非法格式给明确错误。
func parseTTL(v string) (time.Duration, error) {
	if v == "" {
		return defaultCacheTTL, nil
	}
	ttl, err := time.ParseDuration(v)
	if err != nil || ttl <= 0 {
		return 0, fmt.Errorf("CACHE_TTL must be a positive duration, got %q", v)
	}
	return ttl, nil
}

// envOr 读环境变量，空串视为未设置回退默认值。
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
