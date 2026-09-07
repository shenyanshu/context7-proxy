// 程序装配：环境变量 → store/query/server → 监听 → 信号优雅关闭。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"context7-proxy/internal/config"
	"context7-proxy/internal/query"
	"context7-proxy/internal/server"
	"context7-proxy/internal/store"
)

// shutdownTimeout 优雅关闭上限：超过即强制退出，防止个别慢客户端挂死进程。
const shutdownTimeout = 5 * time.Second

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	// 启动即清一次过期缓存：进程停摆期间积压的过期条目不等首个请求惰性删除
	if err := st.CleanExpiredCache(); err != nil {
		return fmt.Errorf("clean expired cache: %w", err)
	}

	svc := query.New(st, st, cfg.Upstream, nil, cfg.CacheTTL)
	httpSrv := server.NewHTTPServer(
		fmt.Sprintf(":%d", cfg.Port),
		server.New(cfg.MasterKey, cfg.MCPUpstream, svc, st).Handler(),
	)

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	log.Printf("context7-proxy listening on :%d (admin: /admin, upstream: %s)",
		cfg.Port, cfg.Upstream)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case sig := <-sigCh:
		log.Printf("received %s, shutting down (max %s)", sig, shutdownTimeout)
	}

	// Shutdown 期间不再接新连接、放存量请求收尾；超时强断，随后 defer st.Close 落盘
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}
