package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ParseBenchmarkCommandPreview recognizes a benchmark command with a
// separate requirement. A command with no requirement remains a legacy
// command. The requirement is returned without trimming its contents.
func ParseBenchmarkCommandPreview(text string) (BenchmarkProfileRef, string, bool, error) {
	remaining := strings.TrimLeftFunc(text, unicode.IsSpace)
	command, remaining, ok := benchmarkCommandToken(remaining)
	if !ok || command != "/benchmark" {
		return BenchmarkProfileRef{}, "", false, nil
	}
	remaining = strings.TrimLeftFunc(remaining, unicode.IsSpace)
	selection, remaining, ok := benchmarkCommandToken(remaining)
	if !ok || strings.TrimSpace(remaining) == "" {
		return BenchmarkProfileRef{}, "", false, nil
	}
	// Consume only the separator. In particular, keep leading/trailing spaces
	// and subsequent newlines that are part of the user's requirement.
	if strings.HasPrefix(remaining, "\r\n") {
		remaining = remaining[2:]
	} else {
		_, size := utf8.DecodeRuneInString(remaining)
		remaining = remaining[size:]
	}
	key, versionText, found := strings.Cut(selection, "@")
	versionText = strings.TrimPrefix(versionText, "v")
	version, err := strconv.Atoi(versionText)
	if !found || !validBenchKey(key) || err != nil || version < 1 {
		return BenchmarkProfileRef{}, remaining, true, ErrBenchmarkInput
	}
	return BenchmarkProfileRef{Profile: key, Version: version}, remaining, true, nil
}

// benchmarkCommandToken returns the next token and the untouched separator.
func benchmarkCommandToken(text string) (string, string, bool) {
	for i, r := range text {
		if unicode.IsSpace(r) {
			return text[:i], text[i:], i > 0
		}
	}
	return text, "", text != ""
}

type BenchmarkCommandPreviewCase struct {
	Key              string                    `json:"key"`
	Targets          []string                  `json:"targets"`
	DependsOn        []string                  `json:"depends_on,omitempty"`
	Parameters       map[string]string         `json:"parameters,omitempty"`
	ActualConfig     map[string]string         `json:"actual_config,omitempty"`
	RequiredMetrics  map[string]string         `json:"required_metrics"`
	ComparisonKeys   []string                  `json:"comparison_keys,omitempty"`
	NeedsRawEvidence bool                      `json:"needs_raw_evidence,omitempty"`
	Threshold        *BenchmarkMetricThreshold `json:"threshold,omitempty"`
}

type BenchmarkCommandPreviewModelInput struct {
	Text      string                        `json:"text"`
	Scope     string                        `json:"scope_label,omitempty"`
	Profile   BenchmarkProfileRef           `json:"profile"`
	Cases     []BenchmarkCommandPreviewCase `json:"cases"`
	Task      *BenchmarkCommandPreviewTask  `json:"referenced_task,omitempty"`
	History   []Message                     `json:"-"`
	Materials []Material                    `json:"-"`
}

// Only the selected task's planning information is shared. Reports, raw
// evidence, credentials, authorization records and other tasks are excluded.
type BenchmarkCommandPreviewTask struct {
	ID        string                          `json:"id"`
	Title     string                          `json:"title"`
	Goal      string                          `json:"goal"`
	Scope     string                          `json:"scope_label,omitempty"`
	Criteria  string                          `json:"acceptance_criteria,omitempty"`
	Status    string                          `json:"status"`
	Revision  int                             `json:"revision"`
	Benchmark *BenchmarkCommandPreviewTaskRun `json:"benchmark,omitempty"`
}

type BenchmarkCommandPreviewTaskRun struct {
	Profile      BenchmarkProfileRef               `json:"profile"`
	PlanRevision int                               `json:"plan_revision"`
	Cases        []BenchmarkCommandPreviewTaskCase `json:"cases"`
}

type BenchmarkCommandPreviewTaskCase struct {
	Key     string   `json:"key"`
	Phase   string   `json:"phase"`
	Targets []string `json:"frozen_targets"`
}

type BenchmarkCommandPreviewModel func(context.Context, BenchmarkCommandPreviewModelInput, int) (string, error)

var benchmarkCommandChecks = map[string]string{
	"targets":    "核对方案目标、要求的筛选与排除条件，以及这些目标的授权。",
	"health":     "核实目标的实时健康和可用状态；方案和本地清单不能证明实时状态。",
	"scale":      "核实要求的节点或设备数量、拓扑和实际可用资源。",
	"cases":      "对照方案测项与依赖，核实要求的测试范围。",
	"parameters": "对照方案参数和验收要求，核实本次条件能否满足。",
	"schedule":   "核实要求的时间和运行条件。",
	"other":      "逐项核实其余要求及对应的执行方式。",
}

type benchmarkCommandAdvice struct {
	Checks []string `json:"checks"`
	Advice string   `json:"advice,omitempty"`
}

func parseBenchmarkCommandAdvice(raw string) (benchmarkCommandAdvice, error) {
	var result benchmarkCommandAdvice
	if len(raw) > 4096 {
		return result, ErrClassification
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return result, ErrClassification
	}
	fields := map[json.Token]bool{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil || fields[key] {
			return result, ErrClassification
		}
		fields[key] = true
		switch key {
		case "checks":
			if err := dec.Decode(&result.Checks); err != nil {
				return result, ErrClassification
			}
		case "advice":
			var advice *string
			if err := dec.Decode(&advice); err != nil || advice == nil {
				return result, ErrClassification
			}
			result.Advice = strings.TrimSpace(*advice)
			if utf8.RuneCountInString(result.Advice) > 600 || strings.ContainsFunc(result.Advice, func(r rune) bool {
				return unicode.IsControl(r) && r != '\n'
			}) {
				return result, ErrClassification
			}
		default:
			return result, ErrClassification
		}
	}
	if result.Checks == nil || len(result.Checks) > len(benchmarkCommandChecks) {
		return result, ErrClassification
	}
	seen := map[string]bool{}
	for _, check := range result.Checks {
		if benchmarkCommandChecks[check] == "" || seen[check] {
			return result, ErrClassification
		}
		seen[check] = true
	}
	if token, err := dec.Token(); err != nil || token != json.Delim('}') {
		return result, ErrClassification
	}
	if _, err := dec.Token(); err != io.EOF {
		return result, ErrClassification
	}
	return result, nil
}

// benchmarkCommandPlanTx authorizes every target using the same actor, case,
// tool and parameter grants used to create a benchmark task. Provider binding
// and live readiness are intentionally not claimed by this advisory preview.
func benchmarkCommandPlanTx(tx *sql.Tx, actor string, ref BenchmarkProfileRef) (BenchmarkCommandPreviewModelInput, error) {
	if actor == "" {
		return BenchmarkCommandPreviewModelInput{}, ErrAuthorizationDenied
	}
	profile, err := readBenchmarkProfile(tx, ref.Profile, ref.Version)
	if err != nil {
		return BenchmarkCommandPreviewModelInput{}, err
	}
	out := BenchmarkCommandPreviewModelInput{Profile: ref, Cases: make([]BenchmarkCommandPreviewCase, 0, len(profile.Cases))}
	stamp := now()
	for _, c := range profile.Cases {
		for _, target := range c.Targets {
			if _, err := benchmarkAuthorization(tx, actor, target, c, stamp); err != nil {
				return BenchmarkCommandPreviewModelInput{}, err
			}
		}
		out.Cases = append(out.Cases, BenchmarkCommandPreviewCase{
			Key: c.Key, Targets: c.Targets, DependsOn: c.DependsOn, Parameters: c.Parameters,
			ActualConfig: c.ActualConfig, RequiredMetrics: c.RequiredMetrics, ComparisonKeys: c.ComparisonKeys,
			NeedsRawEvidence: c.NeedsRawEvidence, Threshold: c.Threshold,
		})
	}
	return out, nil
}

func benchmarkCommandAdvisory(ref BenchmarkProfileRef, body, scope string, result benchmarkCommandAdvice, hasMaterials bool) string {
	var out strings.Builder
	fmt.Fprintf(&out, "已指定已发布方案 `%s@%d`。\n\n待核实的要求：\n\n", ref.Profile, ref.Version)
	for _, line := range strings.Split(body, "\n") {
		out.WriteString("    ")
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if scope != "" {
		fmt.Fprintf(&out, "\n范围标签：%s（也需核实）。\n", scope)
	}
	if hasMaterials {
		out.WriteString("\n当前材料中的条件也需要逐项核实。\n")
	}
	if result.Advice != "" {
		out.WriteString("\n模型建议（待核对）：\n\n")
		for _, line := range strings.Split(result.Advice, "\n") {
			out.WriteString("    ")
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	if len(result.Checks) > 0 {
		out.WriteString("\n建议核对：\n")
		for _, check := range result.Checks {
			out.WriteString("- ")
			out.WriteString(benchmarkCommandChecks[check])
			out.WriteByte('\n')
		}
	}
	out.WriteString("\n以上要求尚未应用。当前不能验证按这些条件筛选后的执行范围；请先核实全部要求，并使用支持这些条件的执行入口。没有创建或派发任务。")
	return out.String()
}

// Load the same non-isolated history and active material snapshots used for a
// conversation reply, then include all explicitly selected new materials.
func benchmarkCommandContextTx(ctx context.Context, tx *sql.Tx, conversation string, selected []string) ([]Message, []Material, error) {
	if len(selected) > MaxMaterials {
		return nil, nil, ErrMaterialSelection
	}
	var history []Message
	var materials []Material
	var err error
	if conversation != "" {
		history, err = queryMessages(tx, false, `SELECT seq,role,text,replace_materials,run_id FROM messages
WHERE conversation_id=? AND run_id NOT IN (SELECT run_id FROM isolated_reply_pins)
AND run_id NOT IN (SELECT id FROM runs WHERE memory_command=1) ORDER BY seq DESC LIMIT 50`, conversation)
		if err != nil {
			return nil, nil, err
		}
		materials, err = activeMaterials(ctx, tx, conversation)
		if err != nil {
			return nil, nil, err
		}
		// The active snapshots are attached to the new required message. Do
		// not attach metadata-only copies from historical messages as well.
		for i := range history {
			history[i].Materials = nil
		}
	}
	active := map[string]bool{}
	for _, material := range materials {
		active[material.ID] = true
	}
	seen := map[string]bool{}
	for _, id := range selected {
		if seen[id] {
			return nil, nil, ErrMaterialSelection
		}
		seen[id] = true
		var material Material
		var upload string
		err := tx.QueryRowContext(ctx, `SELECT m.id,m.path,m.sha256,m.content,m.captured_at,
CASE WHEN u.id IS NULL THEN 'null' ELSE json_object('id',u.id,'name',u.name,'sha256',u.sha256,
 'bytes',u.size,'part',m.upload_part,'parts',u.parts) END
FROM materials m LEFT JOIN uploads u ON u.id=m.upload_id WHERE m.id=?`, id).
			Scan(&material.ID, &material.Path, &material.Digest, &material.Content, &material.CapturedAt, &upload)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrMaterialSelection
		}
		if err != nil {
			return nil, nil, err
		}
		if digest(material.Content) != material.Digest {
			return nil, nil, ErrMaterialsChanged
		}
		if err := json.Unmarshal([]byte(upload), &material.Upload); err != nil {
			return nil, nil, err
		}
		material.Size = len(material.Content)
		if !active[id] {
			materials = append(materials, material)
			active[id] = true
		}
	}
	return history, materials, nil
}

func benchmarkCommandContextDigest(history []Message, materials []Material) string {
	// Material digests are verified before use, so their metadata is enough to
	// detect a changed snapshot without recording its contents in a receipt.
	return digest(toJSON([]any{history, materials}))
}

// Resolve only the explicit current-conversation reference using the same
// visibility check as ordinary messages. Frozen targets are historical task
// scope, not proof that those assets are currently healthy or available.
func benchmarkCommandTaskTx(ctx context.Context, tx *sql.Tx, actor, conversation string, ref *MessageTaskRef) (*BenchmarkCommandPreviewTask, error) {
	if ref == nil {
		return nil, nil
	}
	if err := checkMessageTaskRef(tx, actor, conversation, ref); err != nil {
		return nil, err
	}
	task := &BenchmarkCommandPreviewTask{ID: ref.TaskID}
	if err := tx.QueryRowContext(ctx, `SELECT t.title,t.goal,t.scope_label,t.acceptance_criteria,
COALESCE(ts.state,t.status),t.revision FROM tasks t
LEFT JOIN task_states ts ON ts.task_id=t.id WHERE t.id=?`, ref.TaskID).
		Scan(&task.Title, &task.Goal, &task.Scope, &task.Criteria, &task.Status, &task.Revision); err != nil {
		return nil, err
	}
	benchmark := &BenchmarkCommandPreviewTaskRun{Cases: []BenchmarkCommandPreviewTaskCase{}}
	err := tx.QueryRowContext(ctx, `SELECT profile_key,profile_version,plan_revision FROM benchmark_runs WHERE task_id=?`, ref.TaskID).
		Scan(&benchmark.Profile.Profile, &benchmark.Profile.Version, &benchmark.PlanRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return task, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT case_key,phase,frozen_targets FROM benchmark_cases WHERE task_id=? ORDER BY case_key`, ref.TaskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c BenchmarkCommandPreviewTaskCase
		var raw string
		if err := rows.Scan(&c.Key, &c.Phase, &raw); err != nil {
			return nil, err
		}
		var targets []ticketTarget
		if err := json.Unmarshal([]byte(raw), &targets); err != nil {
			return nil, err
		}
		c.Targets = make([]string, 0, len(targets))
		for _, target := range targets {
			c.Targets = append(c.Targets, target.Ref)
		}
		benchmark.Cases = append(benchmark.Cases, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	task.Benchmark = benchmark
	return task, nil
}

func benchmarkCommandSnapshotDigest(input BenchmarkCommandPreviewModelInput) string {
	contextDigest := benchmarkCommandContextDigest(input.History, input.Materials)
	if input.Task == nil {
		// Preserve the classification receipt for requests without a reference.
		return contextDigest
	}
	return contextDigest + ":" + digest(toJSON(input.Task))
}

// SubmitBenchmarkCommandPreview records an advisory-only reply. It never
// creates a benchmark task or offers confirmation of an unverified scope.
func (s *Store) SubmitBenchmarkCommandPreview(ctx context.Context, key, actor, conversation, text, scope string, materials []string, taskRef *MessageTaskRef, interpret BenchmarkCommandPreviewModel) (Run, error) {
	ref, body, composed, err := ParseBenchmarkCommandPreview(text)
	if err != nil || !composed || len(key) < 8 || len(key) > 128 {
		return Run{}, ErrBenchmarkInput
	}
	if actor == "" {
		return Run{}, ErrAuthorizationDenied
	}
	if taskRef != nil {
		// Keep a stable reference even if the caller reuses its input object.
		refCopy := *taskRef
		taskRef = &refCopy
	}
	fingerprint := digest(toJSON([]any{"benchmark-command-preview/v1", actor, conversation, text, scope, materials}))
	if taskRef != nil {
		fingerprint = digest(fingerprint + " message task reference " + taskRef.TaskID)
	}
	load := func() (Run, BenchmarkCommandPreviewModelInput, error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return Run{}, BenchmarkCommandPreviewModelInput{}, err
		}
		defer tx.Rollback()
		owner, err := benchmarkRouterActor(ctx, tx, key)
		if err != nil {
			return Run{}, BenchmarkCommandPreviewModelInput{}, err
		}
		if owner != "" && owner != actor {
			return Run{}, BenchmarkCommandPreviewModelInput{}, ErrConflict
		}
		if previous, err := previousSubmission(tx, key, fingerprint); err != nil || previous.ID != "" {
			return previous, BenchmarkCommandPreviewModelInput{}, err
		}
		if owner != "" {
			return Run{}, BenchmarkCommandPreviewModelInput{}, ErrConflict
		}
		if conversation != "" {
			if err := checkConversationSubmission(tx, conversation); err != nil {
				return Run{}, BenchmarkCommandPreviewModelInput{}, err
			}
		}
		task, err := benchmarkCommandTaskTx(ctx, tx, actor, conversation, taskRef)
		if err != nil {
			return Run{}, BenchmarkCommandPreviewModelInput{}, err
		}
		input, err := benchmarkCommandPlanTx(tx, actor, ref)
		input.Text, input.Scope = body, scope
		input.Task = task
		if err == nil {
			input.History, input.Materials, err = benchmarkCommandContextTx(ctx, tx, conversation, materials)
		}
		return Run{}, input, err
	}
	prior, input, err := load()
	if err != nil || prior.ID != "" {
		return prior, err
	}
	if interpret == nil {
		return Run{}, ErrClassification
	}
	classified, err := s.classifySubmission(ctx, key, fingerprint, conversation, text, nil, func(call context.Context, _ string, _ []Message, limit int) (DelegationResult, error) {
		raw, err := interpret(call, input, limit)
		if err != nil {
			if errors.Is(err, ErrImageUnsupported) || errors.Is(err, ErrModelRequestBudget) {
				return DelegationResult{Requests: 1}, err
			}
			return DelegationResult{Requests: 1}, errors.New("模型未能生成基准规划建议；没有创建任务")
		}
		advice, err := parseBenchmarkCommandAdvice(raw)
		if err != nil {
			return DelegationResult{Requests: 1}, errors.New("模型建议格式无效；没有创建任务")
		}
		return DelegationResult{Requests: 1, Route: &RouteDecision{Kind: "benchmark_command_advisory", Goal: toJSON(advice), Fragment: benchmarkCommandSnapshotDigest(input)}}, nil
	})
	if err != nil {
		return Run{}, err
	}
	if classified.Route == nil || classified.Route.Kind != "benchmark_command_advisory" {
		return Run{}, ErrClassification
	}
	if classified.Route.Fragment != benchmarkCommandSnapshotDigest(input) {
		if taskRef != nil {
			contextDigest, _, _ := strings.Cut(classified.Route.Fragment, ":")
			if contextDigest != benchmarkCommandContextDigest(input.History, input.Materials) {
				return Run{}, ErrMaterialsChanged
			}
			return Run{}, ErrTaskRevision
		}
		return Run{}, ErrMaterialsChanged
	}
	goal := classified.Route.Goal
	if strings.HasPrefix(strings.TrimSpace(goal), "[") {
		// A preview classified before upgrading stored only the checklist.
		goal = `{"checks":` + goal + `}`
	}
	advice, err := parseBenchmarkCommandAdvice(goal)
	if err != nil {
		return Run{}, err
	}
	response := benchmarkCommandAdvisory(ref, body, scope, advice, len(input.Materials) > 0)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	owner, err := benchmarkRouterActor(ctx, tx, key)
	if err != nil {
		return Run{}, err
	}
	if owner != "" && owner != actor {
		return Run{}, ErrConflict
	}
	if previous, err := previousSubmission(tx, key, fingerprint); err != nil || previous.ID != "" {
		return previous, err
	}
	if owner != "" {
		return Run{}, ErrConflict
	}
	if _, err := benchmarkCommandPlanTx(tx, actor, ref); err != nil {
		return Run{}, err
	}
	currentTask, err := benchmarkCommandTaskTx(ctx, tx, actor, conversation, taskRef)
	if err != nil {
		return Run{}, err
	}
	if toJSON(currentTask) != toJSON(input.Task) {
		return Run{}, ErrTaskRevision
	}
	history, currentMaterials, err := benchmarkCommandContextTx(ctx, tx, conversation, materials)
	if err != nil {
		return Run{}, err
	}
	if benchmarkCommandContextDigest(history, currentMaterials) != benchmarkCommandContextDigest(input.History, input.Materials) {
		return Run{}, ErrMaterialsChanged
	}
	if _, err := admitChatInputWithFingerprint(tx, key, actor, text, fingerprint, "", now()); err != nil {
		return Run{}, err
	}
	// The standalone mode prevents any existing conversation task from being
	// resumed or changed. "clarify" records this as a completed text response.
	classified.Route = &RouteDecision{Kind: "clarify", Question: response}
	run, err := submitWorkTxWithMode(ctx, tx, key, fingerprint, text, scope, conversation, false, Delivery{}, &classified, materials, true)
	if err != nil {
		return Run{}, err
	}
	if taskRef != nil {
		result, err := tx.ExecContext(ctx, `INSERT INTO message_task_refs(message_seq,task_id,actor)
SELECT seq,?,? FROM messages WHERE run_id=? AND role='user'`, taskRef.TaskID, actor, run.ID)
		if err != nil {
			return Run{}, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return Run{}, err
		}
		if count != 1 {
			return Run{}, ErrState
		}
	}
	if err := linkAdmissionToRun(tx, key, run.ID, "", now()); err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}
