package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"clusbot/internal/work"
)

func TestAttentionWithoutModelRequiresLogin(t *testing.T) {
	store, err := work.Open(filepath.Join(t.TempDir(), "attention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := Server{Store: store, Token: "attention-token", Hosts: []string{"127.0.0.1"}}
	h := server.Handler(fstest.MapFS{"index.html": {Data: []byte("v3")}})
	req := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
		if cookie != nil {
			request.AddCookie(cookie)
		}
		if method == http.MethodPost {
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", "http://127.0.0.1")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, request)
		return w
	}
	if got := req(http.MethodGet, "/api/v1/attention", "", nil).Code; got != http.StatusUnauthorized {
		t.Fatalf("anonymous got %d", got)
	}
	resp := req(http.MethodPost, "/api/v1/session", `{"token":"attention-token"}`, nil)
	if cookies := resp.Result().Cookies(); resp.Code != http.StatusOK || len(cookies) == 0 {
		t.Fatalf("login got %d", resp.Code)
	}
	cookie := resp.Result().Cookies()[0]
	resp = req(http.MethodGet, "/api/v1/attention", "", cookie)
	var page work.AttentionPage
	if err := json.Unmarshal(resp.Body.Bytes(), &page); err != nil || resp.Code != http.StatusOK || page.Items == nil || len(page.Items) != 0 || resp.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("empty attention with no model: %d %s %v", resp.Code, resp.Body, err)
	}
	if got := req(http.MethodGet, "/api/v1/attention?before_id=approval:x", "", cookie).Code; got != http.StatusBadRequest {
		t.Fatalf("malformed cursor got %d", got)
	}
	resp = req(http.MethodGet, "/api/v1/attention/approval:unknown", "", cookie)
	if resp.Code != http.StatusNotFound || !strings.Contains(resp.Body.String(), "attention_not_found") {
		t.Fatalf("missing item detail got %d %s", resp.Code, resp.Body)
	}
	resp = req(http.MethodGet, "/api/v1/attention/events", "", cookie)
	var feed work.AttentionFeedPage
	if err := json.Unmarshal(resp.Body.Bytes(), &feed); err != nil || resp.Code != http.StatusOK || feed.Cursor == "" || feed.Events == nil {
		t.Fatalf("empty feed without a model: %d %s %v", resp.Code, resp.Body, err)
	}
	resp = req(http.MethodGet, "/api/v1/attention/events?after=stale", "", cookie)
	if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "attention_cursor_reset") {
		t.Fatalf("stale cursor must request resnapshot: %d %s", resp.Code, resp.Body)
	}
}
