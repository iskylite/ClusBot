package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"clusbot/internal/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func addLookupFixture(server *mcp.Server, description string) {
	server.AddTool(&mcp.Tool{
		Name: "lookup_status", Title: "查询服务状态", Description: description,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"mode":{"type":"string"}},"required":["mode"],"additionalProperties":false}`),
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct{ Mode string }
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		text := "样例服务状态：可用"
		var structured map[string]any
		switch args.Mode {
		case "large":
			credential := os.Getenv("CLUSBOT_MCP_FIXTURE_TOKEN")
			const checks = "06:40:02 INFO  api-gateway 就绪探测通过\n06:40:03 INFO  queue-worker 当前队列长度 12\n"
			text = strings.TrimSpace(credential+"\n"+strings.Repeat(checks, 700)) + "\n最后一行"
			structured = map[string]any{"tail": "最后一行", "credential": credential}
		case "overlimit":
			text = strings.Repeat("测", 400000)
		case "error":
			text = "样例服务未找到"
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, StructuredContent: structured, IsError: args.Mode == "error"}, nil
	})
}

func lookupServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "ClusBot 测试资料", Version: "1"}, nil)
	addLookupFixture(server, "查询固定样例中的服务状态。")
	return server
}

func TestMCPFixture(t *testing.T) {
	switch os.Getenv("CLUSBOT_MCP_FIXTURE") {
	case "stdio":
		if os.Getenv("CLUSBOT_MCP_UNRELATED") != "" {
			os.Exit(2)
		}
		if err := lookupServer().Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case "http":
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		server := lookupServer()
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
		fmt.Println("fixture_url=http://" + listener.Addr().String() + "/mcp")
		if err = http.Serve(listener, handler); err != nil {
			t.Fatal(err)
		}
	default:
		t.Skip("used by integration subprocesses")
	}
}

func TestConnectionsDiscoverAndAuthorizeBothTransports(t *testing.T) {
	for _, transport := range []string{"http", "stdio"} {
		t.Run(transport, func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(filepath.Join(t.TempDir(), "connections.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			t.Setenv("CLUSBOT_MCP_FIXTURE_TOKEN", "test-token")
			cfg := mcpclient.Config{Transport: transport}
			var server *mcp.Server
			if transport == "http" {
				server = lookupServer()
				handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
				httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer test-token" {
						http.Error(w, "missing test credential", 401)
						return
					}
					handler.ServeHTTP(w, r)
				}))
				defer httpServer.Close()
				cfg.Endpoint = httpServer.URL + "/mcp"
				cfg.TokenEnv = "CLUSBOT_MCP_FIXTURE_TOKEN"
			} else {
				t.Setenv("CLUSBOT_MCP_FIXTURE", "stdio")
				t.Setenv("CLUSBOT_MCP_UNRELATED", "never inherited")
				cfg.Command, err = os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				cfg.Args = []string{"-test.run=^TestMCPFixture$"}
				cfg.Env = []string{"CLUSBOT_MCP_FIXTURE", "CLUSBOT_MCP_FIXTURE_TOKEN"}
			}
			c, err := s.SaveConnection(ctx, "", Connection{Name: "样例", Enabled: true, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			c, err = s.TestConnection(ctx, c.ID, c.Revision)
			if err != nil || c.Status != "ready" || c.Protocol == "" || len(c.Tools) != 1 || c.Tools[0].Allowed {
				t.Fatalf("discovery must default to denied: %+v, %v", c, err)
			}
			args := map[string]any{"mode": "normal"}
			if _, err = s.CallConnectionTool(ctx, c.ID, c.Tools[0].Name, c.Revision, args); !errors.Is(err, ErrToolPermission) {
				t.Fatalf("unapproved tool ran: %v", err)
			}
			if err = s.SetToolPermission(ctx, c.ID, c.Tools[0].Name, c.Tools[0].Version, c.Revision, true); err != nil {
				t.Fatal(err)
			}
			c, err = s.Connection(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.CallConnectionTool(ctx, c.ID, c.Tools[0].Name, c.Revision, map[string]any{}); !errors.Is(err, mcpclient.ErrArguments) {
				t.Fatalf("schema did not reject invalid arguments: %v", err)
			}
			result, err := s.CallConnectionTool(ctx, c.ID, c.Tools[0].Name, c.Revision, args)
			if err != nil || result.Text != "样例服务状态：可用" || result.IsError || result.ID == "" {
				t.Fatalf("call: %+v, %v", result, err)
			}
			if _, err = s.ConnectionEvidence(ctx, result.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("small result unexpectedly created a download: %v", err)
			}
			result, err = s.CallConnectionTool(ctx, c.ID, c.Tools[0].Name, c.Revision, map[string]any{"mode": "large"})
			if err != nil || !result.Truncated || result.Reference == nil || len(result.Text) > mcpclient.MaxResult ||
				!utf8.ValidString(result.Text) || strings.Contains(result.Text, "test-token") {
				t.Fatalf("result bound: %d bytes, truncated=%v, %v", len(result.Text), result.Truncated, err)
			}
			evidence, err := s.ConnectionEvidence(ctx, result.Reference.ID)
			if err != nil || result.Reference.ID != result.ID || len(evidence) != result.Reference.Bytes ||
				digest(string(evidence)) != result.Reference.SHA256 || strings.Contains(string(evidence), "test-token") {
				t.Fatalf("evidence receipt does not match the redacted download: %+v, %v", result.Reference, err)
			}
			var full mcpclient.Result
			if err = json.Unmarshal(evidence, &full); err != nil || full.Truncated || !strings.HasSuffix(full.Text, "最后一行") ||
				!strings.Contains(string(full.Structured), `"tail":"最后一行"`) {
				t.Fatalf("saved evidence does not include the complete result: %v", err)
			}
			oldID := result.Reference.ID
			defer func() {
				current, err := s.Connection(ctx, c.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.Tools[0].Allowed {
					if err = s.SetToolPermission(ctx, current.ID, current.Tools[0].Name, current.Tools[0].Version, current.Revision, false); err != nil {
						t.Fatal(err)
					}
				}
				if _, err = s.ConnectionEvidence(ctx, oldID); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("old private evidence available after grant or version changed: %v", err)
				}
				var retained []byte
				if err = s.db.QueryRowContext(ctx, "SELECT evidence FROM connection_calls WHERE id=?", oldID).Scan(&retained); err != nil || string(retained) != string(evidence) {
					t.Fatalf("original stored evidence changed: %v", err)
				}
			}()
			if transport == "stdio" {
				result, err = s.CallConnectionTool(ctx, c.ID, c.Tools[0].Name, c.Revision, map[string]any{"mode": "overlimit"})
				if err != nil || !result.Truncated || result.Reference != nil {
					t.Fatalf("response over the archive bound unexpectedly has a reference: %v", err)
				}
				return
			}
			result, err = s.CallConnectionTool(ctx, c.ID, c.Tools[0].Name, c.Revision, map[string]any{"mode": "error"})
			if err != nil || !result.IsError {
				t.Fatalf("tool error was not preserved: %+v, %v", result, err)
			}
			t.Setenv("CLUSBOT_MCP_FIXTURE_TOKEN", "rotated-token")
			if _, err = s.CallConnectionTool(ctx, c.ID, c.Tools[0].Name, c.Revision, args); !errors.Is(err, mcpclient.ErrSchemaChanged) {
				t.Fatalf("changed credential used an old grant: %v", err)
			}
			t.Setenv("CLUSBOT_MCP_FIXTURE_TOKEN", "test-token")
			addLookupFixture(server, "新版本的样例工具")
			if _, err = s.CallConnectionTool(ctx, c.ID, c.Tools[0].Name, c.Revision, args); !errors.Is(err, mcpclient.ErrSchemaChanged) {
				t.Fatalf("changed definition used an old grant: %v", err)
			}
			c, err = s.TestConnection(ctx, c.ID, c.Revision)
			if err != nil || c.Status != "ready" || len(c.Tools) != 1 || c.Tools[0].Allowed {
				t.Fatalf("changed definition inherited grant: %+v, %v", c, err)
			}
		})
	}
}

func TestConnectionInlineResultSettledAfterRevocation(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "inline-revocation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server := mcp.NewServer(&mcp.Implementation{Name: "local-test", Version: "1"}, nil)
	type revokeSource struct {
		id, name, version string
		revision          int
	}
	input := make(chan revokeSource, 1)
	server.AddTool(&mcp.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			source := <-input
			if err := s.SetToolPermission(ctx, source.id, source.name, source.version, source.revision, false); err != nil {
				return nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "private-fixture"}}}, nil
		})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer httpServer.Close()
	c, err := s.SaveConnection(ctx, "", Connection{Name: "fixture", Enabled: true, Config: mcpclient.Config{Transport: "http", Endpoint: httpServer.URL}})
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.TestConnection(ctx, c.ID, c.Revision)
	if err != nil || len(c.Tools) != 1 {
		t.Fatalf("catalog %v %v", c.Tools, err)
	}
	tool := c.Tools[0]
	if err = s.SetToolPermission(ctx, c.ID, tool.Name, tool.Version, c.Revision, true); err != nil {
		t.Fatal(err)
	}
	c, err = s.Connection(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	input <- revokeSource{c.ID, tool.Name, tool.Version, c.Revision}
	result, err := s.CallConnectionTool(ctx, c.ID, tool.Name, c.Revision, map[string]any{})
	if err != nil || !result.SourceUnavailable || result.ID == "" || result.ReceivedAt == 0 || result.Text != "" || result.Reference != nil {
		t.Fatalf("expected redacted settlement, got %+v %v", result, err)
	}
	var status string
	if err = s.db.QueryRowContext(ctx, "SELECT status FROM connection_calls WHERE id=?", result.ID).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("original dispatched outcome changed: %s %v", status, err)
	}
}
