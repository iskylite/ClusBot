package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"clusbot/internal/work"
)

type accountFixture struct {
	t       *testing.T
	store   *work.Store
	handler http.Handler
}

func newAccountFixture(t *testing.T) *accountFixture {
	t.Helper()
	store, err := work.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := Server{Store: store, ServerMode: true, Origin: "https://ops.example", Hosts: []string{"ops.example"}, Token: "legacy-token"}
	return &accountFixture{t: t, store: store, handler: s.Handler(fstest.MapFS{"index.html": {Data: []byte("test")}})}
}

func (f *accountFixture) request(method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	f.t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, "https://ops.example"+path, bytes.NewReader(raw))
	r.RemoteAddr = "127.0.0.1:12345"
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if method != "GET" && method != "HEAD" {
		r.Header.Set("Origin", "https://ops.example")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func accountJSON(t *testing.T, w *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), target); err != nil {
		t.Fatalf("invalid response %d: %s (%v)", w.Code, w.Body, err)
	}
}

func (f *accountFixture) setup() string {
	f.t.Helper()
	w := f.request("POST", "/api/v1/bootstrap", map[string]string{"username": "admin", "password": "correct horse battery staple"}, nil)
	if w.Code != http.StatusCreated {
		f.t.Fatalf("bootstrap: %d %s", w.Code, w.Body)
	}
	var result struct {
		User work.Account `json:"user"`
	}
	accountJSON(f.t, w, &result)
	if strings.Contains(w.Body.String(), "recovery") || strings.Contains(w.Body.String(), "credential") || strings.Contains(w.Body.String(), "password staple") {
		f.t.Fatalf("bootstrap exposed a secret: %s", w.Body)
	}
	return result.User.PrincipalID
}

func (f *accountFixture) login(username, password string) *http.Cookie {
	f.t.Helper()
	w := f.request("POST", "/api/v1/session", map[string]string{"username": username, "password": password}, nil)
	if w.Code != http.StatusOK {
		f.t.Fatalf("login %s: %d %s", username, w.Code, w.Body)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		f.t.Fatalf("unexpected cookies: %v", cookies)
	}
	return cookies[0]
}

func TestServerBootstrapSessionAndLegacyRoutesClosed(t *testing.T) {
	f := newAccountFixture(t)
	w := f.request("GET", "/api/v1/bootstrap", nil, nil)
	if w.Code != 200 || w.Body.String() != "{\"required\":true}\n" {
		t.Fatalf("status before bootstrap: %d %s", w.Code, w.Body)
	}
	admin := f.setup()
	if admin == "" {
		t.Fatal("missing account ID")
	}
	w = f.request("GET", "/api/v1/bootstrap", nil, nil)
	if w.Code != 200 || w.Body.String() != "{\"required\":false}\n" {
		t.Fatalf("status after bootstrap: %d %s", w.Code, w.Body)
	}
	w = f.request("POST", "/api/v1/bootstrap", map[string]string{"username": "other", "password": "another safe password"}, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("second bootstrap accepted: %d %s", w.Code, w.Body)
	}
	w = f.request("POST", "/api/v1/session", map[string]string{"token": "legacy-token"}, nil)
	if w.Code == 200 {
		t.Fatal("legacy login accepted in server mode")
	}
	cookie := f.login("admin", "correct horse battery staple")
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("unsafe cookie: %+v", cookie)
	}
	w = f.request("GET", "/api/v1/session", nil, cookie)
	var sess struct {
		Authenticated bool         `json:"authenticated"`
		User          work.Account `json:"user"`
	}
	accountJSON(t, w, &sess)
	if !sess.Authenticated || sess.User.PrincipalID != admin || !sess.User.IsSuperadmin || sess.User.MustChangePassword {
		t.Fatalf("bad session: %d %s", w.Code, w.Body)
	}
	for _, old := range []struct{ method, path string }{
		{"GET", "/api/v1/me/assistant"}, {"GET", "/api/v1/admin/groups"},
		{"POST", "/api/v1/admin/users/" + admin + "/credential"}, {"POST", "/api/v1/password/credential"},
	} {
		w = f.request(old.method, old.path, map[string]string{}, cookie)
		if w.Code < 400 {
			t.Fatalf("legacy route %s was available: %d %s", old.path, w.Code, w.Body)
		}
	}
	w = f.request("PATCH", "/api/v1/admin/users/"+admin, map[string]any{"status": "disabled"}, cookie)
	if w.Code != http.StatusConflict {
		t.Fatalf("last admin not protected: %d %s", w.Code, w.Body)
	}
	w = f.request("PATCH", "/api/v1/admin/users/"+admin, map[string]any{"is_superadmin": false}, cookie)
	if w.Code != http.StatusConflict {
		t.Fatalf("last admin demotion accepted: %d %s", w.Code, w.Body)
	}
	r := httptest.NewRequest("POST", "https://ops.example/api/v1/session", bytes.NewBufferString(`{"username":"admin","password":"correct horse battery staple"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://attacker.example")
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("foreign origin accepted: %d", w.Code)
	}
	local := Server{Store: f.store, Token: "legacy-token", Hosts: []string{"ops.example"}}.Handler(fstest.MapFS{"index.html": {Data: []byte("test")}})
	r = httptest.NewRequest("POST", "http://ops.example/api/v1/session", bytes.NewBufferString(`{"token":"legacy-token"}`))
	r.Header.Set("Origin", "http://ops.example")
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	local.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("server database reopened in local mode: %d", w.Code)
	}
}

func TestConcurrentBootstrapOnlyOneSucceeds(t *testing.T) {
	f := newAccountFixture(t)
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = f.request("POST", "/api/v1/bootstrap", map[string]string{"username": "admin", "password": "correct horse battery staple"}, nil).Code
		}(i)
	}
	wg.Wait()
	if !((codes[0] == 201 && codes[1] == 409) || (codes[0] == 409 && codes[1] == 201)) {
		t.Fatalf("concurrent bootstrap outcomes: %v", codes)
	}
}

func TestAccountCreationResetAndRevocation(t *testing.T) {
	f := newAccountFixture(t)
	f.setup()
	admin := f.login("admin", "correct horse battery staple")
	w := f.request("POST", "/api/v1/admin/users", map[string]any{"username": "member", "display_name": "成员", "password": "member password 123"}, admin)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var created struct {
		User work.Account `json:"user"`
	}
	accountJSON(t, w, &created)
	if created.User.Status != "active" || created.User.MustChangePassword || strings.Contains(w.Body.String(), "password 123") || strings.Contains(w.Body.String(), "credential") {
		t.Fatalf("bad create response: %s", w.Body)
	}
	first := f.login("member", "member password 123")
	second := f.login("member", "member password 123")
	w = f.request("GET", "/api/v1/admin/users", nil, first)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member administered accounts: %d %s", w.Code, w.Body)
	}
	w = f.request("POST", "/api/v1/admin/users/"+created.User.PrincipalID+"/password", map[string]string{"password": "unauthorized password"}, first)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member reset a password: %d %s", w.Code, w.Body)
	}
	w = f.request("POST", "/api/v1/admin/users/"+created.User.PrincipalID+"/password", map[string]string{"password": "temporary password 123"}, admin)
	if w.Code != http.StatusNoContent {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	for _, old := range []*http.Cookie{first, second} {
		w = f.request("GET", "/api/v1/admin/users", nil, old)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("old session survived reset: %d %s", w.Code, w.Body)
		}
	}
	w = f.request("POST", "/api/v1/session", map[string]string{"username": "member", "password": "member password 123"}, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("old password survived reset: %d %s", w.Code, w.Body)
	}
	temporary := f.login("member", "temporary password 123")
	otherTemporary := f.login("member", "temporary password 123")
	w = f.request("GET", "/api/v1/session", nil, temporary)
	var session struct {
		User work.Account `json:"user"`
	}
	accountJSON(t, w, &session)
	if !session.User.MustChangePassword {
		t.Fatalf("temporary session did not require password change: %s", w.Body)
	}
	for _, path := range []string{"/api/v1/admin/users", "/api/v1/conversations"} {
		w = f.request("GET", path, nil, temporary)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "password_change_required") {
			t.Fatalf("temporary session accessed %s: %d %s", path, w.Code, w.Body)
		}
	}
	w = f.request("POST", "/api/v1/me/password", map[string]string{"current_password": "wrong", "new_password": "member new password 123"}, temporary)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password accepted: %d %s", w.Code, w.Body)
	}
	w = f.request("POST", "/api/v1/me/password", map[string]string{"current_password": "temporary password 123", "new_password": "temporary password 123"}, temporary)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unchanged password accepted: %d %s", w.Code, w.Body)
	}
	w = f.request("POST", "/api/v1/me/password", map[string]string{"current_password": "temporary password 123", "new_password": "member new password 123"}, temporary)
	if w.Code != http.StatusNoContent {
		t.Fatalf("password change: %d %s", w.Code, w.Body)
	}
	for _, old := range []*http.Cookie{temporary, otherTemporary} {
		w = f.request("GET", "/api/v1/session", nil, old)
		if !strings.Contains(w.Body.String(), `"authenticated":false`) {
			t.Fatalf("temporary session survived password change: %s", w.Body)
		}
	}
	member := f.login("member", "member new password 123")
	w = f.request("GET", "/api/v1/session", nil, member)
	accountJSON(t, w, &session)
	if session.User.MustChangePassword {
		t.Fatalf("password change flag not cleared: %s", w.Body)
	}
	w = f.request("PATCH", "/api/v1/admin/users/"+created.User.PrincipalID, map[string]string{"status": "disabled"}, admin)
	if w.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", w.Code, w.Body)
	}
	w = f.request("GET", "/api/v1/admin/users", nil, member)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("disabled session still active: %d %s", w.Code, w.Body)
	}
	w = f.request("PATCH", "/api/v1/admin/users/"+created.User.PrincipalID, map[string]string{"status": "active"}, admin)
	if w.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", w.Code, w.Body)
	}
	member = f.login("member", "member new password 123")
	w = f.request("DELETE", "/api/v1/session", nil, member)
	if w.Code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", w.Code, w.Body)
	}
	w = f.request("GET", "/api/v1/session", nil, member)
	if !strings.Contains(w.Body.String(), `"authenticated":false`) {
		t.Fatalf("logged out session still active: %s", w.Body)
	}
}

func TestServerPagesServeApplicationShellAndKeepAPIGates(t *testing.T) {
	f := newAccountFixture(t)
	for _, path := range []string{"/", "/account", "/team-tasks", "/operations", "/overview", "/settings"} {
		w := f.request(http.MethodGet, path, nil, nil)
		if w.Code != http.StatusOK || w.Body.String() != "test" || w.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("browser entry %s = %d, %s", path, w.Code, w.Body)
		}
	}
	if w := f.request(http.MethodGet, "/api/v1/admin/users", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("admin API no longer required authentication: %d", w.Code)
	}
	if w := f.request(http.MethodGet, "/not-a-route", nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown page returned %d", w.Code)
	}
}
