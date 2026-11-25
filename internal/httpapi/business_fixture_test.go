package httpapi

import (
	"context"
	"database/sql"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"clusbot/internal/work"
)

const businessFixtureUsername = "fixture-operator"
const businessFixturePassword = "fixture account password for HTTP tests"
const businessFixtureCredentials = `{"username":"` + businessFixtureUsername + `","password":"` + businessFixturePassword + `"}`

var fixturePasswordHash = sync.OnceValues(func() (string, error) { return passwordHash(businessFixturePassword) })

type authenticationFixture struct {
	Store     *work.Store
	Principal string
}

var testAuthStores sync.Map // *testing.T -> authenticationFixture

func businessFixtureAuth(t *testing.T) authenticationFixture {
	t.Helper()
	if value, ok := testAuthStores.Load(t); ok {
		return value.(authenticationFixture)
	}
	auth, err := work.Open(filepath.Join(t.TempDir(), "account.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testAuthStores.Delete(t)
		auth.Close()
	})
	hash, err := fixturePasswordHash()
	if err != nil {
		t.Fatal(err)
	}
	account, err := auth.BootstrapAccount(t.Context(), businessFixtureUsername, businessFixtureUsername, hash)
	if err != nil {
		t.Fatal(err)
	}
	fixture := authenticationFixture{Store: auth, Principal: account.PrincipalID}
	testAuthStores.Store(t, fixture)
	return fixture
}

func businessFixturePrincipal(t *testing.T, store *work.Store) string {
	t.Helper()
	owner := businessFixtureAuth(t).Principal
	if err := store.BindAccountBusinessStore(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	return owner
}

// businessFixtureHandler exercises the business mux with real account sessions.
// It deliberately does not apply the deployment allowlist: some unit tests
// exercise functions not yet exposed to users by the account deployment. Its
// request path must never be used in binaries or tests of public authorization.
func businessFixtureHandler(t *testing.T, s Server) http.Handler {
	t.Helper()
	businessFixturePrincipal(t, s.Store)
	auth := s
	auth.Store = businessFixtureAuth(t).Store
	auth.authLimiter = newAuthLimiter()
	authRoutes := auth.routes()
	api := s.routes()
	bridge := &assistantBridge{pending: make(map[string]assistantPreview)}
	bridge.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/provider/benchmark-events" {
			s.benchmarkEvent(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") != "http://"+r.Host {
			fail(w, 403, "invalid_origin", "请求来源无效")
			return
		}
		ctx := context.WithValue(r.Context(), assistantBridgeKey{}, bridge)
		_, route := api.Handler(r)
		_, public := serverAccountRoute(route)
		if public {
			authRoutes.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		{
			account, err := auth.requestAccount(r)
			if err != nil {
				fail(w, 401, "unauthorized", "请先登录")
				return
			}
			ctx = context.WithValue(ctx, accountContextKey{}, account)
		}
		api.ServeHTTP(w, r.WithContext(ctx))
	})
	return bridge
}

// productionFixtureHandler keeps real public authorization and routing. The
// per-test database is dedicated to this fixture's only account.
func productionFixtureHandler(t *testing.T, s Server, origin string, assets fs.FS) http.Handler {
	t.Helper()
	businessFixturePrincipal(t, s.Store)
	store := s.Store
	s.Store = businessFixtureAuth(t).Store
	s.Origin = origin
	s.PersonalAPI = func(string) (http.Handler, error) { return s.PersonalHandler(store), nil }
	return s.Handler(assets)
}

func businessFixtureLogin(t *testing.T, handler http.Handler, origin string) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, origin+"/api/v1/session", strings.NewReader(businessFixtureCredentials))
	req.Header.Set("Origin", origin)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	cookies := rec.Result().Cookies()
	if rec.Code != http.StatusOK || len(cookies) != 1 || cookies[0].Name != accountCookie {
		t.Fatalf("fixture login failed: %d %s", rec.Code, rec.Body)
	}
	return cookies[0]
}

// Existing demo datasets predate account principals. Test fixtures explicitly
// assign their sample grants to their owner; production never reassigns them.
func assignFixtureGrants(t *testing.T, dbPath string, owner string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		"UPDATE authorizations SET subject=? WHERE subject='user:session'",
		"UPDATE approvals SET subject=? WHERE subject='user:session'",
	} {
		if _, err := db.ExecContext(t.Context(), statement, owner); err != nil {
			t.Fatal(err)
		}
	}
}
