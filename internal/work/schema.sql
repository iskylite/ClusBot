-- Current ClusBot schema for an empty database.

CREATE TABLE conversations (
  id TEXT PRIMARY KEY, title TEXT NOT NULL, created_at INTEGER NOT NULL,
  archived INTEGER NOT NULL DEFAULT 0,
  revision INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE tasks (
  id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL UNIQUE REFERENCES conversations(id),
  title TEXT NOT NULL, goal TEXT NOT NULL, scope_label TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('active','waiting_user','interrupted','completed','cancelled')),
  reason TEXT NOT NULL, revision INTEGER NOT NULL DEFAULT 1,
  updated_at INTEGER NOT NULL, completed_at INTEGER,
  acceptance_criteria TEXT NOT NULL DEFAULT '',
  required_filename TEXT NOT NULL DEFAULT '',
  requirements_changed INTEGER NOT NULL DEFAULT 0 CHECK(requirements_changed IN (0,1)),
  status_reason TEXT NOT NULL DEFAULT '',
  state_changed_at INTEGER NOT NULL DEFAULT 0,
  priority TEXT NOT NULL DEFAULT 'normal' CHECK(priority IN ('high','normal','low'))
);

CREATE TABLE runs (
  id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id),
  task_id TEXT REFERENCES tasks(id), status TEXT NOT NULL, response TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, control TEXT NOT NULL DEFAULT '',
  usage_tracked INTEGER NOT NULL DEFAULT 0,
  model_requests INTEGER NOT NULL DEFAULT 0,
  active_ms INTEGER NOT NULL DEFAULT 0,
  time_partial INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER,
  output_tokens INTEGER,
  last_input_tokens INTEGER,
  error_code TEXT NOT NULL DEFAULT '',
  reported_requests INTEGER NOT NULL DEFAULT 0,
  execution_generation INTEGER NOT NULL DEFAULT 0,
  read_failure_run_id TEXT REFERENCES runs(id),
  memory_command INTEGER NOT NULL DEFAULT 0,
  admission_requests INTEGER NOT NULL DEFAULT 0,
  admission_output_used INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE messages (
  seq INTEGER PRIMARY KEY AUTOINCREMENT, conversation_id TEXT NOT NULL REFERENCES conversations(id),
  run_id TEXT NOT NULL REFERENCES runs(id), role TEXT NOT NULL, text TEXT NOT NULL,
  replace_materials INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE submissions (
  request_key TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, run_id TEXT NOT NULL REFERENCES runs(id)
);

CREATE TABLE run_events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(id),
  kind TEXT NOT NULL, data TEXT NOT NULL
);

CREATE TABLE artifacts (
  id TEXT PRIMARY KEY, run_id TEXT NOT NULL UNIQUE REFERENCES runs(id),
  task_id TEXT NOT NULL REFERENCES tasks(id), name TEXT NOT NULL, content TEXT NOT NULL,
  sha256 TEXT NOT NULL, created_at INTEGER NOT NULL
);

CREATE TABLE publications (
  id TEXT PRIMARY KEY,
  request_key TEXT NOT NULL UNIQUE,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  artifact_id TEXT NOT NULL REFERENCES artifacts(id),
  run_id TEXT NOT NULL REFERENCES runs(id),
  task_revision INTEGER NOT NULL,
  workspace_id TEXT NOT NULL,
  filename TEXT NOT NULL,
  sha256 TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending','dispatched','succeeded','denied','conflict','failed','unknown','not_applied','expired')),
  decision TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '',
  expires_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  previous_sha256 TEXT NOT NULL DEFAULT '',
  previous_version TEXT NOT NULL DEFAULT '',
  previous_content TEXT NOT NULL DEFAULT ''
);

CREATE TABLE materials (
  id TEXT PRIMARY KEY,
  path TEXT NOT NULL,
  sha256 TEXT NOT NULL,
  content TEXT NOT NULL,
  captured_at INTEGER NOT NULL, upload_id TEXT REFERENCES uploads(id), upload_part INTEGER,
  UNIQUE(path, sha256)
);

CREATE TABLE message_materials (
  message_seq INTEGER NOT NULL REFERENCES messages(seq),
  material_id TEXT NOT NULL REFERENCES materials(id),
  ordinal INTEGER NOT NULL,
  PRIMARY KEY (message_seq, material_id),
  UNIQUE (message_seq, ordinal)
);

CREATE TABLE task_acceptances (
  task_id TEXT NOT NULL REFERENCES tasks(id),
  revision INTEGER NOT NULL,
  artifact_id TEXT NOT NULL REFERENCES artifacts(id),
  sha256 TEXT NOT NULL,
  criteria TEXT NOT NULL,
  accepted_at INTEGER NOT NULL, publication_id TEXT REFERENCES publications(id),
  PRIMARY KEY(task_id, revision)
);

CREATE TABLE memories (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL CHECK(kind IN ('preference','fact')),
  title TEXT NOT NULL,
  content TEXT NOT NULL,
  pinned INTEGER NOT NULL DEFAULT 0,
  revision INTEGER NOT NULL DEFAULT 1,
  source_message INTEGER REFERENCES messages(seq) ON DELETE SET NULL,
  updated_at INTEGER NOT NULL, source_deleted INTEGER NOT NULL DEFAULT 0,
  UNIQUE(kind,title)
);

CREATE TABLE run_memories (
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  memory_id TEXT NOT NULL,
  revision INTEGER NOT NULL, title TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(run_id,memory_id)
);

CREATE TABLE connections (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  config TEXT NOT NULL,
  enabled INTEGER NOT NULL,
  revision INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL DEFAULT 'untested',
  detail TEXT NOT NULL DEFAULT '',
  protocol TEXT NOT NULL DEFAULT '',
  credential_binding TEXT NOT NULL DEFAULT '',
  checked_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE connection_tools (
  connection_id TEXT NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  definition TEXT NOT NULL,
  allowed INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(connection_id,name)
);

CREATE TABLE connection_calls (
  id TEXT PRIMARY KEY,
  connection_id TEXT NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
  connection_revision INTEGER NOT NULL,
  tool_name TEXT NOT NULL,
  tool_version TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  evidence BLOB
);

CREATE TABLE deleted_submissions (request_key TEXT PRIMARY KEY);

CREATE TABLE uploads (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  sha256 TEXT NOT NULL,
  size INTEGER NOT NULL,
  parts INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE model_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    base_url TEXT NOT NULL,
    model_id TEXT NOT NULL,
    api_key_ref TEXT NOT NULL,
    api_key TEXT NOT NULL DEFAULT '',
    protocol TEXT NOT NULL DEFAULT 'responses' CHECK (protocol IN ('responses', 'chat_completions')),
    revision INTEGER NOT NULL
);

CREATE TABLE run_read_rounds (
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  request_number INTEGER NOT NULL CHECK(request_number BETWEEN 1 AND 8),
  protocol TEXT NOT NULL,
  output TEXT NOT NULL, output_digest TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(run_id,request_number)
);

CREATE TABLE run_read_model_calls (
  run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
  protocol TEXT NOT NULL,
  call_id TEXT NOT NULL,
  output TEXT NOT NULL
);

CREATE TABLE run_read_checkpoints (
  run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
  version INTEGER NOT NULL,
  provider_binding TEXT NOT NULL,
  context_digest TEXT NOT NULL,
  output_budget INTEGER NOT NULL,
  active_budget_ms INTEGER NOT NULL,
  output_used INTEGER NOT NULL DEFAULT 0,
  request_limit INTEGER NOT NULL DEFAULT 0,
  automatic_tools INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE run_reads (
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  position INTEGER NOT NULL DEFAULT 1 CHECK(position BETWEEN 1 AND 8),
  connection_id TEXT REFERENCES connections(id),
  connection_revision INTEGER NOT NULL,
  connection_name TEXT NOT NULL,
  tool_name TEXT NOT NULL,
  tool_title TEXT NOT NULL,
  tool_version TEXT NOT NULL,
  arguments TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'queued',
  detail TEXT NOT NULL DEFAULT '',
  call_id TEXT REFERENCES connection_calls(id),
  received_at INTEGER NOT NULL DEFAULT 0,
  result TEXT NOT NULL DEFAULT '',
  acknowledged_at INTEGER NOT NULL DEFAULT 0,
  model_round INTEGER,
  model_call_id TEXT,
  model_order INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(run_id,position),
  UNIQUE(run_id,model_call_id),
  FOREIGN KEY(run_id,model_round) REFERENCES run_read_rounds(run_id,request_number)
);

CREATE TABLE submission_classifications (
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
  route TEXT NOT NULL DEFAULT ''
);

CREATE TABLE run_finalizations (
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  execution_generation INTEGER NOT NULL,
  status TEXT NOT NULL,
  failure TEXT NOT NULL,
  question TEXT NOT NULL,
  active_ms INTEGER NOT NULL,
  error_code TEXT NOT NULL,
  input_tokens INTEGER,
  output_tokens INTEGER, plan TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (run_id, execution_generation)
);

CREATE TABLE task_revisions (
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  revision INTEGER NOT NULL,
  title TEXT NOT NULL,
  goal TEXT NOT NULL,
  scope_label TEXT NOT NULL,
  acceptance_criteria TEXT NOT NULL,
  required_filename TEXT NOT NULL,
  PRIMARY KEY (task_id, revision)
);

CREATE TABLE assignments (
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  generation INTEGER NOT NULL,
  worker_role TEXT NOT NULL,
  assigned_at INTEGER NOT NULL,
  PRIMARY KEY (task_id, generation)
);

CREATE TABLE run_bindings (
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  generation INTEGER NOT NULL,
  task_id TEXT,
  task_revision INTEGER,
  assignment_generation INTEGER,
  accepted_at INTEGER NOT NULL,
  PRIMARY KEY (run_id, generation),
  FOREIGN KEY (task_id, task_revision) REFERENCES task_revisions(task_id,revision),
  FOREIGN KEY (task_id, assignment_generation) REFERENCES assignments(task_id,generation)
);

CREATE TABLE input_events (
  message_seq INTEGER PRIMARY KEY REFERENCES messages(seq) ON DELETE CASCADE,
  source_id TEXT NOT NULL UNIQUE,
  accepted_at INTEGER NOT NULL
);

CREATE TABLE input_deliveries (
  message_seq INTEGER NOT NULL REFERENCES input_events(message_seq) ON DELETE CASCADE,
  run_id TEXT NOT NULL,
  generation INTEGER NOT NULL,
  PRIMARY KEY (message_seq,run_id,generation),
  FOREIGN KEY (run_id,generation) REFERENCES run_bindings(run_id,generation) ON DELETE CASCADE
);

CREATE TABLE context_manifests (
  id INTEGER PRIMARY KEY,
  run_id TEXT NOT NULL,
  generation INTEGER NOT NULL,
  assembled_at INTEGER NOT NULL,
  manifest TEXT NOT NULL,
  FOREIGN KEY (run_id,generation) REFERENCES run_bindings(run_id,generation) ON DELETE CASCADE
);

CREATE TABLE context_inputs (
  manifest_id INTEGER NOT NULL REFERENCES context_manifests(id) ON DELETE CASCADE,
  message_seq INTEGER NOT NULL REFERENCES input_events(message_seq) ON DELETE CASCADE,
  PRIMARY KEY (manifest_id,message_seq)
);

CREATE TABLE input_dispositions (
  message_seq INTEGER PRIMARY KEY REFERENCES input_events(message_seq) ON DELETE CASCADE,
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  generation INTEGER NOT NULL,
  outcome TEXT NOT NULL CHECK (outcome IN ('completed','input_required','cancelled')),
  recorded_at INTEGER NOT NULL
);

CREATE TABLE task_waits (
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK(kind IN ('input','acceptance','publication')),
  opened_at INTEGER NOT NULL,
  closed_at INTEGER,
  PRIMARY KEY (run_id,kind)
);

CREATE TABLE experience_routes (
  run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
  policy_version INTEGER NOT NULL,
  artifact_id TEXT REFERENCES artifacts(id),
  artifact_digest TEXT,
  acceptance_revision INTEGER,
  decided_at INTEGER NOT NULL
);

CREATE TABLE task_followups (
  request_key TEXT PRIMARY KEY REFERENCES submissions(request_key) ON DELETE CASCADE,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  text TEXT NOT NULL,
  accepted_at INTEGER NOT NULL,
  dispatched_run_id TEXT REFERENCES runs(id),
  cancelled_at INTEGER
);

CREATE TABLE goals (
  id TEXT PRIMARY KEY,
  fingerprint TEXT NOT NULL,
  target TEXT NOT NULL,
  scope_label TEXT NOT NULL,
  acceptance_criteria TEXT NOT NULL,
  interval_ms INTEGER NOT NULL,
  next_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  remaining INTEGER NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('active','exhausted','stopped','expired')),
  revision INTEGER NOT NULL
);

CREATE TABLE goal_tasks (
  goal_id TEXT NOT NULL REFERENCES goals(id),
  scheduled_at INTEGER NOT NULL,
  task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id),
  PRIMARY KEY (goal_id,scheduled_at)
);

CREATE TABLE personal_skills (
  id TEXT PRIMARY KEY,
  scope_label TEXT NOT NULL UNIQUE,
  revision INTEGER NOT NULL,
  active_version INTEGER
);

CREATE TABLE personal_skill_versions (
  skill_id TEXT NOT NULL REFERENCES personal_skills(id),
  version INTEGER NOT NULL,
  base_revision INTEGER NOT NULL,
  instructions TEXT NOT NULL,
  sha256 TEXT NOT NULL,
  approved_at INTEGER,
  PRIMARY KEY(skill_id,version)
);

CREATE TABLE run_skills (
  run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
  skill_id TEXT NOT NULL,
  version INTEGER NOT NULL,
  FOREIGN KEY(skill_id,version) REFERENCES personal_skill_versions(skill_id,version)
);

CREATE TABLE workflow_children (
  task_id TEXT PRIMARY KEY REFERENCES tasks(id),
  parent_id TEXT NOT NULL REFERENCES tasks(id),
  position INTEGER NOT NULL,
  step_key TEXT NOT NULL,
  optional INTEGER NOT NULL CHECK(optional IN (0,1)),
  UNIQUE(parent_id, position),
  UNIQUE(parent_id, step_key),
  CHECK(task_id<>parent_id)
);

CREATE TABLE workflow_dependencies (
  task_id TEXT NOT NULL REFERENCES workflow_children(task_id),
  prerequisite_id TEXT NOT NULL REFERENCES workflow_children(task_id),
  PRIMARY KEY(task_id,prerequisite_id),
  CHECK(task_id<>prerequisite_id)
);

CREATE TABLE workflow_run_inputs (
  run_id TEXT NOT NULL,
  generation INTEGER NOT NULL,
  task_id TEXT NOT NULL REFERENCES workflow_children(task_id),
  step_key TEXT NOT NULL,
  artifact_id TEXT REFERENCES artifacts(id),
  acceptance_revision INTEGER,
  sha256 TEXT,
  PRIMARY KEY(run_id,generation,task_id),
  FOREIGN KEY(run_id,generation) REFERENCES run_bindings(run_id,generation) ON DELETE CASCADE
);

CREATE TABLE worker_plans (
  parent_id TEXT PRIMARY KEY REFERENCES tasks(id),
  planning_run_id TEXT NOT NULL UNIQUE,
  generation INTEGER NOT NULL,
  proposal TEXT NOT NULL,
  FOREIGN KEY(planning_run_id,generation) REFERENCES run_bindings(run_id,generation)
);

CREATE TABLE worker_plan_budgets (
  run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
  active_ms INTEGER NOT NULL CHECK(active_ms>0),
  output_tokens INTEGER NOT NULL CHECK(output_tokens>=16)
);

CREATE TABLE router_intents (
  id TEXT PRIMARY KEY,
  source_run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  kind TEXT NOT NULL,
  fragment TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('applied','queued','clarify','duplicate')),
  task_id TEXT REFERENCES tasks(id),
  after_task_id TEXT REFERENCES tasks(id),
  target_run_id TEXT REFERENCES runs(id) ON DELETE SET NULL,
  dispatch_key TEXT,
  detail TEXT NOT NULL,
  duplicate_of TEXT,
  resolved_by TEXT,
  UNIQUE(source_run_id, position)
);

CREATE TABLE run_queue_order (
  before_run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  after_run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  PRIMARY KEY(after_run_id,before_run_id),
  CHECK(before_run_id<>after_run_id)
);

CREATE INDEX run_queue_order_before ON run_queue_order(before_run_id);

CREATE TRIGGER run_queue_order_release AFTER UPDATE OF status ON runs
WHEN OLD.status='queued' AND NEW.status<>'queued'
BEGIN
  DELETE FROM run_queue_order WHERE before_run_id=NEW.id OR after_run_id=NEW.id;
END;

CREATE TABLE experience_candidates (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  artifact_id TEXT REFERENCES artifacts(id) ON DELETE CASCADE,
  acceptance_revision INTEGER,
  message_seq INTEGER REFERENCES messages(seq) ON DELETE CASCADE,
  source_digest TEXT NOT NULL,
  counter_artifact_id TEXT REFERENCES artifacts(id) ON DELETE CASCADE,
  counter_digest TEXT NOT NULL DEFAULT '',
  scope_label TEXT NOT NULL,
  goal TEXT NOT NULL,
  criteria TEXT NOT NULL DEFAULT '',
  format_cues TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','approved','rejected')),
  revision INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  CHECK((artifact_id IS NOT NULL AND acceptance_revision IS NOT NULL AND message_seq IS NULL) OR
        (artifact_id IS NULL AND acceptance_revision IS NULL AND message_seq IS NOT NULL))
);

CREATE TABLE skill_proposals (
  id TEXT PRIMARY KEY,
  skill_id TEXT NOT NULL,
  version INTEGER NOT NULL,
  base_version INTEGER,
  goal TEXT NOT NULL,
  guidance TEXT NOT NULL,
  report_digest TEXT NOT NULL,
  snapshot_digest TEXT NOT NULL,
  model TEXT NOT NULL,
  evidence TEXT NOT NULL,
  sources TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE(skill_id, version),
  UNIQUE(report_digest, skill_id, goal),
  FOREIGN KEY(skill_id,version) REFERENCES personal_skill_versions(skill_id,version)
);

CREATE TABLE skill_proposal_replays (
  proposal_id TEXT PRIMARY KEY REFERENCES skill_proposals(id) ON DELETE CASCADE,
  attempt_id TEXT NOT NULL,
  report TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX active_conversation ON runs(conversation_id) WHERE status IN ('queued','running');

CREATE UNIQUE INDEX active_task ON runs(task_id) WHERE task_id IS NOT NULL AND status IN ('queued','running');

CREATE INDEX run_queue ON runs(status,created_at);

CREATE INDEX events_by_run ON run_events(run_id,seq);

CREATE INDEX tasks_by_status ON tasks(status,updated_at DESC);

CREATE UNIQUE INDEX publication_open ON publications(task_id) WHERE status IN ('pending','dispatched','unknown');

CREATE INDEX context_manifests_run ON context_manifests(run_id,generation);

CREATE INDEX context_inputs_message ON context_inputs(message_seq);

CREATE INDEX task_followups_pending ON task_followups(task_id,accepted_at)
WHERE dispatched_run_id IS NULL AND cancelled_at IS NULL;

CREATE INDEX goals_due ON goals(next_at) WHERE status='active';

CREATE INDEX workflow_run_inputs_source ON workflow_run_inputs(task_id);

CREATE INDEX router_intents_pending ON router_intents(source_run_id)
WHERE status='clarify' AND resolved_by IS NULL;

CREATE UNIQUE INDEX experience_by_acceptance ON experience_candidates(task_id,acceptance_revision)
WHERE artifact_id IS NOT NULL;

CREATE UNIQUE INDEX experience_by_correction ON experience_candidates(message_seq)
WHERE message_seq IS NOT NULL;

CREATE VIEW workflow_prerequisites AS
SELECT parent_id AS task_id,task_id AS prerequisite_id,optional FROM workflow_children
UNION ALL
SELECT task_id,prerequisite_id,0 FROM workflow_dependencies;

CREATE VIEW workflow_current_outputs AS
SELECT t.id AS task_id,a.id AS artifact_id,x.revision,x.sha256,r.id AS run_id,r.execution_generation AS generation
FROM tasks t
JOIN task_acceptances x ON x.task_id=t.id AND x.revision=(SELECT max(revision) FROM task_acceptances WHERE task_id=t.id)
JOIN artifacts a ON a.id=x.artifact_id AND a.task_id=t.id AND a.sha256=x.sha256
JOIN runs r ON r.id=a.run_id
WHERE t.status='completed';

CREATE VIEW workflow_accepted_outputs AS
WITH RECURSIVE stale(task_id) AS (
  SELECT o.task_id FROM workflow_current_outputs o
  JOIN workflow_run_inputs i ON i.run_id=o.run_id AND i.generation=o.generation
  JOIN tasks t ON t.id=i.task_id
  LEFT JOIN workflow_current_outputs a ON a.task_id=i.task_id
  WHERE (i.artifact_id IS NULL AND t.status<>'cancelled')
     OR (i.artifact_id IS NOT NULL AND (
       a.artifact_id IS NOT i.artifact_id OR a.revision IS NOT i.acceptance_revision OR a.sha256 IS NOT i.sha256))
  UNION
  SELECT o.task_id FROM stale s
  JOIN workflow_run_inputs i ON i.task_id=s.task_id AND i.artifact_id IS NOT NULL
  JOIN workflow_current_outputs o ON o.run_id=i.run_id AND o.generation=i.generation
)
SELECT task_id,artifact_id,revision,sha256 FROM workflow_current_outputs
WHERE task_id NOT IN (SELECT task_id FROM stale);

CREATE VIEW workflow_blocked_tasks AS
SELECT DISTINCT p.task_id FROM workflow_prerequisites p
JOIN tasks t ON t.id=p.prerequisite_id
LEFT JOIN workflow_accepted_outputs a ON a.task_id=p.prerequisite_id
WHERE a.artifact_id IS NULL AND NOT (p.optional=1 AND t.status='cancelled')
UNION
SELECT c.task_id FROM workflow_children c
JOIN tasks t ON t.id=c.parent_id
WHERE t.status='cancelled' OR t.status_reason IN ('paused','pause_requested','stop_requested');

CREATE TRIGGER task_state_created AFTER INSERT ON tasks
WHEN NEW.state_changed_at=0
BEGIN
    UPDATE tasks SET state_changed_at=NEW.updated_at WHERE id=NEW.id;
END;

CREATE TRIGGER task_state_changed AFTER UPDATE OF status,status_reason ON tasks
WHEN NEW.status<>OLD.status OR NEW.status_reason<>OLD.status_reason
BEGIN
    UPDATE tasks SET state_changed_at=NEW.updated_at WHERE id=NEW.id;
END;

CREATE TRIGGER task_contract_created AFTER INSERT ON tasks
BEGIN
  INSERT INTO task_revisions VALUES
    (NEW.id,NEW.revision,NEW.title,NEW.goal,NEW.scope_label,NEW.acceptance_criteria,NEW.required_filename);
END;

CREATE TRIGGER task_contract_edited AFTER UPDATE OF title,goal,scope_label,acceptance_criteria,required_filename ON tasks
WHEN (
  NEW.title<>OLD.title OR NEW.goal<>OLD.goal OR NEW.scope_label<>OLD.scope_label
  OR NEW.acceptance_criteria<>OLD.acceptance_criteria OR NEW.required_filename<>OLD.required_filename)
BEGIN
  INSERT INTO task_revisions VALUES
    (NEW.id,NEW.revision,NEW.title,NEW.goal,NEW.scope_label,NEW.acceptance_criteria,NEW.required_filename);
END;

CREATE TRIGGER task_wait_opened AFTER UPDATE OF status,status_reason ON tasks
WHEN NEW.status='waiting_user'
AND (NEW.status<>OLD.status OR NEW.status_reason<>OLD.status_reason)
AND NEW.status_reason IN ('input_required','acceptance_required','delivery_required')
BEGIN
  UPDATE task_waits SET closed_at=NEW.updated_at
  WHERE closed_at IS NULL AND run_id IN (SELECT id FROM runs WHERE task_id=NEW.id)
  AND kind<>CASE NEW.status_reason WHEN 'input_required' THEN 'input'
    WHEN 'delivery_required' THEN 'publication' ELSE 'acceptance' END;
  INSERT INTO task_waits(run_id,kind,opened_at)
  SELECT id,CASE NEW.status_reason WHEN 'input_required' THEN 'input'
    WHEN 'delivery_required' THEN 'publication' ELSE 'acceptance' END,NEW.updated_at
  FROM runs WHERE task_id=NEW.id ORDER BY rowid DESC LIMIT 1
  ON CONFLICT(run_id,kind) DO UPDATE SET
    opened_at=CASE WHEN closed_at IS NULL THEN opened_at ELSE excluded.opened_at END,
    closed_at=NULL;
END;

CREATE TRIGGER task_wait_closed AFTER UPDATE OF status ON tasks
WHEN OLD.status='waiting_user' AND NEW.status<>'waiting_user'
BEGIN
  UPDATE task_waits SET closed_at=NEW.updated_at
  WHERE closed_at IS NULL AND run_id IN (SELECT id FROM runs WHERE task_id=NEW.id);
END;
