package work

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPReviewFixture supplies an isolated real protocol endpoint for the UI
// regression; it is never enabled by normal Go test runs.
func TestMCPReviewFixture(t *testing.T) {
	if os.Getenv("CLUSBOT_SCHEMA_FIXTURE") != "1" {
		t.Skip("browser fixture only")
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "合同资料", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "read_summary", Title: "读取合同摘要", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object","properties":{"amount":{"type":"number"}},"required":["amount"]}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "合同金额字段待核对"}}, StructuredContent: map[string]any{"amount": "尚未确认"}}, nil
	})
	server.AddTool(&mcp.Tool{Name: "legacy_search", Title: "查询归档资料", InputSchema: json.RawMessage(`{"type":"object","$schema":"http://json-schema.org/draft-04/schema#"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, fmt.Errorf("unsupported tool should not run")
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	fmt.Println("fixture_url=http://" + listener.Addr().String() + "/mcp")
	if err = http.Serve(listener, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)); err != nil {
		t.Fatal(err)
	}
}
