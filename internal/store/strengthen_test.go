// 项 6/7 测试：DB 文件权限预创建、缓存过期判定口径一致性。
package store

import (
	"os"
	"testing"
	"time"
)

// 项 6：新建 DB 文件 mode 恰为 0600（显式 chmod 消除 umask 干扰后断言）。
func TestOpenPreCreatesPrivateDBFile(t *testing.T) {
	path := t.TempDir() + "/perm.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	// Open 内部已显式 chmod 0600；此处断言实际 mode 排除 umask 缩减干扰
	if fi.Mode().Perm() != filePermPrivate {
		t.Fatalf("db file mode = %o, want %o", fi.Mode().Perm(), filePermPrivate)
	}
}

// 项 6：已存在的宽权限文件被 Open 收回 0600（历史遗留文件的修复路径）。
func TestOpenTightensExistingFilePerm(t *testing.T) {
	path := t.TempDir() + "/loose.db"
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	f.Close()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != filePermPrivate {
		t.Fatalf("db file mode after Open = %o, want %o", fi.Mode().Perm(), filePermPrivate)
	}
}

// 项 7：过期判定口径一致性——SetCache 写入即刻到 GetCache 判定，
// 同一秒内写入的条目不会被判定过期；负 TTL 条目判定 miss 且被删除。
// 三处（Set/Get/Clean）统一使用各自操作时刻的 Unix 秒，秒级粒度下
// “恰好压线”的条目行为确定：expires_at <= now 即过期。
func TestCacheExpiryNowConsistency(t *testing.T) {
	s := newTestStore(t)

	// 零 TTL：expires_at == 写入时刻秒，同秒内读取必然 expires_at <= now，判 miss
	if err := s.SetCache("k0", []byte("b"), "application/json", 0); err != nil {
		t.Fatalf("SetCache: %v", err)
	}
	if _, err := s.GetCache("k0"); err != ErrCacheMiss {
		t.Fatalf("zero-TTL entry err = %v, want ErrCacheMiss (expires_at <= now 口径)", err)
	}

	// 正 TTL：写入后立刻读取必然命中（口径同为当前秒，now 只增不减）
	if err := s.SetCache("k1", []byte("b"), "application/json", time.Minute); err != nil {
		t.Fatalf("SetCache: %v", err)
	}
	if _, err := s.GetCache("k1"); err != nil {
		t.Fatalf("fresh entry err = %v, want hit", err)
	}

	// 过期条目：GetCache 判 miss 且按同一 now 条件删除，重读无残留
	if err := s.SetCache("k2", []byte("b"), "application/json", -time.Minute); err != nil {
		t.Fatalf("SetCache: %v", err)
	}
	if _, err := s.GetCache("k2"); err != ErrCacheMiss {
		t.Fatalf("expired entry err = %v, want ErrCacheMiss", err)
	}
	if _, err := s.GetCache("k2"); err != ErrCacheMiss {
		t.Fatalf("expired entry must be deleted by first GetCache, got err %v", err)
	}
	// CleanExpiredCache 与 GetCache 同口径：未过期条目存活
	if err := s.CleanExpiredCache(); err != nil {
		t.Fatalf("CleanExpiredCache: %v", err)
	}
	if _, err := s.GetCache("k1"); err != nil {
		t.Fatalf("fresh entry must survive CleanExpiredCache, got %v", err)
	}
}
