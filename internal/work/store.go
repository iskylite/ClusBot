package work

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

const databaseSchemaVersion = 43

var (
	ErrConflict        = errors.New("请求标识已用于其他内容")
	ErrActive          = errors.New("当前对话还有一次执行正在处理")
	ErrState           = errors.New("当前状态不允许此操作")
	ErrDeliveryPending = errors.New("请先将当前成果保存到指定的交付文件")
	ErrDeliveryChanged = errors.New("交付文件缺失或内容已变更。恢复原内容，或在补充要求中改用新的文件名")
)

type Store struct {
	db               *sql.DB
	workspace        *workspace
	materials        *workspace
	uploads          *os.Root
	publicationMu    sync.Mutex
	connectionKey    []byte
	finalizationMu   sync.Mutex
	finalizations    map[runFinalizationKey]*runFinalization
	admissionMu      sync.Mutex
	admissions       map[string]*admissionFlight
	runtimeMu        sync.Mutex
	modelConcurrency int
	modelActive      int
	modelAvailable   chan struct{}
	admissionLimits  RunLimits
}

type Task struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	Title          string `json:"title"`
	Goal           string `json:"goal"`
	Scope          string `json:"scope_label"`
	Criteria       string `json:"acceptance_criteria,omitempty"`
	RequiredFile   string `json:"required_filename,omitempty"`
	Status         string `json:"status"`
	StatusReason   string `json:"status_reason"`
	Reason         string `json:"reason"`
	StateChangedAt int64  `json:"state_changed_at"`

	Revision            int    `json:"revision"`
	UpdatedAt           int64  `json:"updated_at"`
	CompletedAt         *int64 `json:"completed_at,omitempty"`
	RequirementsChanged bool   `json:"requirements_changed,omitempty"`
	Priority            string `json:"priority"`
}

type Run struct {
	ID              string          `json:"id"`
	ConversationID  string          `json:"conversation_id"`
	TaskID          string          `json:"task_id,omitempty"`
	Status          string          `json:"status"`
	Response        string          `json:"response"`
	Error           string          `json:"error,omitempty"`
	ErrorCode       string          `json:"error_code,omitempty"`
	Usage           *ExecutionUsage `json:"usage,omitempty"`
	Generation      int             `json:"execution_generation"`
	FollowupQueued  bool            `json:"followup_queued,omitempty"`
	RouterIntents   *[]RouterIntent `json:"router_intents,omitempty"`
	admissionOutput int
}

type Message struct {
	ID               int64          `json:"id,omitempty"`
	RunID            string         `json:"run_id,omitempty"`
	Role             string         `json:"role"`
	Text             string         `json:"text"`
	Materials        []Material     `json:"materials,omitempty"`
	ReplaceMaterials bool           `json:"replace_materials,omitempty"`
	MemoryRefs       []MemoryRef    `json:"memory_refs,omitempty"`
	Incomplete       bool           `json:"incomplete,omitempty"`
	RequiredInput    bool           `json:"-"`
	Memories         []Memory       `json:"-"`
	Experience       *ExperienceRef `json:"-"`
	Skill            *SkillVersion  `json:"-"`
	Read             *TaskRead      `json:"-"`
	Reads            []TaskRead     `json:"-"`
}

type Conversation struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Archived bool   `json:"archived"`
	Revision int    `json:"revision"`
}

type Artifact struct {
	ID         string      `json:"id"`
	TaskID     string      `json:"task_id"`
	Name       string      `json:"name"`
	Content    string      `json:"content,omitempty"`
	Digest     string      `json:"sha256"`
	CreatedAt  int64       `json:"created_at"`
	RunID      string      `json:"run_id,omitempty"`
	TaskTitle  string      `json:"task_title,omitempty"`
	Scope      string      `json:"scope_label,omitempty"`
	AcceptedAt int64       `json:"accepted_at,omitempty"`
	Acceptance *Acceptance `json:"acceptance,omitempty"`
}

type Event struct {
	Seq  int64
	Kind string
	Data string
}

type Detail struct {
	Task             *Task           `json:"task,omitempty"`
	Conversation     Conversation    `json:"conversation"`
	Messages         []Message       `json:"messages"`
	Run              *Run            `json:"run,omitempty"`
	Artifact         *Artifact       `json:"artifact,omitempty"`
	ArtifactVersions []Artifact      `json:"artifact_versions,omitempty"`
	Publication      *Publication    `json:"publication,omitempty"`
	Acceptance       *Acceptance     `json:"acceptance,omitempty"`
	TaskUsage        *ExecutionUsage `json:"task_usage,omitempty"`
	MemoryRefs       []MemoryRef     `json:"memory_refs,omitempty"`
	Read             *TaskRead       `json:"read,omitempty"`
	Reads            []TaskRead      `json:"reads,omitempty"`
	ReadCheckpoint   string          `json:"read_checkpoint,omitempty"`
	UnknownReadRunID string          `json:"unknown_read_run_id,omitempty"`
	Workflow         *Workflow       `json:"workflow,omitempty"`
	WorkflowParentID string          `json:"workflow_parent_id,omitempty"`
}

type Acceptance struct {
	ArtifactID    string `json:"artifact_id"`
	Digest        string `json:"sha256"`
	Criteria      string `json:"criteria"`
	AcceptedAt    int64  `json:"accepted_at"`
	PublicationID string `json:"publication_id,omitempty"`
	Filename      string `json:"filename,omitempty"`
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;"); err != nil {
		db.Close()
		return nil, err
	}
	if err = initializeDatabase(db); err == nil {
		_, err = db.Exec("PRAGMA journal_mode=WAL")
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, connectionKey: randBytes()}, nil
}

func initializeDatabase(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 0 {
		return requireDatabaseVersion(version)
	}
	var empty bool
	if err := tx.QueryRow("SELECT NOT EXISTS(SELECT 1 FROM sqlite_schema)").Scan(&empty); err != nil {
		return err
	}
	if !empty {
		return requireDatabaseVersion(version)
	}
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", databaseSchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func requireDatabaseVersion(version int) error {
	if version != databaseSchemaVersion {
		return fmt.Errorf("数据库版本为 %d，当前需要 %d。请使用新的数据目录（--data-dir）", version, databaseSchemaVersion)
	}
	return nil
}

func (s *Store) SetConnectionCredentialKey(key string) {
	s.connectionKey = []byte(key)
}
func (s *Store) Close() error {
	var err error
	if s.workspace != nil {
		err = errors.Join(s.workspace.root.Close(), s.materials.root.Close())
	}
	if s.uploads != nil {
		err = errors.Join(err, s.uploads.Close())
	}
	return errors.Join(err, s.db.Close())
}

func id() string { return hex.EncodeToString(randBytes()) }

func randBytes() []byte {
	b := make([]byte, 16)
	rand.Read(b)
	return b
}

func digest(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }
func now() int64                { return time.Now().UnixMilli() }

func title(text string) string {
	line := strings.SplitN(text, "\n", 2)[0]
	runes := []rune(line)
	if len(runes) > 30 {
		return string(runes[:30]) + "…"
	}
	return line
}

func taskTitle(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.TrimSpace(line)
	for i, punctuation := range line {
		if i == 0 || !strings.ContainsRune("。！？.!?", punctuation) {
			continue
		}
		next, size := utf8.DecodeRuneInString(line[i+utf8.RuneLen(punctuation):])
		if size == 0 || strings.ContainsRune("”’」』）】》", next) || (punctuation <= unicode.MaxASCII && !unicode.IsSpace(next)) {
			continue
		}
		// A leading list number is not a sentence. ASCII punctuation inside
		// filenames, version numbers and URLs is handled by the space check.
		if head := strings.TrimSpace(line[:i]); strings.ContainsFunc(head, unicode.IsLetter) {
			return title(head)
		}
	}
	return title(line)
}

func addEvent(tx *sql.Tx, runID, kind string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO run_events(run_id,kind,data) VALUES(?,?,?)", runID, kind, string(raw))
	return err
}

const runColumns = "id,conversation_id,coalesce(task_id,''),status,response,error,error_code,usage_tracked,model_requests,reported_requests,active_ms,time_partial,input_tokens,output_tokens,execution_generation,admission_output_used"
const taskColumns = "id,conversation_id,title,goal,scope_label,acceptance_criteria,required_filename,status,status_reason,reason,state_changed_at,revision,updated_at,completed_at,requirements_changed,priority"

type scanner interface{ Scan(...any) error }

func scanRun(row scanner) (Run, error) {
	var r Run
	var tracked bool
	var usage ExecutionUsage
	var reported int
	err := row.Scan(&r.ID, &r.ConversationID, &r.TaskID, &r.Status, &r.Response, &r.Error, &r.ErrorCode,
		&tracked, &usage.ModelRequests, &reported, &usage.ActiveMS, &usage.TimePartial, &usage.InputTokens, &usage.OutputTokens, &r.Generation, &r.admissionOutput)
	if tracked {
		usage.UnreportedRequests = usage.ModelRequests - reported
		r.Usage = &usage
	}
	return r, err
}

func scanTask(row scanner) (Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.ConversationID, &t.Title, &t.Goal, &t.Scope, &t.Criteria, &t.RequiredFile, &t.Status, &t.StatusReason,
		&t.Reason, &t.StateChangedAt, &t.Revision, &t.UpdatedAt, &t.CompletedAt, &t.RequirementsChanged, &t.Priority)
	return t, err
}

func (s *Store) Submit(ctx context.Context, requestKey, text, scope, conversationID string, task bool, materialIDs ...string) (Run, error) {
	return s.SubmitWithDelivery(ctx, requestKey, text, scope, conversationID, task, Delivery{}, materialIDs...)
}

type Delivery struct {
	Criteria         *string
	Filename         *string
	Revision         int
	Read             *TaskReadRequest
	Reads            []TaskReadRequest
	ReplaceMaterials bool
}

func (s *Store) SubmitWithDelivery(ctx context.Context, requestKey, text, scope, conversationID string, task bool, delivery Delivery, materialIDs ...string) (Run, error) {
	return s.submitWithDelivery(ctx, requestKey, text, scope, conversationID, task, delivery, nil, materialIDs)
}

func (s *Store) SubmitChat(ctx context.Context, requestKey, text, scope, conversationID string, delivery Delivery, isDelegation func(context.Context, string, []Message) bool, materialIDs ...string) (Run, error) {
	var classifier DelegationClassifier
	if isDelegation != nil {
		classifier = func(ctx context.Context, text string, history []Message, _ int) (DelegationResult, error) {
			return DelegationResult{Task: isDelegation(ctx, text, history)}, nil
		}
	}
	return s.SubmitClassifiedChat(ctx, requestKey, text, scope, conversationID, delivery, classifier, materialIDs...)
}

func (s *Store) SubmitClassifiedChat(ctx context.Context, requestKey, text, scope, conversationID string, delivery Delivery, classify DelegationClassifier, materialIDs ...string) (Run, error) {
	return s.submitWithDelivery(ctx, requestKey, text, scope, conversationID, false, delivery, classify, materialIDs)
}

func submissionFingerprint(text, scope, conversationID string, task bool) string {
	return digest(fmt.Sprintf("%q %q %q %t", text, scope, conversationID, task))
}

func (s *Store) submitWithDelivery(ctx context.Context, requestKey, text, scope, conversationID string, task bool, delivery Delivery, classify DelegationClassifier, materialIDs []string) (Run, error) {
	if len(delivery.Reads) > MaxTaskReads || (delivery.Read != nil && len(delivery.Reads) > 0) {
		return Run{}, ErrReadSelection
	}
	if delivery.Filename != nil && *delivery.Filename != "" && !validFilename(*delivery.Filename) {
		return Run{}, ErrFilename
	}
	if delivery.ReplaceMaterials && conversationID == "" {
		return Run{}, ErrState
	}
	// Preserve the request's original identity when a chat submission creates a Task.
	fingerprint := submissionFingerprint(text, scope, conversationID, task)
	if len(materialIDs) > 0 {
		fingerprint = digest(fmt.Sprintf("%s %q", fingerprint, materialIDs))
	}
	if delivery.ReplaceMaterials {
		fingerprint = digest(fingerprint + " replace materials")
	}
	if delivery.Criteria != nil {
		fingerprint = digest(fmt.Sprintf("%s %q %d", fingerprint, *delivery.Criteria, delivery.Revision))
	}
	if delivery.Filename != nil {
		fingerprint = digest(fmt.Sprintf("%s filename %q %d", fingerprint, *delivery.Filename, delivery.Revision))
	}
	if delivery.Read != nil {
		payload, err := json.Marshal(delivery.Read)
		if err != nil {
			return Run{}, err
		}
		fingerprint = digest(fingerprint + " read " + string(payload))
	}
	if len(delivery.Reads) > 0 {
		payload, err := json.Marshal(delivery.Reads)
		if err != nil {
			return Run{}, err
		}
		fingerprint = digest(fingerprint + " reads " + string(payload))
	}
	changesDelivery := delivery.Criteria != nil || delivery.Filename != nil
	var classification *submissionClassification
	if classify != nil && !changesDelivery && delivery.Read == nil && len(delivery.Reads) == 0 && !delivery.ReplaceMaterials {
		previous, history, err := s.chatSubmissionContext(ctx, requestKey, fingerprint, conversationID)
		if err != nil || previous.ID != "" {
			return previous, err
		}
		if history != nil {
			decision, err := s.classifySubmission(ctx, requestKey, fingerprint, conversationID, text, history, classify)
			if err != nil {
				return Run{}, err
			}
			classification, task = &decision, decision.Task
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	previous, err := previousSubmission(tx, requestKey, fingerprint)
	if err != nil || previous.ID != "" {
		return previous, err
	}

	if classification != nil && classification.Route != nil &&
		(classification.Route.Kind == "batch" || classification.Route.Kind == "clarify" ||
			usesRouterReceipt(classification.Route.Kind) || classification.Route.ResolveID != "") {
		run, err := submitRouterBatch(ctx, tx, requestKey, fingerprint, text, scope, conversationID, classification, materialIDs)
		if err != nil {
			return Run{}, err
		}
		return run, tx.Commit()
	}
	run, err := submitWorkTx(ctx, tx, requestKey, fingerprint, text, scope, conversationID, task, delivery, classification, materialIDs)
	if err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}

func submitWorkTx(ctx context.Context, tx *sql.Tx, requestKey, fingerprint, text, scope, conversationID string, task bool, delivery Delivery, classification *submissionClassification, materialIDs []string) (Run, error) {
	var err error
	reads := delivery.Reads
	if delivery.Read != nil {
		reads = []TaskReadRequest{*delivery.Read}
	}
	changesDelivery := delivery.Criteria != nil || delivery.Filename != nil

	goal := text
	reply := ""
	if classification != nil && classification.Route != nil {
		route := classification.Route
		switch route.Kind {
		case "new_task":
			goal = route.Goal
			delivery.Criteria = &route.Criteria
		case "follow_up", "progress", "pause", "stop", "resume":
			target, err := scanTask(tx.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE id=?", route.TaskID))
			if err != nil {
				return Run{}, err
			}
			switch route.Kind {
			case "follow_up":
				if target.Revision != route.Revision {
					return Run{}, ErrTaskRevision
				}
				if err := ensureGoalTaskContinuable(tx, target.ID); err != nil {
					return Run{}, err
				}
				if err := checkConversationSubmission(tx, target.ConversationID); errors.Is(err, ErrActive) && len(materialIDs) == 0 {
					pending, err := deferTaskFollowup(tx, target, requestKey, fingerprint, text)
					if err != nil {
						return Run{}, err
					}
					return pending, nil
				} else if err != nil {
					return Run{}, err
				}
				conversationID = target.ConversationID
			case "progress":
				reply = target.Title + "：" + target.Reason
			case "resume":
				if _, err := resumeTaskTx(tx, target.ID, digest("router-resume:"+requestKey), route.Revision, "resume"); err != nil {
					return Run{}, err
				}
				reply = target.Title + "：已安排继续"
			case "pause", "stop":
				if err := controlTask(tx, target.ID, route.Kind, route.Revision); err != nil {
					return Run{}, err
				}
				target, err = scanTask(tx.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE id=?", target.ID))
				if err != nil {
					return Run{}, err
				}
				reply = target.Title + "：" + target.Reason
			}
		case "clarify":
			reply = route.Question
		}
	}
	r := Run{ID: id(), ConversationID: conversationID, Status: "queued"}
	if reply != "" {
		r.Status, r.Response = "completed", reply
	}
	var taskID any
	reuseRead := true
	if conversationID == "" {
		r.ConversationID = id()
		if _, err = tx.Exec("INSERT INTO conversations(id,title,created_at) VALUES(?,?,?)", r.ConversationID, title(text), now()); err != nil {
			return Run{}, err
		}
		if task {
			r.TaskID = id()
			taskID = r.TaskID
			if err = insertTask(tx, r, goal, scope, delivery); err != nil {
				return Run{}, err
			}
		}
	} else {
		if err = checkConversationSubmission(tx, conversationID); err != nil {
			return Run{}, err
		}
		existing, findErr := scanTask(tx.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE conversation_id=?", conversationID))
		if findErr == nil {
			if err = ensureGoalTaskContinuable(tx, existing.ID); err != nil {
				return Run{}, err
			}
			if err = ensureNoPublication(tx, existing.ID); err != nil {
				return Run{}, err
			}
			if err = ensureReadConfirmed(tx, existing.ID); err != nil {
				return Run{}, err
			}
			if existing.Status == "cancelled" {
				return Run{}, ErrState
			}
			if changesDelivery && delivery.Revision != existing.Revision {
				return Run{}, ErrState
			}
			r.TaskID = existing.ID
			taskID = r.TaskID
			reuseRead = !existing.RequirementsChanged
			if _, err = tx.Exec(`UPDATE tasks SET acceptance_criteria=coalesce(?,acceptance_criteria),required_filename=coalesce(?,required_filename),
status='active',status_reason='queued',reason='补充要求已受理',requirements_changed=0,completed_at=NULL,revision=revision+1,updated_at=? WHERE id=?`, delivery.Criteria, delivery.Filename, now(), r.TaskID); err != nil {
				return Run{}, err
			}
		} else if !errors.Is(findErr, sql.ErrNoRows) {
			return Run{}, findErr
		} else if task && !delivery.ReplaceMaterials {
			r.TaskID = id()
			taskID = r.TaskID
			if err = insertTask(tx, r, goal, scope, delivery); err != nil {
				return Run{}, err
			}
		}
	}
	if (changesDelivery || len(reads) > 0 || delivery.ReplaceMaterials) && r.TaskID == "" {
		return Run{}, ErrState
	}
	if _, err = tx.Exec("INSERT INTO runs(id,conversation_id,task_id,status,created_at,response,usage_tracked) VALUES(?,?,?,?,?,?,?)", r.ID, r.ConversationID, taskID, r.Status, now(), reply, reply != ""); err != nil {
		return Run{}, err
	}
	if len(reads) > 0 {
		for i, read := range reads {
			if err = recordTaskRead(ctx, tx, r, i+1, read); err != nil {
				return Run{}, err
			}
		}
	} else if r.TaskID != "" && reuseRead {
		if err = reuseTaskRead(tx, r.TaskID, r.ID, false); err != nil {
			return Run{}, err
		}
	}
	res, err := tx.Exec("INSERT INTO messages(conversation_id,run_id,role,text,replace_materials) VALUES(?,?,'user',?,?)",
		r.ConversationID, r.ID, text, delivery.ReplaceMaterials)
	if err != nil {
		return Run{}, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return Run{}, err
	}
	if err = recordInputEvent(tx, seq, requestKey); err != nil {
		return Run{}, err
	}
	if reply != "" {
		if _, err = tx.Exec("INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'assistant',?)", r.ConversationID, r.ID, reply); err != nil {
			return Run{}, err
		}
		if _, err = tx.Exec("INSERT INTO input_dispositions VALUES(?,?,0,'completed',?)", seq, r.ID, now()); err != nil {
			return Run{}, err
		}
	}
	if len(materialIDs) > 0 {
		if err = attachMaterials(tx, seq, materialIDs); err != nil {
			return Run{}, err
		}
	}
	if _, err = tx.Exec("INSERT INTO submissions VALUES(?,?,?)", requestKey, fingerprint, r.ID); err != nil {
		return Run{}, err
	}
	if classification != nil {
		if err = attachClassification(tx, r.ID, requestKey, *classification); err != nil {
			return Run{}, err
		}
		if classification.Requests > 0 {
			r, err = scanRun(tx.QueryRow("SELECT "+runColumns+" FROM runs WHERE id=?", r.ID))
			if err != nil {
				return Run{}, err
			}
		}
	}
	if err = addEvent(tx, r.ID, "run.state_changed", map[string]string{"status": r.Status}); err != nil {
		return Run{}, err
	}
	return r, nil
}

func previousSubmission(tx *sql.Tx, requestKey, fingerprint string) (Run, error) {
	var previous, previousRun string
	err := tx.QueryRow("SELECT fingerprint,run_id FROM submissions WHERE request_key=?", requestKey).Scan(&previous, &previousRun)
	if err == nil {
		if previous != fingerprint {
			return Run{}, ErrConflict
		}
		var deferred bool
		var dispatched sql.NullString
		err = tx.QueryRow("SELECT dispatched_run_id,cancelled_at IS NULL FROM task_followups WHERE request_key=?", requestKey).Scan(&dispatched, &deferred)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Run{}, err
		}
		if dispatched.Valid {
			previousRun = dispatched.String
		}
		run, err := scanRun(tx.QueryRow("SELECT "+runColumns+" FROM runs WHERE id=?", previousRun))
		run.FollowupQueued = deferred && !dispatched.Valid
		if err != nil {
			return run, err
		}
		run.RouterIntents, err = routerIntentReceipt(tx, run.ID)
		return run, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Run{}, err
	}
	var classifiedFingerprint string
	err = tx.QueryRow("SELECT fingerprint FROM submission_classifications WHERE request_key=?", requestKey).Scan(&classifiedFingerprint)
	if err == nil && classifiedFingerprint != fingerprint {
		return Run{}, ErrConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Run{}, err
	}
	var deleted bool
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM deleted_submissions WHERE request_key=?)", requestKey).Scan(&deleted); err != nil {
		return Run{}, err
	}
	if deleted {
		return Run{}, ErrConflict
	}
	return Run{}, nil
}

func checkConversationSubmission(tx *sql.Tx, id string) error {
	var archived, active bool
	err := tx.QueryRow(`SELECT archived, EXISTS(SELECT 1 FROM runs WHERE conversation_id=conversations.id
AND status IN ('queued','running')) FROM conversations WHERE id=?`, id).Scan(&archived, &active)
	if err != nil {
		return err
	}
	if archived {
		return ErrConversationArchived
	}
	if active {
		return ErrActive
	}
	return nil
}

func insertTask(tx *sql.Tx, run Run, text, scope string, delivery Delivery) error {
	_, err := tx.Exec(`INSERT INTO tasks(id,conversation_id,title,goal,scope_label,acceptance_criteria,required_filename,status,status_reason,reason,updated_at)
VALUES(?,?,?,?,?,coalesce(?,''),coalesce(?,''),'active','queued','已受理',?)`, run.TaskID, run.ConversationID, taskTitle(text), text, scope, delivery.Criteria, delivery.Filename, now())
	return err
}

func (s *Store) Conversations(archived bool) ([]Conversation, error) {
	rows, err := s.db.Query("SELECT "+conversationColumns+" FROM conversations WHERE archived=? AND NOT EXISTS(SELECT 1 FROM tasks WHERE conversation_id=conversations.id) ORDER BY created_at DESC LIMIT 100", archived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Conversation{}
	for rows.Next() {
		item, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

type rowsQueryer interface {
	Query(string, ...any) (*sql.Rows, error)
}

func messages(q rowsQueryer, conversationID string, forModel bool) ([]Message, error) {
	return queryMessages(q, !forModel, `SELECT seq,role,text,replace_materials,run_id FROM messages WHERE conversation_id=?
AND (NOT ? OR NOT EXISTS(SELECT 1 FROM runs WHERE id=messages.run_id AND memory_command=1))
ORDER BY seq DESC LIMIT 50`, conversationID, forModel)
}

func queryMessages(q rowsQueryer, withMemoryRefs bool, selection string, args ...any) ([]Message, error) {
	rows, err := q.Query(`SELECT msg.seq,msg.role,msg.text,msg.replace_materials,msg.run_id,
		(SELECT json_group_array(json_object('id',material.id,'path',material.path,'sha256',material.sha256,
			'size',length(CAST(material.content AS BLOB)),'captured_at',material.captured_at,'upload',
			CASE WHEN upload.id IS NULL THEN NULL ELSE json_object('id',upload.id,'name',upload.name,'sha256',upload.sha256,
				'bytes',upload.size,'part',material.upload_part,'parts',upload.parts) END))
		FROM (SELECT material_id FROM message_materials WHERE message_seq=msg.seq ORDER BY ordinal) AS refs
		JOIN materials AS material ON material.id=refs.material_id LEFT JOIN uploads AS upload ON upload.id=material.upload_id),
		CASE WHEN ? AND msg.role='assistant' THEN (`+memoryRefsJSON+`msg.run_id) ELSE '[]' END,
		msg.role='assistant' AND length(msg.text)>0 AND EXISTS(SELECT 1 FROM runs r WHERE r.id=msg.run_id AND (r.control='stop' OR r.error_code IN ('stop','stopped')))
		FROM (`+selection+`) AS msg ORDER BY msg.seq`, append([]any{withMemoryRefs}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Message{}
	for rows.Next() {
		var item Message
		var refs, memories, runID string
		if err = rows.Scan(&item.ID, &item.Role, &item.Text, &item.ReplaceMaterials, &runID, &refs, &memories, &item.Incomplete); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(refs), &item.Materials); err != nil {
			return nil, err
		}
		if withMemoryRefs {
			item.RunID = runID
		}
		if withMemoryRefs && item.Role == "assistant" {
			err = json.Unmarshal([]byte(memories), &item.MemoryRefs)
			if err != nil {
				return nil, err
			}
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) Detail(key string, isTask bool) (Detail, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Detail{}, err
	}
	defer tx.Rollback()
	d := Detail{}
	if isTask {
		t, err := scanTask(tx.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE id=?", key))
		if err != nil {
			return d, err
		}
		key = t.ConversationID
		d.Task = &t
	} else {
		t, err := scanTask(tx.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE conversation_id=?", key))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return d, err
		}
		if err == nil {
			d.Task = &t
		}
	}
	if d.Conversation, err = scanConversation(tx.QueryRow("SELECT "+conversationColumns+" FROM conversations WHERE id=?", key)); err != nil {
		return d, err
	}
	if d.Messages, err = messages(tx, key, false); err != nil {
		return d, err
	}
	r, err := scanRun(tx.QueryRow("SELECT "+runColumns+" FROM runs WHERE conversation_id=? ORDER BY memory_command,rowid DESC LIMIT 1", key))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return d, err
	}
	if err == nil {
		d.Run = &r
		reads, readErr := listTaskReadEvidence(context.Background(), tx, r.ID)
		if readErr != nil {
			return d, readErr
		}
		if len(reads) == 1 {
			d.Read = &reads[0]
		} else {
			d.Reads = reads
		}
		if r.Status == "interrupted" || r.Status == "failed" {
			d.ReadCheckpoint, err = readResumeStatus(tx, r.ID)
			if err != nil {
				return d, err
			}
		}
		refs, readErr := readMemoryRefs(tx, r.ID)
		if readErr != nil {
			return d, readErr
		}
		d.MemoryRefs = refs
	}
	if d.Task != nil {
		graph, graphErr := readWorkflow(context.Background(), tx, d.Task.ID)
		if graphErr == nil {
			d.Workflow = &graph
		} else if !errors.Is(graphErr, sql.ErrNoRows) {
			return d, graphErr
		}
		if err = tx.QueryRow("SELECT coalesce((SELECT parent_id FROM workflow_children WHERE task_id=?),'')", d.Task.ID).
			Scan(&d.WorkflowParentID); err != nil {
			return d, err
		}
		if d.UnknownReadRunID, err = unknownReadRun(tx, d.Task.ID); err != nil {
			return d, err
		}
		usage, err := taskExecutionUsage(tx, d.Task.ID)
		if err != nil {
			return d, err
		}
		d.TaskUsage = &usage
		var a Artifact
		err = tx.QueryRow("SELECT id,task_id,name,content,sha256,created_at FROM artifacts WHERE task_id=? ORDER BY rowid DESC LIMIT 1", d.Task.ID).Scan(&a.ID, &a.TaskID, &a.Name, &a.Content, &a.Digest, &a.CreatedAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return d, err
		}
		if err == nil {
			d.Artifact = &a
			d.ArtifactVersions, err = listArtifacts(tx, d.Task.ID, "", "", "", 20)
			if err != nil {
				return d, err
			}
		}
		publication, readErr := scanPublication(tx.QueryRow("SELECT "+publicationColumns+" FROM publications WHERE task_id=? ORDER BY rowid DESC LIMIT 1", d.Task.ID))
		if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
			return d, readErr
		}
		if readErr == nil && d.Artifact != nil && publication.ArtifactID == d.Artifact.ID {
			if publication.Status == "pending" && publication.PreviousDigest != "" {
				var before string
				if err = tx.QueryRow("SELECT previous_content FROM publications WHERE id=?", publication.ID).Scan(&before); err != nil {
					return d, err
				}
				publication.PreviousContent = &before
			}
			d.Publication = &publication
		}
		if d.Task.Status == "completed" && d.Artifact != nil {
			if d.Acceptance, err = artifactAcceptance(tx, d.Task.ID, d.Artifact.ID); err != nil {
				return d, err
			}
		}
	}
	return d, tx.Commit()
}

const artifactPageSize = 100

func (s *Store) Artifacts(taskID, scope, after string) ([]Artifact, error) {
	return listArtifacts(s.db, taskID, scope, after, "", artifactPageSize)
}

const artifactListColumns = `a.id,a.task_id,a.name,a.sha256,a.created_at,a.run_id,t.title,t.scope_label,
coalesce((SELECT max(accepted_at) FROM task_acceptances WHERE task_id=a.task_id AND artifact_id=a.id),0)`

func listArtifacts(q rowsQueryer, taskID, scope, after, runID string, limit int) ([]Artifact, error) {
	rows, err := q.Query("SELECT "+artifactListColumns+` FROM artifacts a JOIN tasks t ON t.id=a.task_id
WHERE (?='' OR a.task_id=?) AND (?='' OR t.scope_label=?) AND (?='' OR a.run_id=?)
AND (?='' OR a.rowid < (SELECT rowid FROM artifacts WHERE id=?))
ORDER BY a.rowid DESC LIMIT ?`, taskID, taskID, scope, scope, runID, runID, after, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Artifact{}
	for rows.Next() {
		var a Artifact
		if err = rows.Scan(&a.ID, &a.TaskID, &a.Name, &a.Digest, &a.CreatedAt, &a.RunID, &a.TaskTitle, &a.Scope, &a.AcceptedAt); err != nil {
			return nil, err
		}
		items = append(items, a)
	}
	return items, rows.Err()
}

func (s *Store) Artifact(id string) (Artifact, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Artifact{}, err
	}
	defer tx.Rollback()
	var a Artifact
	err = tx.QueryRow("SELECT "+artifactListColumns+",a.content FROM artifacts a JOIN tasks t ON t.id=a.task_id WHERE a.id=?", id).
		Scan(&a.ID, &a.TaskID, &a.Name, &a.Digest, &a.CreatedAt, &a.RunID, &a.TaskTitle, &a.Scope, &a.AcceptedAt, &a.Content)
	if err != nil {
		return a, err
	}
	if a.AcceptedAt != 0 {
		if a.Acceptance, err = artifactAcceptance(tx, a.TaskID, a.ID); err != nil {
			return a, err
		}
	}
	return a, tx.Commit()
}

func artifactAcceptance(tx *sql.Tx, taskID, artifactID string) (*Acceptance, error) {
	var a Acceptance
	err := tx.QueryRow(`SELECT a.artifact_id,a.sha256,a.criteria,a.accepted_at,coalesce(p.id,''),coalesce(p.filename,'')
FROM task_acceptances a LEFT JOIN publications p ON p.id=a.publication_id
WHERE a.task_id=? AND a.artifact_id=? ORDER BY a.revision DESC LIMIT 1`, taskID, artifactID).
		Scan(&a.ArtifactID, &a.Digest, &a.Criteria, &a.AcceptedAt, &a.PublicationID, &a.Filename)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

func (s *Store) Accept(ctx context.Context, taskID, artifactID string, revision int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = ensureNoPublication(tx, taskID); err != nil {
		return err
	}
	if err = ensureReadConfirmed(tx, taskID); err != nil {
		return err
	}
	var currentArtifact, runStatus, taskStatus, content, hash, requiredFile, runID string
	var currentRevision, generation int
	err = tx.QueryRow(`SELECT coalesce(a.id,''),r.status,t.status,coalesce(a.content,''),coalesce(a.sha256,''),t.required_filename,t.revision,r.id,r.execution_generation
FROM tasks t JOIN runs r ON r.task_id=t.id LEFT JOIN artifacts a ON a.run_id=r.id WHERE t.id=? ORDER BY r.rowid DESC LIMIT 1`, taskID).
		Scan(&currentArtifact, &runStatus, &taskStatus, &content, &hash, &requiredFile, &currentRevision, &runID, &generation)
	if err != nil {
		return err
	}
	if currentRevision != revision || taskStatus != "waiting_user" || currentArtifact == "" || currentArtifact != artifactID || runStatus != "completed" || digest(content) != hash {
		return ErrState
	}
	if err = checkWorkflowEvidence(tx, runID, generation); err != nil {
		return err
	}
	var publicationID any
	if requiredFile != "" {
		p, err := scanPublication(tx.QueryRow("SELECT "+publicationColumns+
			" FROM publications WHERE task_id=? AND artifact_id=? AND filename=? AND status='succeeded' ORDER BY rowid DESC LIMIT 1", taskID, artifactID, requiredFile))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDeliveryPending
		}
		if err != nil {
			return err
		}
		status, _ := s.inspectPublication(p, false)
		if p.Digest != hash || status != "succeeded" {
			return ErrDeliveryChanged
		}
		publicationID = p.ID
	}
	acceptedAt := now()
	res, err := tx.Exec("UPDATE tasks SET status='completed',status_reason='accepted',reason='已验收成果',revision=revision+1,completed_at=?,updated_at=? WHERE id=? AND revision=? AND status='waiting_user'", acceptedAt, acceptedAt, taskID, revision)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrState
	}
	if _, err = tx.Exec(`INSERT INTO task_acceptances(task_id,revision,artifact_id,sha256,criteria,accepted_at,publication_id)
SELECT id,revision,?,?,acceptance_criteria,?,? FROM tasks WHERE id=?`, artifactID, hash, acceptedAt, publicationID, taskID); err != nil {
		return err
	}
	if err = recordAcceptedExperience(tx, taskID, artifactID, revision+1, content, hash); err != nil {
		return err
	}
	return tx.Commit()
}
