package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clusbot/internal/work"
)

type browserCluskitSource struct {
	executable, base, lookupKey string
	arguments, envNames         []string
	router, selectTool, answer  atomic.Int32
}

func buildBrowserCluskitSource(t *testing.T, ctx context.Context, web string) *browserCluskitSource {
	t.Helper()
	adjacent := filepath.Clean(filepath.Join(web, "..", "..", "Cluskit2-MCP"))
	if _, err := os.Stat(filepath.Join(adjacent, "go.mod")); err != nil {
		t.Fatalf("Cluskit2-MCP source checkout is required: %v", err)
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(base, "clusconf")
	bin := exec.CommandContext(ctx, "go", "build", "-mod=vendor", "-buildvcs=false", "-o", exe, "./cmd/clusconf")
	bin.Dir = adjacent
	bin.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off")
	if out, err := bin.CombinedOutput(); err != nil {
		t.Fatalf("build real clusconf for the local OS: %v\n%s", err, out)
	}
	config := filepath.Join(base, "isolated-cluskit.yaml")
	if err := os.WriteFile(config, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// The stdio executor clears the child's environment and supplies only
	// these explicit references. No live configuration, SSH agent, or cluster
	// access is needed. task_status below is a local idempotency lookup.
	t.Setenv("CLUS_ROOT", base)
	t.Setenv("CLUS_CONFIG_FILE", config)
	t.Setenv("CLUSKIT_LOG_DIR", base)
	t.Setenv("HOME", base)
	t.Setenv("XDG_STATE_HOME", base)
	t.Setenv("TMPDIR", base)
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	return &browserCluskitSource{executable: exe, base: base, lookupKey: "browser-absent-" + hex.EncodeToString(nonce[:]),
		arguments: []string{"mcp"}, envNames: []string{"CLUS_ROOT", "CLUS_CONFIG_FILE", "CLUSKIT_LOG_DIR", "HOME", "XDG_STATE_HOME", "TMPDIR"}}
}

func (s *browserCluskitSource) probeDiagnostic(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.executable, s.arguments...)
	cmd.Dir = s.base
	cmd.Env = []string{}
	for _, name := range s.envNames {
		cmd.Env = append(cmd.Env, name+"="+os.Getenv(name))
	}
	cmd.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"local-test","version":"1"}}}` + "\n")
	out, err := cmd.CombinedOutput()
	t.Logf("isolated stdio startup diagnostics: %v; %.1500s", err, out)
}

func (s *browserCluskitSource) modelDecision(t *testing.T, w http.ResponseWriter, raw []byte) {
	t.Helper()
	if bytes.Contains(raw, []byte(s.base)) || bytes.Contains(raw, []byte("CLUS_CONFIG_FILE")) || bytes.Contains(raw, []byte("CLUSKIT_LOG_DIR")) || bytes.Contains(raw, []byte("correct horse battery staple")) {
		t.Error("connection configuration or login data reached the test model")
	}
	var req struct {
		Instructions string            `json:"instructions"`
		Input        []json.RawMessage `json:"input"`
		Tools        []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Errorf("decode model request: %v", err)
		http.Error(w, "invalid test request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	stream := func(answer string) {
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", answer)
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":11,\"output_tokens\":7}}}\n\n")
	}
	switch {
	case strings.Contains(req.Instructions, "ClusBot Router"):
		s.router.Add(1)
		if !bytes.Contains(raw, []byte(s.lookupKey)) {
			t.Error("query was not sent from the current conversation")
		}
		stream(`{"kind":"new_task","goal":"只读查询本地 Cluskit2 的测试幂等键","criteria":"引用原始查询结果，不把不存在当作执行失败"}`)
	default:
		for _, item := range req.Input {
			var out struct {
				Type, Output string
			}
			if json.Unmarshal(item, &out) != nil || out.Type != "function_call_output" {
				continue
			}
			var actual work.TaskRead
			if err := json.Unmarshal([]byte(out.Output), &actual); err != nil || !s.matches(actual) {
				t.Errorf("no matching real Cluskit2 evidence in model input: %v", err)
				return
			}
			s.answer.Add(1)
			stream(s.answerText())
			return
		}
		var candidates []string
		for _, tool := range req.Tools {
			if strings.HasPrefix(tool.Name, "mcp_") {
				if !strings.Contains(tool.Description, " / task_status.") {
					t.Error("an unbound MCP tool was offered to the worker")
					return
				}
				candidates = append(candidates, tool.Name)
			}
		}
		if len(candidates) != 1 {
			t.Errorf("expected one bound Cluskit2 read tool, got %q", candidates)
			return
		}
		s.selectTool.Add(1)
		arguments, _ := json.Marshal(map[string]string{"idempotency_key": s.lookupKey})
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"function_call\",\"name\":%q,\"call_id\":\"real-stdio-status\",\"status\":\"completed\",\"arguments\":%q}],\"usage\":{\"input_tokens\":11,\"output_tokens\":7}}}\n\n", candidates[0], string(arguments))
	}
}

func (s *browserCluskitSource) matches(read work.TaskRead) bool {
	if read.ToolName != "task_status" || read.Status != "completed" || read.Result == nil || read.Result.IsError {
		return false
	}
	var evidence struct {
		Found *bool  `json:"found"`
		State string `json:"state"`
		Key   string `json:"idempotency_key"`
	}
	return json.Unmarshal(read.Result.Structured, &evidence) == nil && evidence.Found != nil && !*evidence.Found &&
		evidence.State == "NOT_FOUND" && evidence.Key == s.lookupKey
}

func (s *browserCluskitSource) answerText() string {
	return "Cluskit2 本地只读查询：未找到该幂等键（NOT_FOUND）。"
}

func (s *browserCluskitSource) verify(t *testing.T, ctx context.Context, store *work.Store, actor, conversation, run string, setupCalls int32) {
	t.Helper()
	got, err := store.RunRecord(ctx, run, "")
	if err != nil || got.TaskID == "" || got.ConversationID != conversation || got.Status != "completed" || len(got.Reads) != 1 || !s.matches(got.Reads[0]) {
		t.Fatalf("run did not preserve the real Cluskit2 read in the originating conversation: %+v %v", got.Run, err)
	}
	bindings, err := store.ReadRunMCPBindings(ctx, actor, run)
	if err != nil || bindings.AutomaticPin == nil || bindings.AutomaticPin.ToolName != "task_status" || len(bindings.Bindings) != 1 ||
		bindings.Bindings[0].Selection != "model_selected" || bindings.Bindings[0].ReadStatus != "completed" {
		t.Fatalf("run did not use the bound tool: %+v %v", bindings, err)
	}
	detail, err := store.Detail(conversation, false)
	if err != nil {
		t.Fatal(err)
	}
	persisted := false
	for _, message := range detail.Messages {
		persisted = persisted || (message.RunID == run && message.Role == "assistant" && strings.Contains(message.Text, s.answerText()))
	}
	if !persisted || setupCalls != 4 || s.router.Load() != 1 || s.selectTool.Load() != 1 || s.answer.Load() != 1 {
		t.Fatalf("unexpected evidence or replay: reply=%v setup=%d router=%d read=%d answer=%d", persisted, setupCalls, s.router.Load(), s.selectTool.Load(), s.answer.Load())
	}
	t.Log("ClusBot Web → real Cluskit2 stdio task_status (isolated local account session) → pinned read evidence and assistant message in the same conversation")
}
