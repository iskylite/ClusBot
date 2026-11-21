package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"clusbot/internal/httpapi"
	"clusbot/internal/work"
)

func TestServeAccountsConfiguresWithoutBootstrapToken(t *testing.T) {
	dir := t.TempDir()
	store, err := work.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	expected := errors.New("stop after configuration")
	configured, cleaned := false, false
	err = serveAccounts(store, "127.0.0.1:0", "https://ops.example", false, func(ctx context.Context, server *httpapi.Server) (func(), error) {
		configured = ctx != nil && server.Store == store && server.ServerMode && server.Origin == "https://ops.example"
		return func() { cleaned = true }, expected
	})
	if !errors.Is(err, expected) || !configured || !cleaned {
		t.Fatalf("configuration: err=%v configured=%v cleaned=%v", err, configured, cleaned)
	}
	if _, err = os.Stat(filepath.Join(dir, "bootstrap-token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected bootstrap token: %v", err)
	}
}
