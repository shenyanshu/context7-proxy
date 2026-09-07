// Package store 提供基于 SQLite（WAL）的持久化：上游 key 池、响应缓存、请求日志与统计。
// 全部 SQL 参数化；key 原文只在本包与调用方内部流转，对外 JSON 模型一律脱敏。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动（无 CGO），注册驱动名 "sqlite"
)

// 预定义错误：调用方用 errors.Is 判断业务结果，不解析错误字符串。
var (
	ErrDuplicateKey     = errors.New("upstream key already exists")
	ErrKeyNotFound      = errors.New("upstream key not found")
	ErrInvalidKeyFormat = errors.New("upstream key must start with ctx7sk- and be at least 13 characters")
)

const (
	// 单实例部署用单连接串行访问即可；busy_timeout 兜底与外部进程
	//（如备份工具）短暂持锁的冲突，避免直接报 SQLITE_BUSY。
	busyTimeoutMs = 5000
	maxOpenConns  = 1
)

// 数据库文件包含上游 key 原文，目录与文件必须按私密数据处理。
const (
	dirPermPrivate  os.FileMode = 0o700
	filePermPrivate os.FileMode = 0o600
)

var schema = `
CREATE TABLE IF NOT EXISTS keys (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	value          TEXT NOT NULL UNIQUE,
	enabled        INTEGER NOT NULL DEFAULT 1,
	remaining      INTEGER,
	rate_limit     INTEGER,
	cooldown_until INTEGER,
	request_count  INTEGER NOT NULL DEFAULT 0,
	last_used_at   INTEGER,
	added_at       INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS cache (
	hash         TEXT PRIMARY KEY,
	body         BLOB NOT NULL,
	content_type TEXT NOT NULL,
	expires_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS request_log (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	time        INTEGER NOT NULL,
	method      TEXT NOT NULL,
	path        TEXT NOT NULL,
	status      INTEGER NOT NULL,
	key_id      INTEGER NOT NULL DEFAULT 0,
	key_masked  TEXT NOT NULL DEFAULT '',
	cached      INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS stats_counters (
	id             INTEGER PRIMARY KEY CHECK (id = 1),
	total_requests INTEGER NOT NULL DEFAULT 0,
	cache_hits     INTEGER NOT NULL DEFAULT 0
);

-- 按日计数单独建表而非从 500 条日志推算：日志会环形裁剪，当天总量必须独立累计
CREATE TABLE IF NOT EXISTS stats_daily (
	day      TEXT PRIMARY KEY,
	requests INTEGER NOT NULL DEFAULT 0
);

-- 累计计数固定单行：初始化即插入，GetStats 永远能读到该行，新库零值合法
INSERT OR IGNORE INTO stats_counters (id) VALUES (1);
`

// Store 封装 SQLite 连接；database/sql 在单连接上限下自动排队，多 goroutine 使用安全。
type Store struct {
	db *sql.DB
}

// Open 打开（或创建）数据库文件并完成迁移。父目录不存在时按 0700 创建，
// 已存在目录的权限不做任何改动。数据库文件包含上游 key 原文，必须 0600：
// SQLite 以默认 umask 创建文件（常见 0644），存在凭据落盘后被其他用户
// 读取的窗口，因此先由本函数预创建（O_CREATE，已存在则跳过并显式 chmod
// 消除 umask 干扰）再交给 SQLite；-wal/-shm 附属文件在首次写入后才出现，
// 由末尾统一收紧。
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), dirPermPrivate); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}
	if err := ensurePrivateFile(path); err != nil {
		return nil, err
	}
	// 用 DSN 传 PRAGMA 而非 Exec：连接意外重建时语义依然生效，不依赖连接复用假设
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)", path, busyTimeoutMs)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	// WAL 附属文件在前面的写入后才出现，所以权限收紧放在迁移之后
	if err := chmodPrivate(path); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// ensurePrivateFile 确保 DB 文件存在且恰为 0600：
// 不存在则以 0600 创建（O_EXCL 不必要——存在分支单独处理）；
// 已存在则显式 chmod 回 0600，覆盖 umask 与历史遗留的宽权限。
func ensurePrivateFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, filePermPrivate)
	if err != nil {
		return fmt.Errorf("pre-create db file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close pre-created db file: %w", err)
	}
	// 显式 chmod：OpenFile 的 perm 参数受 umask 缩减，可能得到比 0600 更严的
	// 权限；统一固定为 0600，断言口径与实际状态一致。
	if err := os.Chmod(path, filePermPrivate); err != nil {
		return fmt.Errorf("chmod db file: %w", err)
	}
	return nil
}

// Close 关闭数据库连接，WAL 内容由 SQLite 正常落盘。
func (s *Store) Close() error {
	return s.db.Close()
}

// chmodPrivate 收紧数据库本体与 -wal/-shm 附属文件权限；附属文件不存在属正常，忽略即可。
func chmodPrivate(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, filePermPrivate); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("chmod %s: %w", p, err)
		}
	}
	return nil
}
