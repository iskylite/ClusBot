package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolSchemaCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"default", `{"type":"object"}`, true},
		{"2020", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}`, true},
		{"draft7", `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"values":{"type":"array","items":[{"type":"integer"}],"additionalItems":false}}}`, true},
		{"local-reference", `{"type":"object","properties":{"x":{"$ref":"#/$defs/count"}},"$defs":{"count":{"type":"integer"}}}`, true},
		{"example-is-data", `{"type":"object","examples":[{"$schema":"custom-instance-field"}],"additionalProperties":false}`, true},
		{"unsupported-dialect", `{"$schema":"https://json-schema.org/draft/2019-09/schema","type":"object"}`, false},
		{"nested-dialect", `{"type":"object","properties":{"value":{"$schema":"https://example.invalid/schema","type":"string"}}}`, false},
		{"mixed-dialects", `{"type":"object","$defs":{"old":{"$schema":"http://json-schema.org/draft-07/schema#","type":"string"}}}`, false},
		{"invalid-pattern", `{"type":"object","properties":{"name":{"type":"string","pattern":"["}}}`, false},
		{"remote-reference", `{"type":"object","properties":{"x":{"$ref":"https://example.invalid/schema"}}}`, false},
		{"unknown-required-vocabulary", `{"$schema":"https://json-schema.org/draft/2020-12/schema","$vocabulary":{"https://example.invalid/custom":true},"type":"object"}`, false},
		{"null", `null`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateToolSchemas(Tool{InputSchema: json.RawMessage(tc.raw)})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}

func TestOutputSchemaChecksCompleteResultBeforeRedactionAndTruncation(t *testing.T) {
	const secret = "fixture-private-token"
	t.Setenv("CLUSBOT_SCHEMA_TEST_TOKEN", secret)
	server := mcp.NewServer(&mcp.Implementation{Name: "schema-fixture", Version: "1"}, nil)
	input := json.RawMessage(`{"type":"object"}`)
	output := json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"},"secret":{"const":"fixture-private-token"}},"required":["count","secret"]}`)
	for _, name := range []string{"valid", "invalid", "missing", "large-invalid", "tool-error", "plain"} {
		tool := &mcp.Tool{Name: name, InputSchema: input, OutputSchema: output}
		if name == "plain" {
			tool.OutputSchema = nil
		}
		server.AddTool(tool, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var data any = map[string]any{"count": 7, "secret": secret}
			text := "small receipt"
			if name == "invalid" || name == "large-invalid" {
				data = map[string]any{"count": "invalid", "secret": secret}
			}
			if name == "large-invalid" {
				text = strings.Repeat("valid-looking prose ", 1800)
			}
			if name == "missing" || name == "tool-error" || name == "plain" {
				data = nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, StructuredContent: data, IsError: name == "tool-error"}, nil
		})
	}
	server.AddTool(&mcp.Tool{Name: "unsupported", InputSchema: json.RawMessage(`{"type":"object","$schema":"https://example.invalid/draft"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		t.Fatal("unsupported tool ran")
		return nil, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer httpServer.Close()
	cfg := Config{Transport: "http", Endpoint: httpServer.URL, TokenEnv: "CLUSBOT_SCHEMA_TEST_TOKEN"}
	session, err := Connect(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	catalog, err := Discover(t.Context(), session)
	if err != nil || len(catalog.Tools) != 7 {
		t.Fatalf("discover: %+v %v", catalog, err)
	}
	for _, tool := range catalog.Tools {
		if tool.Name == "unsupported" {
			if tool.SchemaError == "" {
				t.Fatal("unsupported dialect offered as usable")
			}
			continue
		}
		if tool.SchemaError != "" {
			t.Fatal(tool.SchemaError)
		}
		if tool.Name != "plain" && len(tool.OutputSchema) == 0 {
			t.Fatal("output schema missing")
		}
		result, archive, err := Call(t.Context(), session, tool, map[string]any{}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		invalid := tool.Name == "invalid" || tool.Name == "missing" || tool.Name == "large-invalid"
		if (result.ValidationError != "") != invalid || result.IsError != (invalid || tool.Name == "tool-error") {
			t.Fatalf("%s: %+v", tool.Name, result)
		}
		if invalid && len(archive) == 0 {
			t.Fatalf("%s lost original evidence", tool.Name)
		}
		if tool.Name == "large-invalid" && !result.Truncated {
			t.Fatal("expected bounded preview")
		}
		if strings.Contains(string(result.Structured), secret) || strings.Contains(string(archive), secret) {
			t.Fatal("credential leaked")
		}
	}
}
