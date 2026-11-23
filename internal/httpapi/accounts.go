package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"clusbot/internal/work"
	"golang.org/x/crypto/argon2"
)

const accountCookie = "clusbot_account_session"
const sessionLifetime = 12 * time.Hour

// Only these routes have an account-aware authorization boundary. Every other
// API remains unavailable in server mode until its data paths are audited.
func serverAccountRoute(pattern string) (allowed, public bool) {
	switch pattern {
	case "GET /api/v1/session", "POST /api/v1/session", "DELETE /api/v1/session", "GET /api/v1/bootstrap", "POST /api/v1/bootstrap":
		return true, true
	case "POST /api/v1/me/password", "PATCH /api/v1/me/profile", "GET /api/v1/admin/users", "POST /api/v1/admin/users", "PATCH /api/v1/admin/users/{id}", "POST /api/v1/admin/users/{id}/password":
		return true, false
	default:
		return false, false
	}
}

// The limiter is shared by all requests served by one Handler. The proxy address
// is deliberately used; untrusted forwarding headers never choose the key.
type authLimiter struct {
	mu      sync.Mutex
	entries map[string]limitWindow
	hashes  chan struct{}
}
type limitWindow struct {
	start time.Time
	count int
}

func newAuthLimiter() *authLimiter {
	return &authLimiter{entries: make(map[string]limitWindow), hashes: make(chan struct{}, 2)}
}
func (l *authLimiter) allow(r *http.Request, action string) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	now := time.Now()
	key := action + ":" + host
	globalKey := "all:" + host
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, v := range l.entries {
		if now.Sub(v.start) >= 15*time.Minute {
			delete(l.entries, k)
		}
	}
	if len(l.entries) >= 4095 {
		if _, ok := l.entries[key]; !ok {
			return false
		}
	}
	global := l.entries[globalKey]
	if global.start.IsZero() {
		global.start = now
	}
	global.count++
	l.entries[globalKey] = global
	if global.count > 200 {
		return false
	}
	v := l.entries[key]
	if v.start.IsZero() {
		v.start = now
	}
	v.count++
	l.entries[key] = v
	return v.count <= 10
}
func (l *authLimiter) success(r *http.Request, action string) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	l.mu.Lock()
	delete(l.entries, action+":"+host)
	l.mu.Unlock()
}
func (l *authLimiter) withHash(ctx context.Context, fn func() string) (string, error) {
	select {
	case l.hashes <- struct{}{}:
		defer func() { <-l.hashes }()
		return fn(), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
func makeSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
func secretDigest(secret string) string {
	d := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(d[:])
}
func validSecret(secret string) bool {
	b, err := base64.RawURLEncoding.DecodeString(secret)
	return err == nil && len(b) == 32
}
func validUsername(name string) bool {
	if len(name) < 3 || len(name) > 64 {
		return false
	}
	for i, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || (i > 0 && (c == '_' || c == '-' || c == '.'))) {
			return false
		}
	}
	return true
}
func validName(name string) bool {
	return strings.TrimSpace(name) != "" && utf8.RuneCountInString(name) <= 120
}
func validPassword(password string) bool { return len(password) >= 12 && len(password) <= 1024 }

const passwordParams = "$argon2id$v=19$m=65536,t=3,p=2$"

func passwordHash(password string) (string, error) {
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt[:], 3, 64*1024, 2, 32)
	return passwordParams + base64.RawStdEncoding.EncodeToString(salt[:]) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}
func passwordMatches(password, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 6 || "$"+strings.Join(parts[1:4], "$")+"$" != passwordParams {
		return false
	}
	salt, e1 := base64.RawStdEncoding.DecodeString(parts[4])
	expected, e2 := base64.RawStdEncoding.DecodeString(parts[5])
	if e1 != nil || e2 != nil || len(salt) != 16 || len(expected) != 32 {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}
func (s Server) hashPassword(ctx context.Context, value string) (string, error) {
	var hashErr error
	hash, err := s.authLimiter.withHash(ctx, func() string { var h string; h, hashErr = passwordHash(value); return h })
	if err != nil {
		return "", err
	}
	return hash, hashErr
}
func (s Server) checkPassword(ctx context.Context, value, hash string) (bool, error) {
	// An absent user does the same expensive derivation as an existing account.
	if hash == "" {
		hash = passwordParams + base64.RawStdEncoding.EncodeToString(make([]byte, 16)) + "$" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	}
	out, err := s.authLimiter.withHash(ctx, func() string {
		if passwordMatches(value, hash) {
			return "1"
		}
		return "0"
	})
	return out == "1", err
}
func (s Server) limitAuth(w http.ResponseWriter, r *http.Request, action string) bool {
	if !s.authLimiter.allow(r, action) {
		w.Header().Set("Retry-After", "900")
		fail(w, http.StatusTooManyRequests, "rate_limited", "尝试次数过多，请稍后再试")
		return false
	}
	return true
}
func (s Server) accountSessionDigest(r *http.Request) string {
	c, err := r.Cookie(accountCookie)
	if err != nil || !validSecret(c.Value) {
		return ""
	}
	return secretDigest(c.Value)
}

// Server deployments use HTTPS. The packaged desktop listener is restricted
// to an explicit 127.0.0.1 port and uses HTTP without a reverse proxy.
func (s Server) secureAccountCookie() bool {
	u, err := url.Parse(s.Origin)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" ||
		u.String() != "http://"+u.Host {
		return true
	}
	return false
}

type accountContextKey struct{}

// accountFromContext 只读取由授权中间件验证后写入的账号。
func accountFromContext(r *http.Request) (work.Account, bool) {
	a, ok := r.Context().Value(accountContextKey{}).(work.Account)
	return a, ok
}

func (s Server) requestAccount(r *http.Request) (work.Account, error) {
	if a, ok := accountFromContext(r); ok {
		return a, nil
	}
	d := s.accountSessionDigest(r)
	if d == "" || s.Store == nil {
		return work.Account{}, work.ErrAccountDenied
	}
	return s.Store.AccountForSession(r.Context(), d)
}
func (s Server) sessionStatus(w http.ResponseWriter, r *http.Request) {
	if !s.ServerMode {
		jsonResponse(w, 200, map[string]any{"authenticated": s.authenticated(r), "mode": "local"})
		return
	}
	a, err := s.requestAccount(r)
	if errors.Is(err, work.ErrAccountDenied) {
		jsonResponse(w, 200, map[string]any{"authenticated": false, "mode": "server"})
		return
	}
	if err != nil {
		accountError(w, err)
		return
	}
	jsonResponse(w, 200, map[string]any{"authenticated": true, "mode": "server", "user": a, "capabilities": map[string]bool{"manage_users": a.IsSuperadmin && !a.MustChangePassword}})
}
func (s Server) accountLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Password) > 1024 || len(in.Username) > 64 {
		fail(w, 401, "unauthorized", "登录名或密码不正确")
		return
	}
	action := "login:" + strings.ToLower(strings.TrimSpace(in.Username))
	if !s.limitAuth(w, r, action) {
		return
	}
	principal, hash, err := s.Store.AccountPassword(r.Context(), strings.TrimSpace(in.Username))
	if err != nil && !errors.Is(err, work.ErrAccountDenied) {
		accountError(w, err)
		return
	}
	valid, err := s.checkPassword(r.Context(), in.Password, hash)
	if err != nil {
		accountError(w, err)
		return
	}
	if !valid || principal == "" {
		fail(w, 401, "unauthorized", "登录名或密码不正确")
		return
	}
	secret, err := makeSecret()
	if err != nil {
		accountError(w, err)
		return
	}
	if err = s.Store.CreateAccountSession(r.Context(), principal, hash, secretDigest(secret), time.Now().Add(sessionLifetime).UnixMilli()); err != nil {
		accountError(w, err)
		return
	}
	s.authLimiter.success(r, action)
	http.SetCookie(w, &http.Cookie{Name: accountCookie, Value: secret, Path: "/", HttpOnly: true, Secure: s.secureAccountCookie(), SameSite: http.SameSiteStrictMode, MaxAge: int(sessionLifetime.Seconds())})
	jsonResponse(w, 200, map[string]bool{"authenticated": true})
}
func (s Server) endSession(w http.ResponseWriter, r *http.Request) {
	if s.ServerMode {
		if d := s.accountSessionDigest(r); d != "" {
			if err := s.Store.DeleteAccountSession(r.Context(), d); err != nil {
				accountError(w, err)
				return
			}
		}
		http.SetCookie(w, &http.Cookie{Name: accountCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secureAccountCookie(), SameSite: http.SameSiteStrictMode})
	} else {
		http.SetCookie(w, &http.Cookie{Name: "clusbot_session", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s Server) bootstrapStatus(w http.ResponseWriter, r *http.Request) {
	if !s.ServerMode {
		http.NotFound(w, r)
		return
	}
	required, err := s.Store.BootstrapRequired(r.Context())
	if err != nil {
		accountError(w, err)
		return
	}
	jsonResponse(w, 200, map[string]bool{"required": required})
}
func (s Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	if !s.ServerMode {
		http.NotFound(w, r)
		return
	}
	if !s.limitAuth(w, r, "bootstrap") {
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if !validUsername(in.Username) || !validPassword(in.Password) {
		fail(w, 400, "invalid_account", "登录名须为 3–64 位字母、数字或 ._-，密码至少 12 字节")
		return
	}
	hash, err := s.hashPassword(r.Context(), in.Password)
	if err != nil {
		accountError(w, err)
		return
	}
	a, err := s.Store.BootstrapAccount(r.Context(), in.Username, in.Username, hash)
	if err != nil {
		accountError(w, err)
		return
	}
	jsonResponse(w, 201, map[string]any{"user": a})
}
func (s Server) changePassword(w http.ResponseWriter, r *http.Request) {
	if !s.limitAuth(w, r, "password") {
		return
	}
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.CurrentPassword) > 1024 || !validPassword(in.NewPassword) {
		fail(w, 400, "invalid_password", "新密码至少 12 字节")
		return
	}
	if in.CurrentPassword == in.NewPassword {
		fail(w, 400, "invalid_password", "新密码不能与当前密码相同")
		return
	}
	a, err := s.requestAccount(r)
	if err != nil {
		accountError(w, err)
		return
	}
	_, oldHash, err := s.Store.AccountPassword(r.Context(), a.Username)
	if err != nil {
		accountError(w, err)
		return
	}
	valid, err := s.checkPassword(r.Context(), in.CurrentPassword, oldHash)
	if err != nil {
		accountError(w, err)
		return
	}
	if !valid {
		fail(w, 401, "unauthorized", "当前密码不正确")
		return
	}
	newHash, err := s.hashPassword(r.Context(), in.NewPassword)
	if err != nil {
		accountError(w, err)
		return
	}
	if err = s.Store.ChangeAccountPassword(r.Context(), s.accountSessionDigest(r), oldHash, newHash); err != nil {
		accountError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: accountCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secureAccountCookie(), SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}
func (s Server) updateMyProfile(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DisplayName *string `json:"display_name"`
		Avatar      *string `json:"avatar"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.DisplayName != nil {
		n := strings.TrimSpace(*in.DisplayName)
		in.DisplayName = &n
		if !validName(n) {
			fail(w, 400, "invalid_account", "姓名格式无效")
			return
		}
	}
	if in.Avatar != nil && len(*in.Avatar) > 1024 {
		fail(w, 400, "invalid_account", "头像信息过长")
		return
	}
	a, err := s.Store.UpdateMyProfile(r.Context(), s.accountSessionDigest(r), in.DisplayName, in.Avatar)
	if err != nil {
		accountError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, a)
}
func (s Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	users, err := s.Store.ListAccounts(r.Context(), s.accountSessionDigest(r))
	if err != nil {
		accountError(w, err)
		return
	}
	jsonResponse(w, 200, users)
}
func (s Server) createAccount(w http.ResponseWriter, r *http.Request) {
	author, err := s.requestAccount(r)
	if err != nil || !author.IsSuperadmin || author.MustChangePassword {
		accountError(w, work.ErrAccountDenied)
		return
	}
	var in struct {
		Username           string `json:"username"`
		DisplayName        string `json:"display_name"`
		Password           string `json:"password"`
		IsSuperadmin       bool   `json:"is_superadmin"`
		MustChangePassword bool   `json:"must_change_password"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	if in.DisplayName == "" {
		in.DisplayName = in.Username
	}
	if !validUsername(in.Username) || !validName(in.DisplayName) || !validPassword(in.Password) {
		fail(w, 400, "invalid_account", "登录名或姓名格式无效，密码至少 12 字节")
		return
	}
	hash, err := s.hashPassword(r.Context(), in.Password)
	if err != nil {
		accountError(w, err)
		return
	}
	a, err := s.Store.CreateAccountWithPasswordPolicy(r.Context(), s.accountSessionDigest(r), in.Username, in.DisplayName, hash, in.IsSuperadmin, in.MustChangePassword)
	if err != nil {
		accountError(w, err)
		return
	}
	jsonResponse(w, 201, map[string]any{"user": a})
}
func (s Server) updateAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DisplayName  *string `json:"display_name"`
		Avatar       *string `json:"avatar"`
		Status       *string `json:"status"`
		IsSuperadmin *bool   `json:"is_superadmin"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.DisplayName != nil {
		n := strings.TrimSpace(*in.DisplayName)
		in.DisplayName = &n
		if !validName(n) {
			fail(w, 400, "invalid_account", "姓名格式无效")
			return
		}
	}
	if in.Avatar != nil && len(*in.Avatar) > 1024 {
		fail(w, 400, "invalid_account", "头像信息过长")
		return
	}
	if in.Status != nil && *in.Status != "active" && *in.Status != "disabled" {
		fail(w, 400, "invalid_account", "账号状态无效")
		return
	}
	a, err := s.Store.UpdateAccount(r.Context(), s.accountSessionDigest(r), r.PathValue("id"), work.AccountPatch{DisplayName: in.DisplayName, Avatar: in.Avatar, Status: in.Status, IsSuperadmin: in.IsSuperadmin})
	if err != nil {
		accountError(w, err)
		return
	}
	jsonResponse(w, 200, a)
}
func (s Server) resetAccountPassword(w http.ResponseWriter, r *http.Request) {
	author, err := s.requestAccount(r)
	if err != nil || !author.IsSuperadmin || author.MustChangePassword {
		accountError(w, work.ErrAccountDenied)
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !validPassword(in.Password) {
		fail(w, 400, "invalid_password", "临时密码至少 12 字节")
		return
	}
	hash, err := s.hashPassword(r.Context(), in.Password)
	if err != nil {
		accountError(w, err)
		return
	}
	if err = s.Store.ResetAccountPassword(r.Context(), s.accountSessionDigest(r), r.PathValue("id"), hash); err != nil {
		accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func accountError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, work.ErrAccountDenied):
		fail(w, 403, "account_denied", "账号或凭据无效")
	case errors.Is(err, work.ErrAccountExists):
		fail(w, 409, "account_exists", "登录名已存在")
	case errors.Is(err, work.ErrBootstrapComplete):
		fail(w, 409, "bootstrap_complete", "初始化已经完成")
	case errors.Is(err, work.ErrLastSuperadmin):
		fail(w, 409, "last_superadmin", "不能停用或降级最后一位可用超级管理员")
	case errors.Is(err, work.ErrSingleSuperadmin):
		fail(w, 409, "single_superadmin_only", "全站仅允许唯一超级管理员，不能新建或提权为第二位超管")
	case errors.Is(err, work.ErrAccountState):
		fail(w, 409, "account_state", "账号状态不允许此操作")
	case errors.Is(err, sql.ErrNoRows):
		fail(w, 404, "not_found", "记录不存在")
	default:
		fail(w, 500, "internal_error", "操作未完成")
	}
}
