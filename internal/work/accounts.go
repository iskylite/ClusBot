package work

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"
)

//go:embed accounts.sql
var accountSchema string

func applyAccountSchema(tx *sql.Tx) error {
	if _, err := tx.Exec(accountSchema); err != nil {
		return err
	}
	return migrateAccountPasswordState(tx)
}

// migrateAccountPasswordState 保留旧账号和旧表，同时记录初始化已经完成。
func migrateAccountPasswordState(tx *sql.Tx) error {
	if err := addColumnIfMissing(tx, "accounts", "must_change_password", `ALTER TABLE accounts ADD COLUMN must_change_password INTEGER NOT NULL DEFAULT 0 CHECK(must_change_password IN (0,1))`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS account_bootstrap (singleton INTEGER PRIMARY KEY CHECK(singleton=1));
		INSERT OR IGNORE INTO account_bootstrap(singleton) SELECT 1 WHERE EXISTS(SELECT 1 FROM accounts)`); err != nil {
		return err
	}
	if err := migrateAccountLoginTime(tx); err != nil {
		return err
	}
	return migrateAccountConnectionCalls(tx)
}

func migrateAccountLoginTime(tx *sql.Tx) error {
	exists, err := columnExists(tx, "accounts", "last_login_at")
	if err != nil {
		return err
	}
	if !exists {
		if _, err = tx.Exec(`ALTER TABLE accounts ADD COLUMN last_login_at INTEGER NOT NULL DEFAULT 0;
			UPDATE accounts SET last_login_at = COALESCE((
				SELECT MAX(s.created_at) FROM account_sessions s WHERE s.principal_id = accounts.principal_id
			), 0)`); err != nil {
			return err
		}
	}
	// Record successfully created sessions even if they are later revoked or deleted.
	_, err = tx.Exec(`CREATE TRIGGER IF NOT EXISTS account_last_login_after_insert
		AFTER INSERT ON account_sessions BEGIN
			UPDATE accounts SET last_login_at = MAX(last_login_at, NEW.created_at)
			WHERE principal_id = NEW.principal_id;
		END`)
	return err
}

var (
	ErrAccountDenied     = errors.New("账号或凭据无效")
	ErrAccountExists     = errors.New("登录名已存在")
	ErrBootstrapComplete = errors.New("超级管理员已经初始化")
	ErrLastSuperadmin    = errors.New("不能停用或降级最后一位可用超级管理员")
	ErrSingleSuperadmin  = errors.New("全站仅允许唯一超级管理员，不能新建或提权为第二位超管")
	ErrAccountState      = errors.New("账号状态不允许此操作")
	ErrServerDatabase    = errors.New("此数据库已启用多人服务器模式，不能使用本机共享口令模式启动")
)

// AccountIsActive returns true when the principal exists in accounts and has status='active'.
func (s *Store) AccountIsActive(ctx context.Context, principalID string) bool {
	if s == nil || s.db == nil || strings.TrimSpace(principalID) == "" {
		return false
	}
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM accounts WHERE principal_id=?`, principalID).Scan(&status); err != nil {
		return false
	}
	return status == "active"
}

// EnsureAccountMode 防止多人数据库被意外切换回本机共享口令模式。
func (s *Store) EnsureAccountMode(ctx context.Context, server bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if server {
		if _, err = tx.ExecContext(ctx, `INSERT INTO account_instance(singleton,mode) VALUES(1,'server') ON CONFLICT(singleton) DO NOTHING`); err != nil {
			return err
		}
	} else {
		var used bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_instance) OR EXISTS(SELECT 1 FROM account_bootstrap) OR EXISTS(SELECT 1 FROM accounts) OR EXISTS(SELECT 1 FROM account_business_owner)`).Scan(&used); err != nil {
			return err
		}
		if used {
			return ErrServerDatabase
		}
	}
	return tx.Commit()
}

type AccountGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}
type Account struct {
	PrincipalID        string `json:"principal_id"`
	Username           string `json:"username"`
	DisplayName        string `json:"display_name"`
	Avatar             string `json:"avatar"`
	Status             string `json:"status"`
	IsSuperadmin       bool   `json:"is_superadmin"`
	MustChangePassword bool   `json:"must_change_password"`
	CreatedAt          int64  `json:"created_at,omitempty"`
	LastLoginAt        int64  `json:"last_login_at,omitempty"`
}
type OperationGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type AccountPatch struct {
	DisplayName  *string
	Avatar       *string
	Status       *string
	IsSuperadmin *bool
}

const accountFields = `a.principal_id,a.username,a.display_name,a.avatar,a.status,a.is_superadmin,a.must_change_password,a.created_at,a.last_login_at`

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	err := row.Scan(&a.PrincipalID, &a.Username, &a.DisplayName, &a.Avatar, &a.Status, &a.IsSuperadmin, &a.MustChangePassword, &a.CreatedAt, &a.LastLoginAt)
	return a, err
}

func groupsFor(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, principal string) ([]AccountGroup, error) {
	rows, err := q.QueryContext(ctx, `SELECT g.group_id,g.name,m.role FROM account_group_memberships m JOIN operation_groups g ON g.group_id=m.group_id WHERE m.principal_id=? ORDER BY g.name,g.group_id`, principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccountGroup{}
	for rows.Next() {
		var g AccountGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.Role); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) BootstrapRequired(ctx context.Context) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_bootstrap) OR EXISTS(SELECT 1 FROM accounts)`).Scan(&exists)
	return !exists, err
}

// BootstrapAccount 的首次检查、账号创建和关闭初始化在同一写事务中完成。
func (s *Store) BootstrapAccount(ctx context.Context, username, displayName, passwordHash string) (Account, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_bootstrap) OR EXISTS(SELECT 1 FROM accounts)`).Scan(&exists); err != nil {
		return Account{}, err
	}
	if exists {
		return Account{}, ErrBootstrapComplete
	}
	principal := "user:" + id()
	created := time.Now().UnixMilli()
	if _, err = tx.ExecContext(ctx, `INSERT INTO principals(principal_id,kind,identity_digest,enabled,created_at) VALUES(?,'user',?,1,?)`, principal, digest(principal), created); err != nil {
		return Account{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO accounts(principal_id,username,display_name,password_hash,status,is_superadmin,created_at) VALUES(?,?,?,?,'active',1,?)`, principal, username, displayName, passwordHash, created); err != nil {
		return Account{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO account_bootstrap(singleton) VALUES(1)`); err != nil {
		return Account{}, err
	}
	if err = tx.Commit(); err != nil {
		return Account{}, err
	}
	return Account{PrincipalID: principal, Username: username, DisplayName: displayName, Status: "active", IsSuperadmin: true, CreatedAt: created}, nil
}

// AccountPassword 只返回密码验证所需数据，不用于 JSON 输出。
func (s *Store) AccountPassword(ctx context.Context, username string) (string, string, error) {
	var principal, hash string
	err := s.db.QueryRowContext(ctx, `SELECT principal_id,password_hash FROM accounts WHERE username=? AND status='active'`, username).Scan(&principal, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrAccountDenied
	}
	return principal, hash, err
}

// CreateAccountSession 比较验证时的哈希，防止密码或账号状态在验证后变化。
func (s *Store) CreateAccountSession(ctx context.Context, principal, expectedHash, tokenDigest string, expiresAt int64) error {
	result, err := s.db.ExecContext(ctx, `INSERT INTO account_sessions(token_digest,principal_id,status_version,expires_at,created_at)
		SELECT ?,principal_id,status_version,?,? FROM accounts WHERE principal_id=? AND status='active' AND password_hash=?`, tokenDigest, expiresAt, time.Now().UnixMilli(), principal, expectedHash)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrAccountDenied
	}
	return nil
}

func sessionAccount(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, tokenDigest string) (Account, error) {
	a, err := scanAccount(q.QueryRowContext(ctx, `SELECT `+accountFields+` FROM account_sessions s JOIN accounts a ON a.principal_id=s.principal_id WHERE s.token_digest=? AND s.expires_at>? AND s.status_version=a.status_version AND a.status='active'`, tokenDigest, time.Now().UnixMilli()))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrAccountDenied
	}
	return a, err
}

func (s *Store) AccountForSession(ctx context.Context, tokenDigest string) (Account, error) {
	return sessionAccount(ctx, s.db, tokenDigest)
}
func (s *Store) DeleteAccountSession(ctx context.Context, tokenDigest string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM account_sessions WHERE token_digest=?`, tokenDigest)
	return err
}

func adminSession(ctx context.Context, tx *sql.Tx, tokenDigest string) (Account, error) {
	a, err := sessionAccount(ctx, tx, tokenDigest)
	if err != nil {
		return Account{}, err
	}
	if !a.IsSuperadmin || a.MustChangePassword {
		return Account{}, ErrAccountDenied
	}
	return a, nil
}

func (s *Store) ListAccounts(ctx context.Context, tokenDigest string) ([]Account, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = adminSession(ctx, tx, tokenDigest); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+accountFields+` FROM accounts a ORDER BY a.username`)
	if err != nil {
		return nil, err
	}
	out := []Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, a)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// MentionableAccount 表示前端 Composer `@` 引用成员列表所需的最小公开信息（绝不含密码、密保或会话凭据）。
type MentionableAccount struct {
	PrincipalID  string `json:"principal_id"`
	Username     string `json:"username"`
	DisplayName  string `json:"display_name"`
	Avatar       string `json:"avatar"`
	IsSuperadmin bool   `json:"is_superadmin"`
}

// ListMentionableAccounts 返回可供对话 `@` 引用的活跃成员基础资料（不含停用账号或敏感字段）。
func (s *Store) ListMentionableAccounts(ctx context.Context, limit int) ([]MentionableAccount, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT principal_id,username,display_name,avatar,is_superadmin FROM accounts WHERE status='active' ORDER BY display_name,username LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MentionableAccount{}
	for rows.Next() {
		var a MentionableAccount
		if err := rows.Scan(&a.PrincipalID, &a.Username, &a.DisplayName, &a.Avatar, &a.IsSuperadmin); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) CreateAccount(ctx context.Context, tokenDigest, username, displayName, passwordHash string, superadmin bool) (Account, error) {
	return s.CreateAccountWithPasswordPolicy(ctx, tokenDigest, username, displayName, passwordHash, superadmin, false)
}

// CreateAccountWithPasswordPolicy 由超管创建普通成员账号，支持设定首次登录强制改密；全站仅允许唯一超级管理员。
func (s *Store) CreateAccountWithPasswordPolicy(ctx context.Context, tokenDigest, username, displayName, passwordHash string, superadmin, mustChangePassword bool) (Account, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback()
	if _, err = adminSession(ctx, tx, tokenDigest); err != nil {
		return Account{}, err
	}
	if superadmin {
		var adminCount int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE is_superadmin=1`).Scan(&adminCount); err != nil {
			return Account{}, err
		}
		if adminCount > 0 {
			return Account{}, ErrSingleSuperadmin
		}
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE username=?)`, username).Scan(&exists); err != nil {
		return Account{}, err
	}
	if exists {
		return Account{}, ErrAccountExists
	}
	principal, created := "user:"+id(), time.Now().UnixMilli()
	if _, err = tx.ExecContext(ctx, `INSERT INTO principals(principal_id,kind,identity_digest,enabled,created_at) VALUES(?,'user',?,1,?)`, principal, digest(principal), created); err != nil {
		return Account{}, err
	}
	mustChange := 0
	if mustChangePassword {
		mustChange = 1
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO accounts(principal_id,username,display_name,password_hash,must_change_password,status,is_superadmin,created_at) VALUES(?,?,?,?,?,'active',?,?)`, principal, username, displayName, passwordHash, mustChange, superadmin, created); err != nil {
		return Account{}, err
	}
	if err = tx.Commit(); err != nil {
		return Account{}, err
	}
	return Account{PrincipalID: principal, Username: username, DisplayName: displayName, Status: "active", IsSuperadmin: superadmin, MustChangePassword: mustChangePassword, CreatedAt: created}, nil
}

func normalizeAvatarBadge(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	runes := []rune(trimmed)
	return string(runes[:1])
}

// UpdateMyProfile 允许当前已登录用户修改本人显示姓名与头像标识（不涉及角色或权限变更）。
func (s *Store) UpdateMyProfile(ctx context.Context, tokenDigest string, displayName, avatar *string) (Account, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback()
	a, err := sessionAccount(ctx, tx, tokenDigest)
	if err != nil {
		return Account{}, err
	}
	if displayName != nil {
		a.DisplayName = *displayName
	}
	if avatar != nil {
		a.Avatar = normalizeAvatarBadge(*avatar)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE accounts SET display_name=?,avatar=? WHERE principal_id=?`, a.DisplayName, a.Avatar, a.PrincipalID); err != nil {
		return Account{}, err
	}
	if err = tx.Commit(); err != nil {
		return Account{}, err
	}
	return a, nil
}

// UpdateAccount 在同一事务保护最后一位可用超管，并即时使停用账号会话失效。
func (s *Store) UpdateAccount(ctx context.Context, tokenDigest, principal string, patch AccountPatch) (Account, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback()
	if _, err = adminSession(ctx, tx, tokenDigest); err != nil {
		return Account{}, err
	}
	a, err := scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountFields+` FROM accounts a WHERE a.principal_id=?`, principal))
	if err != nil {
		return Account{}, err
	}
	oldStatus, oldAdmin := a.Status, a.IsSuperadmin
	if patch.DisplayName != nil {
		a.DisplayName = *patch.DisplayName
	}
	if patch.Avatar != nil {
		a.Avatar = normalizeAvatarBadge(*patch.Avatar)
	}
	if patch.Status != nil {
		a.Status = *patch.Status
	}
	if patch.IsSuperadmin != nil {
		if *patch.IsSuperadmin && !oldAdmin {
			return Account{}, ErrSingleSuperadmin
		}
		a.IsSuperadmin = *patch.IsSuperadmin
	}
	var hash string
	if err = tx.QueryRowContext(ctx, `SELECT password_hash FROM accounts WHERE principal_id=?`, principal).Scan(&hash); err != nil {
		return Account{}, err
	}
	if (a.Status == "active" && hash == "") || (a.Status == "pending" && hash != "") {
		return Account{}, ErrAccountState
	}
	if oldAdmin && oldStatus == "active" && (!a.IsSuperadmin || a.Status != "active") {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE status='active' AND is_superadmin=1`).Scan(&count); err != nil {
			return Account{}, err
		}
		if count <= 1 {
			return Account{}, ErrLastSuperadmin
		}
	}
	statusChange := 0
	if a.Status != oldStatus {
		statusChange = 1
	}
	if _, err = tx.ExecContext(ctx, `UPDATE accounts SET display_name=?,avatar=?,status=?,is_superadmin=?,status_version=status_version+? WHERE principal_id=?`, a.DisplayName, a.Avatar, a.Status, a.IsSuperadmin, statusChange, principal); err != nil {
		return Account{}, err
	}
	if statusChange == 1 {
		if _, err = tx.ExecContext(ctx, `UPDATE principals SET enabled=? WHERE principal_id=?`, a.Status == "active", principal); err != nil {
			return Account{}, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM account_sessions WHERE principal_id=?`, principal); err != nil {
			return Account{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return Account{}, err
	}
	return a, nil
}

// ResetAccountPassword 设置临时密码并在同一事务中撤销该账号的全部会话。
func (s *Store) ResetAccountPassword(ctx context.Context, tokenDigest, principal, passwordHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = adminSession(ctx, tx, tokenDigest); err != nil {
		return err
	}
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM accounts WHERE principal_id=?`, principal).Scan(&status); err != nil {
		return err
	}
	// 旧版待激活账号改为直接设密，停用账号仍保持停用。
	if _, err = tx.ExecContext(ctx, `UPDATE accounts SET password_hash=?,must_change_password=1,status_version=status_version+1,
		status=CASE WHEN status='pending' THEN 'active' ELSE status END WHERE principal_id=?`, passwordHash, principal); err != nil {
		return err
	}
	if status == "pending" {
		if _, err = tx.ExecContext(ctx, `UPDATE principals SET enabled=1 WHERE principal_id=?`, principal); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM account_sessions WHERE principal_id=?`, principal); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ChangeAccountPassword(ctx context.Context, tokenDigest, expectedHash, newHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, err := sessionAccount(ctx, tx, tokenDigest)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE accounts SET password_hash=?,must_change_password=0,status_version=status_version+1 WHERE principal_id=? AND password_hash=? AND status='active'`, newHash, a.PrincipalID, expectedHash)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrAccountDenied
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM account_sessions WHERE principal_id=?`, a.PrincipalID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListOperationGroups(ctx context.Context, tokenDigest string) ([]OperationGroup, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = adminSession(ctx, tx, tokenDigest); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT group_id,name FROM operation_groups ORDER BY name,group_id`)
	if err != nil {
		return nil, err
	}
	out := []OperationGroup{}
	for rows.Next() {
		var g OperationGroup
		if err = rows.Scan(&g.ID, &g.Name); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, g)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
func (s *Store) CreateOperationGroup(ctx context.Context, tokenDigest, name string) (OperationGroup, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OperationGroup{}, err
	}
	defer tx.Rollback()
	if _, err = adminSession(ctx, tx, tokenDigest); err != nil {
		return OperationGroup{}, err
	}
	g := OperationGroup{ID: "group:" + id(), Name: name}
	if _, err = tx.ExecContext(ctx, `INSERT INTO operation_groups(group_id,name,created_at) VALUES(?,?,?)`, g.ID, g.Name, time.Now().UnixMilli()); err != nil {
		return OperationGroup{}, err
	}
	if err = tx.Commit(); err != nil {
		return OperationGroup{}, err
	}
	return g, nil
}
func (s *Store) SetGroupMembership(ctx context.Context, tokenDigest, groupID, principal, role string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = adminSession(ctx, tx, tokenDigest); err != nil {
		return err
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM operation_groups WHERE group_id=?) AND EXISTS(SELECT 1 FROM accounts WHERE principal_id=?)`, groupID, principal).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return sql.ErrNoRows
	}
	if role == "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM account_group_memberships WHERE group_id=? AND principal_id=?`, groupID, principal)
	} else if role == "leader" || role == "member" {
		_, err = tx.ExecContext(ctx, `INSERT INTO account_group_memberships(group_id,principal_id,role) VALUES(?,?,?) ON CONFLICT(group_id,principal_id) DO UPDATE SET role=excluded.role`, groupID, principal, role)
	} else {
		return fmt.Errorf("invalid group role")
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AccountCanRunDiscovery(ctx context.Context, principal string) (bool, error) {
	var allowed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts
		WHERE principal_id=? AND status='active' AND is_superadmin=1 AND must_change_password=0)`, principal).Scan(&allowed)
	return allowed, err
}
