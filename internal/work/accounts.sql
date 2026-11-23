-- 用户身份和组关系与工作记录同库，以便写操作在同一事务内重验权限。
CREATE TABLE IF NOT EXISTS account_instance (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  mode TEXT NOT NULL CHECK(mode='server')
);
CREATE TABLE IF NOT EXISTS account_bootstrap (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1)
);
CREATE TABLE IF NOT EXISTS accounts (
  principal_id TEXT PRIMARY KEY REFERENCES principals(principal_id),
  username TEXT NOT NULL COLLATE NOCASE UNIQUE,
  display_name TEXT NOT NULL,
  avatar TEXT NOT NULL DEFAULT '',
  password_hash TEXT NOT NULL DEFAULT '',
  must_change_password INTEGER NOT NULL DEFAULT 0 CHECK(must_change_password IN (0,1)),
  status TEXT NOT NULL CHECK(status IN ('active','disabled','pending')),
  is_superadmin INTEGER NOT NULL DEFAULT 0 CHECK(is_superadmin IN (0,1)),
  status_version INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  last_login_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS operation_groups (
  group_id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS account_group_memberships (
  group_id TEXT NOT NULL REFERENCES operation_groups(group_id),
  principal_id TEXT NOT NULL REFERENCES accounts(principal_id),
  role TEXT NOT NULL CHECK(role IN ('leader','member')),
  PRIMARY KEY(group_id, principal_id)
);
CREATE TABLE IF NOT EXISTS account_sessions (
  token_digest TEXT PRIMARY KEY,
  principal_id TEXT NOT NULL REFERENCES accounts(principal_id),
  status_version INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS account_sessions_principal ON account_sessions(principal_id);
CREATE TABLE IF NOT EXISTS account_credentials (
  token_digest TEXT PRIMARY KEY,
  principal_id TEXT NOT NULL REFERENCES accounts(principal_id),
  purpose TEXT NOT NULL CHECK(purpose IN ('activate','reset')),
  expires_at INTEGER NOT NULL,
  consumed_at INTEGER
);
CREATE INDEX IF NOT EXISTS account_credentials_principal ON account_credentials(principal_id);
CREATE TABLE IF NOT EXISTS account_recovery_codes (
  principal_id TEXT PRIMARY KEY REFERENCES accounts(principal_id),
  token_digest TEXT NOT NULL UNIQUE
);
