CREATE TABLE IF NOT EXISTS submission_classifications (
  request_key TEXT PRIMARY KEY,
  fingerprint TEXT NOT NULL,
  conversation_id TEXT NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('running','completed','failed')),
  is_task INTEGER NOT NULL DEFAULT 0,
  requests INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER,
  output_tokens INTEGER,
  active_ms INTEGER NOT NULL DEFAULT 0,
  output_used INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  run_id TEXT REFERENCES runs(id) ON DELETE SET NULL,
  created_at INTEGER NOT NULL,
  route TEXT NOT NULL DEFAULT '',
  router_model_provenance TEXT NOT NULL DEFAULT ''
);
