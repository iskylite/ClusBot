-- P4-15：观测接入（V3 §4.2、§10.1、E02/E04、F03/F12）。
CREATE TABLE IF NOT EXISTS fault_segments (
  segment_id TEXT PRIMARY KEY,
  fault_key TEXT NOT NULL,
  observation_refs TEXT NOT NULL DEFAULT '[]',
  window_start INTEGER NOT NULL,
  window_end INTEGER NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('active','recovered','recurred')) DEFAULT 'active',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS segments_by_fault ON fault_segments(fault_key, state, window_end);
