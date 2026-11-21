package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"clusbot/internal/httpapi"
	"clusbot/internal/work"
	"clusbot/web"
)

func serveAccounts(db *work.Store, listen, origin string, printURL bool, configure func(context.Context, *httpapi.Server) (func(), error)) error {
	required, err := db.BootstrapRequired(context.Background())
	if err != nil {
		return err
	}
	assets, err := fs.Sub(web.Assets, web.AssetDirectory)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	parsed, err := url.Parse(origin)
	if err != nil {
		return err
	}
	api := httpapi.Server{Store: db, ServerMode: true, Origin: origin, Hosts: []string{parsed.Host}}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if configure != nil {
		cleanup, configureErr := configure(ctx, &api)
		if cleanup != nil {
			defer cleanup()
		}
		if configureErr != nil {
			return configureErr
		}
	}
	server := &http.Server{Handler: api.Handler(assets), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	if required {
		slog.Info("服务器已启动，请在限制访问的部署环境中设置首位超级管理员", "origin", origin)
	} else {
		slog.Info("服务器已启动", "origin", origin)
	}
	if printURL {
		fmt.Println(origin)
	}
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-done:
	}
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err = server.Shutdown(shutdown); err != nil {
		server.Close()
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}
