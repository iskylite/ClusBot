package work

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestV31BenchmarkHTTPBoundTransport(t *testing.T) {
	ctx := context.Background()
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer bench-token" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/must-not-call", http.StatusTemporaryRedirect)
		case "/lookup":
			_, _ = w.Write([]byte(`{"job_id":"original"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p, err := NewHTTPBenchmarkProvider(server.URL, "test", 1, []string{"gpu"}, "bench-token")
	if err != nil {
		t.Fatal(err)
	}
	req := BenchmarkJobRequest{Binding: BenchmarkBinding{"test", 1, "gpu", benchmarkValidatorV1}, IdempotencyKey: "same-key"}
	if _, err = p.Start(ctx, req); err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("redirect should be refused: %v", err)
	}
	if job, err := p.Lookup(ctx, req); err != nil || job != "original" || calls != 2 {
		t.Fatalf("lookup: %q %d %v", job, calls, err)
	}
	s, err := Open(filepath.Join(t.TempDir(), "connection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.InstallBenchmarkProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	q, err := NewHTTPBenchmarkProvider(server.URL+"/other", "test", 1, []string{"gpu"}, "bench-token")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.InstallBenchmarkProvider(ctx, q); err == nil {
		t.Fatal("changing endpoint silently changed frozen binding")
	}
}
