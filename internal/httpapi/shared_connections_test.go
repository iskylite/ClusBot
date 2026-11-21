package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/mcpclient"
	"clusbot/internal/work"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSharedConnectionCallOwnership(t *testing.T) {
	ctx := context.Background()
	store, err := work.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := Server{Store: store, ServerMode: true, Origin: "https://ops.example", Hosts: []string{"ops.example"}}
	handler := server.Handler(fstest.MapFS{})
	const testHash = "fixture-password-hash"
	admin, err := store.BootstrapAccount(ctx, "admin", "Admin", testHash)
	if err != nil {
		t.Fatal(err)
	}
	session := func(account work.Account, hash string) *http.Cookie {
		t.Helper()
		token, err := makeSecret()
		if err != nil {
			t.Fatal(err)
		}
		if err = store.CreateAccountSession(ctx, account.PrincipalID, hash, secretDigest(token), time.Now().Add(time.Hour).UnixMilli()); err != nil {
			t.Fatal(err)
		}
		return &http.Cookie{Name: accountCookie, Value: token}
	}
	adminCookie := session(admin, testHash)
	member, err := store.CreateAccount(ctx, secretDigest(adminCookie.Value), "member", "Member", testHash, false)
	if err != nil {
		t.Fatal(err)
	}
	memberInitial := session(member, testHash)
	const memberHash = "fixture-member-password-hash"
	if err := store.ChangeAccountPassword(ctx, secretDigest(memberInitial.Value), testHash, memberHash); err != nil {
		t.Fatal(err)
	}
	memberCookie := session(member, memberHash)

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	mcpServer.AddTool(&mcp.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("fixture content ", 3000)}}}, nil
	})
	t.Setenv("CLUSBOT_SHARED_CONNECTION_TEST_TOKEN", "fixture-token")
	backend := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil))
	defer backend.Close()

	request := func(method, path string, body any, cookie *http.Cookie, status int) *httptest.ResponseRecorder {
		t.Helper()
		var payload []byte
		if body != nil {
			var err error
			payload, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		r := httptest.NewRequest(method, "https://ops.example"+path, bytes.NewReader(payload))
		r.Header.Set("Origin", "https://ops.example")
		r.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s = %d, want %d: %.400s", method, path, w.Code, status, w.Body.String())
		}
		return w
	}
	readJSON := func(w *httptest.ResponseRecorder, target any) {
		t.Helper()
		if err := json.Unmarshal(w.Body.Bytes(), target); err != nil {
			t.Fatalf("invalid response %d: %.400s (%v)", w.Code, w.Body.String(), err)
		}
	}
	base := "/api/v1/connections"
	config := map[string]any{"name": "Instance MCP", "enabled": true, "config": mcpclient.Config{Transport: "http", Endpoint: backend.URL, TokenEnv: "CLUSBOT_SHARED_CONNECTION_TEST_TOKEN"}}
	request("GET", base, nil, nil, http.StatusUnauthorized)
	request("POST", base, config, memberCookie, http.StatusForbidden)
	var connection work.Connection
	readJSON(request("POST", base, config, adminCookie, http.StatusCreated), &connection)
	base += "/" + connection.ID
	request("POST", base+"/test", map[string]int{"revision": connection.Revision}, memberCookie, http.StatusForbidden)
	readJSON(request("POST", base+"/test", map[string]int{"revision": connection.Revision}, adminCookie, http.StatusOK), &connection)
	if len(connection.Tools) != 1 {
		t.Fatalf("missing test catalog: %+v", connection)
	}
	grant := map[string]any{"name": "read", "version": connection.Tools[0].Version, "revision": connection.Revision, "allowed": true}
	request("POST", base+"/permission", grant, memberCookie, http.StatusForbidden)
	request("PATCH", base, config, memberCookie, http.StatusForbidden)
	request("POST", base+"/permission", grant, adminCookie, http.StatusNoContent)
	connection.Revision++

	view := request("GET", base, nil, memberCookie, http.StatusOK)
	if strings.Contains(view.Body.String(), backend.URL) || strings.Contains(view.Body.String(), "CLUSBOT_SHARED_CONNECTION_TEST_TOKEN") || strings.Contains(view.Body.String(), `"config"`) {
		t.Fatalf("member received connection settings: %s", view.Body)
	}
	request("GET", "/api/v1/task-tools", nil, memberCookie, http.StatusOK)
	legacy, err := store.CallConnectionTool(ctx, connection.ID, "read", connection.Revision, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	call := map[string]any{"name": "read", "revision": connection.Revision, "arguments": map[string]any{}}
	var memberResult, adminResult work.ConnectionResult
	readJSON(request("POST", base+"/call", call, memberCookie, http.StatusOK), &memberResult)
	readJSON(request("POST", base+"/call", call, adminCookie, http.StatusOK), &adminResult)
	for _, pair := range []struct {
		cookie     *http.Cookie
		own, other string
	}{
		{memberCookie, memberResult.ID, adminResult.ID},
		{adminCookie, adminResult.ID, memberResult.ID},
	} {
		var calls []work.ConnectionCall
		readJSON(request("GET", base+"/calls", nil, pair.cookie, http.StatusOK), &calls)
		if len(calls) != 1 || calls[0].ID != pair.own {
			t.Fatalf("unexpected call history: %+v", calls)
		}
		request("GET", "/api/v1/tool-results/"+pair.other+"/content", nil, pair.cookie, http.StatusNotFound)
		request("GET", "/api/v1/tool-results/"+legacy.ID+"/content", nil, pair.cookie, http.StatusNotFound)
		request("GET", "/api/v1/tool-results/"+pair.own+"/content", nil, pair.cookie, http.StatusOK)
	}
	grant["revision"], grant["allowed"] = connection.Revision, false
	request("POST", base+"/permission", grant, adminCookie, http.StatusNoContent)
	request("GET", "/api/v1/tool-results/"+memberResult.ID+"/content", nil, memberCookie, http.StatusNotFound)
}
