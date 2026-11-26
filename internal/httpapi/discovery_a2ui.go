package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

const (
	a2uiReviewAction  = "review_discovery_candidate"
	a2uiRequestAction = "request_discovery_admission"
	a2uiAdmitTool     = "admit_discovery_candidate"
	a2uiSurfaceTTL    = 10 * time.Minute
)

type discoveryA2UIKey struct{}

type discoveryA2UIRuntime struct {
	secret    [32]byte
	ready     bool
	modelGate chan struct{}
	previewMu sync.Mutex
	previews  map[string]a2uiDiscoveryReply
}

func newDiscoveryA2UIRuntime() *discoveryA2UIRuntime {
	s := &discoveryA2UIRuntime{previews: map[string]a2uiDiscoveryReply{}, modelGate: make(chan struct{}, 4)}
	_, err := rand.Read(s.secret[:])
	s.ready = err == nil
	return s
}

func (s *discoveryA2UIRuntime) mac(values ...string) string {
	mac := hmac.New(sha256.New, s.secret[:])
	raw, _ := json.Marshal(values)
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

func actionSurfacePrefix(action string) string {
	switch action {
	case mcpChooseTransport, mcpSetupStep:
		return "mcp"
	case work.BenchmarkAdmissionPreviewAction:
		return "benchadmission"
	case a2uiBenchmarkPreview, a2uiBenchmarkSyncActual:
		return "benchmark"
	case assistantTaskProgressAction:
		return "taskcard"
	case assistantAssetLedgerAction:
		return "assetcard"
	case assistantAttentionReasonAction:
		return "attentioncard"
	case assistantArtifactPreviewAction:
		return "artifactcard"
	case assistantSkillPreviewAction:
		return "skillcard"
	case assistantMemoryReviewAction:
		return "memorycard"
	case assistantDiscoveryScheduleAction:
		return "schedulecard"
	case assistantBenchmarkCompareAction:
		return "reportcard"
	case assistantAdviceActionName:
		return "advice"
	default:
		return "discovery"
	}
}

// An issued surface and component identify exactly one allowed event in the
// current authenticated session. They do not grant asset or model authority.
func (s *discoveryA2UIRuntime) surface(session, conversation, action, candidate string, revision int64) (string, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	prefix := actionSurfacePrefix(action)
	base := fmt.Sprintf("%s_%x_%x", prefix, time.Now().Unix(), nonce)
	return base + "_" + s.mac(base, session, conversation, action, candidate, strconv.FormatInt(revision, 10)), nil
}

func (s *discoveryA2UIRuntime) validSurface(id, session, conversation, action, candidate string, revision int64, bindings ...string) bool {
	p := strings.Split(id, "_")
	prefix := actionSurfacePrefix(action)
	if len(p) != 4 || p[0] != prefix || len(p[1]) > 16 || len(p[2]) != 16 || len(p[3]) != 32 {
		return false
	}
	stamp, err := strconv.ParseInt(p[1], 16, 64)
	age := time.Since(time.Unix(stamp, 0))
	if err != nil || age < 0 || age > a2uiSurfaceTTL {
		return false
	}
	signature := s.mac(append([]string{strings.Join(p[:3], "_"), session, conversation, action, candidate, strconv.FormatInt(revision, 10)}, bindings...)...)
	return hmac.Equal([]byte(p[3]), []byte(signature))
}

// The review result uses the same form identity if the HTTP acknowledgment is
// lost. Deadline is inherited from the original list, not extended by retry.
func (s *discoveryA2UIRuntime) formSurface(list, session, conversation string, c work.DiscoveryCandidate, run string) string {
	p := strings.Split(list, "_")
	base := "discovery_" + p[1] + "_" + s.mac(list, run)[:16]
	return base + "_" + s.mac(base, session, conversation, a2uiRequestAction, c.ID, strconv.FormatInt(c.SeenAt, 10), c.A2UISnapshot)
}

func (s *discoveryA2UIRuntime) component(surface string, candidate work.DiscoveryCandidate) string {
	return "review_" + s.mac(surface, candidate.ID, strconv.FormatInt(candidate.SeenAt, 10), candidate.A2UISnapshot)
}

type a2uiDiscoveryAction struct {
	Name              string          `json:"name"`
	SurfaceID         string          `json:"surfaceId"`
	SourceComponentID string          `json:"sourceComponentId"`
	Timestamp         string          `json:"timestamp"`
	Context           json.RawMessage `json:"context"`
	UserMessage       json.RawMessage `json:"userMessage"`
	Metadata          json.RawMessage `json:"metadata"`
}

type a2uiDiscoveryContext struct {
	Candidate string `json:"candidate_id"`
	Revision  int64  `json:"revision"`
	Owner     string `json:"owner,omitempty"`
	Location  string `json:"location,omitempty"`
	Role      string `json:"role,omitempty"`
}

type discoveryA2UIConfirmation struct {
	Action    work.DiscoveryA2UIAction
	RunID     string
	Admission work.DiscoveryAdmission
}

type a2uiDiscoveryReply struct {
	State          string              `json:"state"`
	Tool           string              `json:"tool"`
	Conversation   string              `json:"conversation_id"`
	Run            string              `json:"run_id,omitempty"`
	Outcome        string              `json:"outcome,omitempty"`
	Reply          string              `json:"reply,omitempty"`
	Result         any                 `json:"result"`
	Surface        []map[string]any    `json:"surface"`
	ConfirmationID string              `json:"confirmation_id,omitempty"`
	ExpiresAt      int64               `json:"expires_at_unix_ms,omitempty"`
	Input          *assistantToolInput `json:"input,omitempty"`
	Target         any                 `json:"target,omitempty"`
}

var a2uiBrowserKey = regexp.MustCompile(`^[A-Za-z0-9_-]{8,100}$`)
var a2uiCandidateID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (s Server) a2uiDiscoveryAccess(w http.ResponseWriter, r *http.Request) (work.Account, *discoveryA2UIRuntime, bool) {
	account, ok := accountFromContext(r)
	if !ok || !account.IsSuperadmin || account.MustChangePassword {
		fail(w, http.StatusForbidden, "forbidden", "只有当前获授权的管理员可以审阅候选资产")
		return account, nil, false
	}
	runtime, ok := r.Context().Value(discoveryA2UIKey{}).(*discoveryA2UIRuntime)
	if !ok || !runtime.ready || s.assistantSession(r) == "" {
		fail(w, http.StatusServiceUnavailable, "a2ui_unavailable", "候选资产交互暂不可用")
		return account, nil, false
	}
	return account, runtime, true
}

func a2uiDiscoveryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, work.ErrA2UIDiscoveryStale):
		fail(w, http.StatusConflict, "a2ui_candidate_stale", err.Error())
	case errors.Is(err, work.ErrA2UIActionPending):
		fail(w, http.StatusConflict, "a2ui_action_pending", err.Error())
	case errors.Is(err, work.ErrA2UIActionFailed):
		fail(w, http.StatusConflict, "a2ui_action_failed", err.Error())
	case errors.Is(err, work.ErrTaskWorkerModelRoleUnavailable):
		fail(w, http.StatusServiceUnavailable, "a2ui_model_unavailable", "请先配置允许读取已授权结果的助手任务模型；未发生纳管")
	case errors.Is(err, sql.ErrNoRows):
		fail(w, http.StatusNotFound, "a2ui_target_unavailable", "会话或候选资产不可查看，请重新打开列表")
	default:
		writeError(w, err)
	}
}

func a2uiText(id, text string) map[string]any {
	return map[string]any{"id": id, "component": "Text", "text": text}
}

func a2uiButton(id, label, name string, context map[string]any) map[string]any {
	return map[string]any{"id": id, "component": "Button", "child": label, "variant": "primary",
		"action": map[string]any{"event": map[string]any{"name": name, "context": context}}}
}

func a2uiColumn(children []string) map[string]any {
	return map[string]any{"id": "root", "component": "Column", "children": children}
}

func a2uiCreateSurface(id string, components []map[string]any, data map[string]any) []map[string]any {
	create := map[string]any{"surfaceId": id, "catalogId": assistantA2UICatalog, "components": components}
	if data != nil {
		create["dataModel"] = data
	}
	return []map[string]any{{"version": "1.0", "createSurface": create}}
}

func a2uiCandidateDisplay(c work.DiscoveryCandidate) string {
	// These fields are shown only to the authorized user, never to the model.
	return assistantSurfaceClip(assistantSurfaceParts(c.Serial, c.Address, c.Scope))
}

func (s Server) a2uiDiscoveryList(w http.ResponseWriter, r *http.Request) {
	account, runtime, ok := s.a2uiDiscoveryAccess(w, r)
	if !ok {
		return
	}
	s.writeA2UIDiscoveryList(w, r, account, runtime, r.PathValue("id"), "")
}

func (s Server) a2uiDiscoveryOpen(w http.ResponseWriter, r *http.Request) {
	account, runtime, ok := s.a2uiDiscoveryAccess(w, r)
	if !ok {
		return
	}
	reqKey := r.Header.Get("X-ClusBot-Request-Key")
	if len(r.Header.Values("X-ClusBot-Request-Key")) != 1 || !a2uiBrowserKey.MatchString(reqKey) {
		fail(w, 400, "a2ui_request_key_required", "缺少有效的操作请求标识")
		return
	}
	var body map[string]json.RawMessage
	if !decode(w, r, &body) {
		return
	}
	if body == nil || len(body) != 0 {
		fail(w, 400, "a2ui_invalid_request", "打开候选列表不接受其他参数")
		return
	}
	run, err := s.Store.OpenDiscoveryA2UIConversation(r.Context(), account.PrincipalID, reqKey)
	if err != nil {
		a2uiDiscoveryError(w, err)
		return
	}
	s.writeA2UIDiscoveryList(w, r, account, runtime, run.ConversationID, run.ID)
}

func (s Server) writeA2UIDiscoveryList(w http.ResponseWriter, r *http.Request, account work.Account, runtime *discoveryA2UIRuntime, conversation, run string) {
	if !validConversationID(conversation) {
		fail(w, 400, "a2ui_conversation_required", "请先打开一段有效会话")
		return
	}
	candidates, err := s.Store.DiscoveryA2UICandidates(r.Context(), account.PrincipalID, conversation)
	if err != nil {
		a2uiDiscoveryError(w, err)
		return
	}
	id, err := runtime.surface(s.assistantSession(r), conversation, a2uiReviewAction, "", 0)
	if err != nil {
		a2uiDiscoveryError(w, err)
		return
	}
	children := []string{"heading"}
	components := []map[string]any{nil, a2uiText("heading", "候选资产")}
	for i, candidate := range candidates[:min(len(candidates), assistantSurfaceRows)] {
		if !a2uiCandidateID.MatchString(candidate.ID) {
			continue
		}
		component := runtime.component(id, candidate)
		label := "review_label_" + strconv.Itoa(i)
		children = append(children, component)
		components = append(components, a2uiText(label, "审阅："+a2uiCandidateDisplay(candidate)), a2uiButton(component, label,
			a2uiReviewAction, map[string]any{"candidate_id": candidate.ID, "revision": candidate.SeenAt}))
	}
	if len(children) == 1 {
		children = append(children, "empty")
		components = append(components, a2uiText("empty", "没有当前可审阅的候选资产。"))
	}
	if len(candidates) > assistantSurfaceRows {
		children = append(children, "limited")
		components = append(components, a2uiText("limited", "仅显示最近 10 条。"))
	}
	components[0] = a2uiColumn(children)
	jsonResponse(w, 200, a2uiDiscoveryReply{State: "completed", Tool: "get_discovery_candidates", Result: map[string]any{"count": len(candidates)},
		Conversation: conversation, Run: run, Surface: a2uiCreateSurface(id, components, nil)})
}

func a2uiStrictObject(raw []byte, fields ...string) bool {
	expected := map[string]bool{}
	for _, key := range fields {
		expected[strings.TrimPrefix(key, "?")] = !strings.HasPrefix(key, "?")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if _, exists := expected[key]; err != nil || !ok || !exists {
			return false
		}
		delete(expected, key)
		var ignored json.RawMessage
		if dec.Decode(&ignored) != nil {
			return false
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return false
	}
	for _, required := range expected {
		if required {
			return false
		}
	}
	_, err := dec.Token()
	return err == io.EOF
}

func validDiscoveryField(value string) bool {
	return strings.TrimSpace(value) == value && value != "" && len(value) <= 256 &&
		!strings.ContainsFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.In(r, unicode.Cf) }) &&
		!assistantCredentialValue.MatchString(value)
}

func (s Server) a2uiDiscoveryAction(w http.ResponseWriter, r *http.Request) {
	account, runtime, ok := s.a2uiDiscoveryAccess(w, r)
	if !ok {
		return
	}
	conversation := r.PathValue("id")
	reqKey := r.Header.Get("X-ClusBot-Request-Key")
	if !validConversationID(conversation) || len(r.Header.Values("X-ClusBot-Request-Key")) != 1 || !a2uiBrowserKey.MatchString(reqKey) {
		fail(w, 400, "a2ui_invalid_request", "需要当前会话和有效的操作请求标识")
		return
	}
	if readOnlyAssistant(r) {
		fail(w, 400, "a2ui_model_required", "此交互需要助手模型；只读固定查询不会转成纳管预览")
		return
	}
	var raw json.RawMessage
	if !decode(w, r, &raw) {
		return
	}
	var msg struct {
		Version string          `json:"version"`
		Action  json.RawMessage `json:"action"`
	}
	var action a2uiDiscoveryAction
	if len(raw) > 8192 || !a2uiStrictObject(raw, "version", "action") || json.Unmarshal(raw, &msg) != nil || msg.Version != "1.0" ||
		!a2uiStrictObject(msg.Action, "name", "surfaceId", "sourceComponentId", "timestamp", "context", "?userMessage", "?metadata") ||
		json.Unmarshal(msg.Action, &action) != nil || (action.Name != a2uiReviewAction && action.Name != a2uiRequestAction) ||
		!validA2UIUserMessage(action.UserMessage) || !validA2UIMetadata(action.Metadata) {
		fail(w, 400, "a2ui_invalid_action", "无效的 A2UI 行为消息")
		return
	}
	fields := []string{"candidate_id", "revision"}
	if action.Name == a2uiRequestAction {
		fields = append(fields, "owner", "location", "role")
	}
	var in a2uiDiscoveryContext
	_, timeErr := time.Parse(time.RFC3339Nano, action.Timestamp)
	if !a2uiStrictObject(action.Context, fields...) || json.Unmarshal(action.Context, &in) != nil ||
		!a2uiCandidateID.MatchString(in.Candidate) || in.Revision < 1 || in.Revision > (1<<53)-1 || timeErr != nil ||
		(action.Name == a2uiRequestAction && (!validDiscoveryField(in.Owner) || !validDiscoveryField(in.Location) || !validDiscoveryField(in.Role))) {
		fail(w, 400, "a2ui_invalid_action", "候选资产引用、观测版本或纳管资料无效")
		return
	}
	session := s.assistantSession(r)
	issued, err := s.Store.DiscoveryA2UICandidate(r.Context(), account.PrincipalID, conversation, in.Candidate, in.Revision)
	if err != nil {
		a2uiDiscoveryError(w, err)
		return
	}
	surfaceCandidate, surfaceRevision, bindings := in.Candidate, in.Revision, []string{issued.A2UISnapshot}
	component := "request_admission"
	if action.Name == a2uiReviewAction {
		surfaceCandidate, surfaceRevision, bindings = "", 0, nil
		component = runtime.component(action.SurfaceID, issued)
	}
	if action.SourceComponentID != component || !runtime.validSurface(action.SurfaceID, session, conversation, action.Name, surfaceCandidate, surfaceRevision, bindings...) {
		fail(w, 409, "a2ui_surface_stale", "此界面行为已过期或与当前会话不匹配，请重新打开候选资产")
		return
	}
	fingerprint, _ := json.Marshal([]any{action.Name, action.SurfaceID, component, in})
	request := work.DiscoveryA2UIAction{Actor: account.PrincipalID, Conversation: conversation, ClientKey: reqKey,
		Candidate: in.Candidate, Revision: in.Revision, Name: action.Name, Snapshot: issued.A2UISnapshot,
		Fingerprint: fmt.Sprintf("%x", sha256.Sum256(fingerprint))}
	select {
	case runtime.modelGate <- struct{}{}:
		defer func() { <-runtime.modelGate }()
	default:
		fail(w, http.StatusTooManyRequests, "a2ui_model_busy", "候选资产审阅正在处理，请稍后重试")
		return
	}
	candidate, receipt, err := s.Store.BeginDiscoveryA2UIAction(r.Context(), request)
	if err != nil {
		a2uiDiscoveryError(w, err)
		return
	}
	duplicate := receipt.Run.ID != ""
	if !duplicate {
		slotCtx, stop := context.WithTimeout(r.Context(), 750*time.Millisecond)
		release, slotErr := s.Store.AcquireDiscoveryA2UIModelSlot(slotCtx)
		stop()
		if slotErr != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
			defer cancel()
			_ = s.Store.FailDiscoveryA2UIAction(cleanup, request)
			fail(w, http.StatusTooManyRequests, "a2ui_model_busy", "助手当前没有可用模型执行名额；未调用模型或执行纳管")
			return
		}
		defer release()
		results := []string{"show_review", "ask_for_details", "cannot_prepare"}
		recordText := "审阅候选资产"
		suffix := " 这是候选记录，尚未纳管。"
		if action.Name == a2uiRequestAction {
			results[0] = "show_admission_preview"
			recordText = "请求候选资产纳管预览"
			suffix = " 这是纳管预览，尚未执行。"
		}
		modelRaw, callErr := s.Settings.InterpretDiscoveryA2UI(r.Context(), model.DiscoveryA2UIModelInput{
			CandidateID: "candidate_" + runtime.mac("model-reference", account.PrincipalID, conversation, reqKey, candidate.ID),
			Revision:    candidate.SeenAt, State: candidate.State, Action: action.Name,
			Allowed: results, Fields: []string{"owner", "location", "role"},
		}, func(ctx context.Context) error {
			if err := s.verifyDiscoveryA2UISession(ctx, session, account.PrincipalID); err != nil {
				return err
			}
			verified, err := s.Store.DiscoveryA2UICandidate(ctx, account.PrincipalID, conversation, in.Candidate, in.Revision)
			if err == nil && !sameA2UICandidate(candidate, verified) {
				return work.ErrA2UIDiscoveryStale
			}
			return err
		})
		finishCtx, stop := context.WithTimeout(context.WithoutCancel(r.Context()), 4*time.Second)
		defer stop()
		failReceipt := func() { _ = s.Store.FailDiscoveryA2UIAction(finishCtx, request) }
		if callErr != nil {
			failReceipt()
			if errors.Is(callErr, work.ErrTaskWorkerModelRoleUnavailable) || errors.Is(callErr, sql.ErrNoRows) ||
				errors.Is(callErr, work.ErrA2UIDiscoveryStale) || errors.Is(callErr, work.ErrAuthorizationDenied) {
				a2uiDiscoveryError(w, callErr)
			} else {
				fail(w, http.StatusBadGateway, "a2ui_model_failed", "助手未完成审阅；没有执行纳管")
			}
			return
		}
		var advice struct {
			Action string `json:"action"`
			Reply  string `json:"reply"`
		}
		if len(modelRaw) > 4096 || !a2uiStrictObject(modelRaw, "action", "reply") ||
			json.Unmarshal(modelRaw, &advice) != nil || !slices.Contains(results, advice.Action) ||
			strings.TrimSpace(advice.Reply) == "" || utf8.RuneCountInString(advice.Reply) > 400 || assistantCredentialValue.MatchString(advice.Reply) {
			failReceipt()
			fail(w, http.StatusBadGateway, "a2ui_model_invalid", "助手回复未符合受限审阅格式；没有执行纳管")
			return
		}
		if advice.Action == "ask_for_details" || advice.Action == "cannot_prepare" {
			suffix = " 尚未生成可确认的纳管预览，也没有执行纳管。"
		}
		if err := s.verifyDiscoveryA2UISession(finishCtx, session, account.PrincipalID); err != nil {
			failReceipt()
			a2uiDiscoveryError(w, err)
			return
		}
		candidate, receipt, err = s.Store.FinishDiscoveryA2UIAction(finishCtx, request, advice.Action, recordText, assistantSurfaceClip(assistantSurfaceValue(advice.Reply))+suffix)
		if err != nil {
			failReceipt()
			a2uiDiscoveryError(w, err)
			return
		}
	}
	switch receipt.Outcome {
	case "show_review":
		s.writeA2UIReview(w, runtime, session, action.SurfaceID, candidate, receipt.Run)
	case "show_admission_preview":
		s.writeA2UIAdmissionPreview(w, r, runtime, session, candidate, in, receipt.Run, request, duplicate)
	default:
		s.writeA2UIDiscoveryReceipt(w, runtime, session, candidate, receipt, false)
	}
}

func validA2UIUserMessage(raw json.RawMessage) bool {
	if raw == nil {
		return true
	}
	var value *string
	return len(raw) <= 4096 && json.Unmarshal(raw, &value) == nil && value != nil && len(*value) <= 1024
}

func validA2UIMetadata(raw json.RawMessage) bool {
	if raw == nil {
		return true
	}
	var value struct {
		Extensions map[string]json.RawMessage `json:"extensions"`
	}
	return len(raw) <= 1024 && a2uiStrictObject(raw, "?extensions") && json.Unmarshal(raw, &value) == nil
}

func (s Server) a2uiDiscoveryReadback(w http.ResponseWriter, r *http.Request) {
	account, runtime, ok := s.a2uiDiscoveryAccess(w, r)
	if !ok {
		return
	}
	conversation, reqKey := r.PathValue("id"), r.PathValue("key")
	if !validConversationID(conversation) || !a2uiBrowserKey.MatchString(reqKey) {
		fail(w, 400, "a2ui_invalid_request", "需要当前会话和有效的操作请求标识")
		return
	}
	candidate, receipt, err := s.Store.ReadDiscoveryA2UIAction(r.Context(), account.PrincipalID, conversation, reqKey)
	if err != nil {
		a2uiDiscoveryError(w, err)
		return
	}
	if receipt.Admission != nil {
		s.writeA2UIAdmitted(w, runtime, s.assistantSession(r), receipt)
		return
	}
	if receipt.Outcome == "show_review" {
		session := s.assistantSession(r)
		id, err := runtime.surface(session, conversation, a2uiReviewAction, "", 0)
		if err != nil {
			a2uiDiscoveryError(w, err)
			return
		}
		s.writeA2UIReview(w, runtime, session, id, candidate, receipt.Run)
		return
	}
	s.writeA2UIDiscoveryReceipt(w, runtime, s.assistantSession(r), candidate, receipt, true)
}

func (s Server) writeA2UIDiscoveryReceipt(w http.ResponseWriter, runtime *discoveryA2UIRuntime, session string, candidate work.DiscoveryCandidate, receipt work.DiscoveryA2UIReceipt, readback bool) {
	run := receipt.Run
	id, err := runtime.surface(session, run.ConversationID, receipt.Action, candidate.ID, candidate.SeenAt)
	if err != nil {
		a2uiDiscoveryError(w, err)
		return
	}
	line := run.Response
	if readback && receipt.Outcome == "show_admission_preview" {
		line += " 此回执不签发新的确认。请先核对目标当前状态，再决定是否重新发起。"
	}
	jsonResponse(w, 200, a2uiDiscoveryReply{
		State: "completed", Tool: receipt.Action, Outcome: receipt.Outcome, Conversation: run.ConversationID, Run: run.ID, Reply: line,
		Result:  map[string]any{"id": candidate.ID, "state": candidate.State, "revision": candidate.SeenAt},
		Surface: a2uiCreateSurface(id, []map[string]any{a2uiColumn([]string{"advice"}), a2uiText("advice", line)}, nil),
	})
}

func sameA2UICandidate(a, b work.DiscoveryCandidate) bool {
	return a.A2UISnapshot != "" && a.A2UISnapshot == b.A2UISnapshot
}

func (s Server) verifyDiscoveryA2UISession(ctx context.Context, digest, actor string) error {
	return s.verifyA2UIAccount(ctx, digest, actor, true)
}

func (s Server) verifyA2UIAccount(ctx context.Context, digest, actor string, admin bool) error {
	if s.instanceStore == nil || digest == "" {
		return work.ErrAuthorizationDenied
	}
	a, err := s.instanceStore.AccountForSession(ctx, digest)
	if err != nil {
		return err
	}
	if a.PrincipalID != actor || (admin && !a.IsSuperadmin) || a.MustChangePassword {
		return work.ErrAuthorizationDenied
	}
	return nil
}

func (s Server) writeA2UIReview(w http.ResponseWriter, runtime *discoveryA2UIRuntime, session, list string, c work.DiscoveryCandidate, run work.Run) {
	surface := runtime.formSurface(list, session, run.ConversationID, c, run.ID)
	field := func(key, label string) map[string]any {
		return map[string]any{"id": key, "component": "TextField", "label": label, "value": map[string]string{"@path": "/form/" + key}}
	}
	bind := func(key string) map[string]string { return map[string]string{"@path": "/form/" + key} }
	out := a2uiDiscoveryReply{State: "completed", Tool: a2uiReviewAction, Outcome: "show_review", Conversation: run.ConversationID, Run: run.ID, Reply: run.Response,
		Result: map[string]any{"id": c.ID, "state": c.State, "revision": c.SeenAt},
		Surface: a2uiCreateSurface(surface, []map[string]any{
			a2uiColumn([]string{"candidate", "advice", "owner", "location", "role", "request_admission"}),
			a2uiText("candidate", a2uiCandidateDisplay(c)), a2uiText("advice", run.Response),
			field("owner", "负责人"), field("location", "位置"), field("role", "用途"),
			a2uiText("request_label", "生成纳管预览"), a2uiButton("request_admission", "request_label", a2uiRequestAction,
				map[string]any{"candidate_id": c.ID, "revision": c.SeenAt, "owner": bind("owner"), "location": bind("location"), "role": bind("role")}),
		}, map[string]any{"form": map[string]string{"owner": "", "location": "", "role": ""}})}
	jsonResponse(w, 200, out)
}

func (s Server) writeA2UIAdmissionPreview(w http.ResponseWriter, r *http.Request, runtime *discoveryA2UIRuntime,
	session string, candidate work.DiscoveryCandidate, in a2uiDiscoveryContext, run work.Run, request work.DiscoveryA2UIAction, duplicate bool) {
	bridge, ok := r.Context().Value(assistantBridgeKey{}).(*assistantBridge)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "a2ui_unavailable", "确认服务暂不可用")
		return
	}
	runtime.previewMu.Lock()
	defer runtime.previewMu.Unlock()
	for key, old := range runtime.previews {
		if old.ExpiresAt <= time.Now().UnixMilli() {
			delete(runtime.previews, key)
		}
	}
	if prior, ok := runtime.previews[run.ID]; ok {
		if bridge.hasPreview(prior.ConfirmationID, session) {
			jsonResponse(w, 200, prior)
			return
		}
		delete(runtime.previews, run.ID)
	}
	if duplicate {
		fail(w, http.StatusConflict, "a2ui_preview_unavailable", "这次请求不能再签发确认；请核对结果或重新打开候选资产")
		return
	}
	input, _ := json.Marshal(map[string]any{"confirmed_serial": candidate.Serial, "expected_seen_at": candidate.SeenAt,
		"owner": in.Owner, "location": in.Location, "role": in.Role})
	id, expiresAt, ok := bridge.prepare(assistantPreview{session: session, tool: a2uiAdmitTool,
		a2ui: &discoveryA2UIConfirmation{Action: request, RunID: run.ID, Admission: work.DiscoveryAdmission{
			Candidate: candidate.ID, ConfirmedSerial: candidate.Serial, ExpectedSeenAt: candidate.SeenAt,
			Owner: in.Owner, Location: in.Location, Role: in.Role,
		}}})
	if !ok {
		fail(w, http.StatusTooManyRequests, "assistant_preview_busy", "待确认操作过多，当前未执行纳管")
		return
	}
	surface, err := runtime.surface(session, run.ConversationID, a2uiAdmitTool, candidate.ID, candidate.SeenAt)
	if err != nil {
		a2uiDiscoveryError(w, err)
		return
	}
	reply := a2uiDiscoveryReply{State: "confirmation_required", Tool: a2uiAdmitTool, Outcome: "show_admission_preview", Conversation: run.ConversationID, Run: run.ID,
		Reply: run.Response, ConfirmationID: id, ExpiresAt: expiresAt.UnixMilli(),
		Input:  &assistantToolInput{Tool: a2uiAdmitTool, Params: map[string]string{"id": candidate.ID}, Body: input},
		Target: map[string]any{"id": candidate.ID, "serial": candidate.Serial, "address": candidate.Address, "scope": candidate.Scope},
		Surface: a2uiCreateSurface(surface, []map[string]any{
			a2uiColumn([]string{"preview_title", "candidate", "advice"}), a2uiText("preview_title", "纳管预览"),
			a2uiText("candidate", a2uiCandidateDisplay(candidate)), a2uiText("advice", run.Response),
		}, nil)}
	runtime.previews[run.ID] = reply
	jsonResponse(w, 200, reply)
}

func (s Server) a2uiDiscoveryConfirm(w http.ResponseWriter, r *http.Request) {
	account, runtime, ok := s.a2uiDiscoveryAccess(w, r)
	if !ok {
		return
	}
	conversation, requestKey := r.PathValue("id"), r.PathValue("key")
	if !validConversationID(conversation) || !a2uiBrowserKey.MatchString(requestKey) || readOnlyAssistant(r) {
		fail(w, 400, "a2ui_invalid_request", "此请求不能确认纳管")
		return
	}
	var in struct {
		ID string `json:"confirmation_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	bridge, ok := r.Context().Value(assistantBridgeKey{}).(*assistantBridge)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "a2ui_unavailable", "确认服务暂不可用")
		return
	}
	session := s.assistantSession(r)
	preview, ok := bridge.takeForConfirmation(in.ID, session, conversation, requestKey)
	if !ok || preview.a2ui == nil || preview.a2ui.Action.Actor != account.PrincipalID {
		fail(w, http.StatusConflict, "assistant_confirmation_expired", "确认已失效或与本轮不符，请核对原请求")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := s.verifyDiscoveryA2UISession(ctx, session, account.PrincipalID); err != nil {
		a2uiDiscoveryError(w, err)
		return
	}
	p := preview.a2ui
	receipt, err := s.Store.ConfirmDiscoveryA2UIAction(ctx, p.Action, p.RunID, p.Admission)
	if err != nil {
		a2uiDiscoveryError(w, err)
		return
	}
	s.writeA2UIAdmitted(w, runtime, session, receipt)
}

func (s Server) writeA2UIAdmitted(w http.ResponseWriter, runtime *discoveryA2UIRuntime, session string, receipt work.DiscoveryA2UIReceipt) {
	item := receipt.Admission
	text := "已纳管候选资产，正式资产编号：" + item.Asset + "。这不等于验收通过。"
	id, err := runtime.surface(session, receipt.Run.ConversationID, "admitted", item.ID, item.Revision)
	if err != nil {
		a2uiDiscoveryError(w, err)
		return
	}
	jsonResponse(w, 200, a2uiDiscoveryReply{State: "completed", Tool: a2uiAdmitTool, Outcome: "admitted", Conversation: receipt.Run.ConversationID,
		Run: receipt.Run.ID, Reply: text, Result: item, Surface: a2uiCreateSurface(id,
			[]map[string]any{a2uiColumn([]string{"admitted"}), a2uiText("admitted", text)}, nil)})
}
