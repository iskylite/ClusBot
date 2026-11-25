package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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
		configured = ctx != nil && server.Store == store && server.Origin == "https://ops.example"
		return func() { cleaned = true }, expected
	})
	if !errors.Is(err, expected) || !configured || !cleaned {
		t.Fatalf("configuration: err=%v configured=%v cleaned=%v", err, configured, cleaned)
	}
	if _, err = os.Stat(filepath.Join(dir, "bootstrap-token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected bootstrap token: %v", err)
	}
}

func TestAccountStartupFixture(t *testing.T) {
	if os.Getenv("CLUSBOT_TEST_ACCOUNT_STARTUP") != "1" {
		t.Skip("local account service subprocess")
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append(os.Args[:1], os.Args[i+1:]...)
			break
		}
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
}

func TestWebAndDesktopStartTheSameAccountService(t *testing.T) {
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	const credentials = `{"username":"startup-admin","password":"only-test-password"}`
	var principal string
	for _, mode := range []string{"", "loopback"} {
		label := mode
		if label == "" {
			label = "default-web"
		} else {
			label = "app-loopback"
		}
		t.Run(label, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			args := []string{"-test.run=^TestAccountStartupFixture$", "--", "serve", "--listen", "127.0.0.1:0", "--data-dir", dir, "--print-url"}
			if mode != "" {
				args = append(args, "--mode", mode)
			}
			process := exec.CommandContext(ctx, executable, args...)
			process.Env = []string{"CLUSBOT_TEST_ACCOUNT_STARTUP=1", "GORACE=atexit_sleep_ms=0"}
			stdout, err := process.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err = process.Start(); err != nil {
				t.Fatal(err)
			}
			stopped := false
			defer func() {
				if !stopped {
					cancel()
					_ = process.Wait()
				}
			}()
			reader := bufio.NewReader(stdout)
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("startup did not emit URL: %q, %v", line, err)
			}
			origin := strings.TrimSpace(line)
			parsed, err := url.Parse(origin)
			if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" {
				t.Fatalf("unsafe or invalid startup URL %q: %v", origin, err)
			}
			go io.Copy(io.Discard, reader)
			request := func(method, path, body string, cookie *http.Cookie, status int) *http.Response {
				t.Helper()
				req, err := http.NewRequestWithContext(ctx, method, origin+"/api/v1"+path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				if method == "POST" {
					req.Header.Set("Origin", origin)
					req.Header.Set("Content-Type", "application/json")
				}
				if cookie != nil {
					req.AddCookie(cookie)
				}
				res, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { res.Body.Close() })
				if res.StatusCode != status {
					data, _ := io.ReadAll(res.Body)
					t.Fatalf("%s %s: status %d want %d (%s)", method, path, res.StatusCode, status, data)
				}
				return res
			}
			var init struct{ Required bool }
			res := request("GET", "/bootstrap", "", nil, http.StatusOK)
			if err = json.NewDecoder(res.Body).Decode(&init); err != nil || init.Required != (mode == "") {
				t.Fatalf("bootstrap required=%v, err=%v", init.Required, err)
			}
			request("GET", "/tasks", "", nil, http.StatusUnauthorized)
			request("POST", "/session", `{"token":"legacy-credential-fixture"}`, nil, http.StatusBadRequest)
			if init.Required {
				request("POST", "/bootstrap", credentials, nil, http.StatusCreated)
			}
			login := request("POST", "/session", credentials, nil, http.StatusOK)
			cookies := login.Cookies()
			if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode {
				t.Fatalf("unexpected local account cookie properties")
			}
			var session struct {
				User work.Account `json:"user"`
			}
			if err := json.NewDecoder(request("GET", "/session", "", cookies[0], http.StatusOK).Body).Decode(&session); err != nil ||
				!session.User.IsSuperadmin || session.User.PrincipalID == "" {
				t.Fatalf("session is not the initialized account: %+v %v", session, err)
			}
			if mode == "" {
				principal = session.User.PrincipalID
			} else if session.User.PrincipalID != principal {
				t.Fatal("desktop launcher did not reuse the web account")
			}
			request("GET", "/tasks", "", cookies[0], http.StatusOK)
			if err = process.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			err = process.Wait()
			stopped = true
			if err != nil {
				t.Fatalf("account service shutdown: %v", err)
			}
		})
	}
	assertNoStartupPath(t, dir, "access-token")
}
