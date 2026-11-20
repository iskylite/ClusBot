package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"clusbot/internal/mcpclient"
	"clusbot/internal/model"
	"clusbot/internal/work"
)

type Server struct {
	Store             *work.Store
	Token             string
	Hosts             []string
	Settings          *model.Settings
	WorkspacePath     string
	TicketProvider    *work.TicketProvider
	BenchmarkProvider work.BenchmarkProvider
	BenchmarkEvents   *work.BenchmarkEventReceiver
	DiscoverySource   work.DiscoveryScanSource
}

type submission struct {
	TaskRef                     *work.MessageTaskRef           `json:"task_ref"`
	RequestID                   string                         `json:"client_request_id"`
	MessageID                   string                         `json:"client_message_id"`
	Text                        string                         `json:"text"`
	Scope                       string                         `json:"scope_label"`
	AttachmentIDs               []string                       `json:"attachment_ids"`
	ReplaceMaterials            bool                           `json:"replace_materials"`
	Criteria                    *string                        `json:"acceptance_criteria"`
	Filename                    *string                        `json:"required_filename"`
	Revision                    int                            `json:"revision"`
	InterpretBenchmarkExecution bool                           `json:"interpret_benchmark_execution"`
	BenchmarkExecutionTarget    *work.BenchmarkExecutionTarget `json:"benchmark_execution_target"`
	BenchmarkRevisionTarget     *work.BenchmarkRevisionTarget  `json:"benchmark_revision_target"`
	Read                        *work.TaskReadRequest          `json:"mcp_read"`
	Reads                       []work.TaskReadRequest         `json:"mcp_reads"`
}

func (s Server) Handler(assets fs.FS) http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/session", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 200, map[string]any{"authenticated": s.authenticated(r)})
	})
	api.HandleFunc("POST /api/v1/session", s.login)
	api.HandleFunc("DELETE /api/v1/session", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "clusbot_session", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		w.WriteHeader(http.StatusNoContent)
	})
	api.HandleFunc("GET /api/v1/settings", s.modelSettings)
	api.HandleFunc("GET /api/v1/settings/model/runtime", s.modelRuntimeProjection)
	api.HandleFunc("GET /api/v1/settings/model/profiles", s.modelProfiles)
	api.HandleFunc("GET /api/v1/settings/model/roles/benchmark-admission", s.readBenchmarkAdmissionRole)
	api.HandleFunc("PUT /api/v1/settings/model/roles/benchmark-admission", s.setBenchmarkAdmissionRole)
	api.HandleFunc("GET /api/v1/settings/model/roles/benchmark-revision", s.readBenchmarkRevisionRole)
	api.HandleFunc("PUT /api/v1/settings/model/roles/benchmark-revision", s.setBenchmarkRevisionRole)
	api.HandleFunc("GET /api/v1/settings/model/roles/benchmark-execution", s.readBenchmarkExecutionRole)
	api.HandleFunc("PUT /api/v1/settings/model/roles/benchmark-execution", s.setBenchmarkExecutionRole)
	api.HandleFunc("GET /api/v1/settings/model/roles/general-router", s.readGeneralRouterRole)
	api.HandleFunc("PUT /api/v1/settings/model/roles/general-router", s.setGeneralRouterRole)
	api.HandleFunc("POST /api/v1/settings/model/profiles", s.saveModelProfile)
	api.HandleFunc("PUT /api/v1/settings/model/profiles/{id}", s.saveModelProfile)
	api.HandleFunc("DELETE /api/v1/settings/model/profiles/{id}", s.deleteModelProfile)
	api.HandleFunc("POST /api/v1/settings/model/profiles/{id}/test", s.testModelProfile)
	api.HandleFunc("PATCH /api/v1/settings", s.saveModelSettings)
	api.HandleFunc("POST /api/v1/settings/model/test", s.testModelSettings)
	api.HandleFunc("POST /api/v1/settings/model/discover", s.discoverModels)
	api.HandleFunc("GET /api/v1/overview", s.overview)
	api.HandleFunc("GET /api/v1/workspace/files", s.materialFiles)
	api.HandleFunc("POST /api/v1/attachments/workspace", s.captureMaterial)
	api.HandleFunc("POST /api/v1/attachments", s.uploadMaterial)
	api.HandleFunc("POST /api/v1/attachments/uploads/{id}/part", s.selectUploadPart)
	api.HandleFunc("GET /api/v1/experience-candidates", s.experienceCandidates)
	api.HandleFunc("POST /api/v1/experience-candidates/from-acceptance", s.captureAcceptedExperience)
	api.HandleFunc("POST /api/v1/experience-candidates/from-correction", s.captureExperienceCorrection)
	api.HandleFunc("POST /api/v1/experience-candidates/{id}/review", s.reviewExperience)
	api.HandleFunc("GET /api/v1/memories", s.memories)
	api.HandleFunc("POST /api/v1/memories", s.saveMemory)
	api.HandleFunc("GET /api/v1/memories/source/{message}", s.memorySource)
	api.HandleFunc("PATCH /api/v1/memories/{id}", s.saveMemory)
	api.HandleFunc("DELETE /api/v1/memories/{id}", s.deleteMemory)
	api.HandleFunc("GET /api/v1/connections", s.connections)
	api.HandleFunc("POST /api/v1/connections", s.saveConnection)
	api.HandleFunc("GET /api/v1/connections/{id}", s.connection)
	api.HandleFunc("PATCH /api/v1/connections/{id}", s.saveConnection)
	api.HandleFunc("POST /api/v1/connections/{id}/test", s.testConnection)
	api.HandleFunc("POST /api/v1/connections/{id}/permission", s.toolPermission)
	api.HandleFunc("GET /api/v1/connections/{id}/calls", s.connectionCalls)
	api.HandleFunc("POST /api/v1/connections/{id}/call", s.callConnectionTool)
	api.HandleFunc("GET /api/v1/tool-results/{id}/content", s.connectionEvidence)
	api.HandleFunc("GET /api/v1/task-tools", s.taskTools)

	api.HandleFunc("GET /api/v1/skill-proposals", s.skillProposals)
	api.HandleFunc("POST /api/v1/skill-proposals", s.proposeSkill)
	api.HandleFunc("POST /api/v1/skill-proposals/{id}/replay", s.replaySkillProposal)
	api.HandleFunc("POST /api/v1/skill-proposals/{id}/approve", s.approveSkillProposal)
	api.HandleFunc("GET /api/v1/skills", s.listSkills)
	api.HandleFunc("POST /api/v1/skills", s.draftSkill)
	api.HandleFunc("POST /api/v1/skills/import-preview", s.previewSkillDocument)
	api.HandleFunc("POST /api/v1/skills/import", s.importSkillDocument)
	api.HandleFunc("POST /api/v1/skills/{id}/activate", s.activateSkill)
	api.HandleFunc("POST /api/v1/skills/{id}/disable", s.disableSkill)
	api.HandleFunc("GET /api/v1/goals", s.listGoals)
	api.HandleFunc("POST /api/v1/goals", s.createGoal)
	api.HandleFunc("POST /api/v1/goals/{id}/stop", s.stopGoal)
	api.HandleFunc("POST /api/v1/workflows", s.createWorkflow)
	api.HandleFunc("GET /api/v1/tasks/{id}/workflow", s.workflow)
	api.HandleFunc("GET /api/v1/admissions", s.pendingAdmissions)
	api.HandleFunc("GET /api/v1/admissions/{key}", s.admission)
	api.HandleFunc("GET /api/v1/decisions", s.listDecisions)
	api.HandleFunc("GET /api/v1/attention", s.listAttention)
	api.HandleFunc("GET /api/v1/attention/events", s.attentionEvents)
	api.HandleFunc("GET /api/v1/attention/{id}", s.attentionDetail)
	api.HandleFunc("GET /api/v1/approvals", s.listDecisions)
	api.HandleFunc("POST /api/v1/approvals/{id}/decision", s.decideApproval)
	api.HandleFunc("GET /api/v1/domain/snapshot", s.domainSnapshot)
	api.HandleFunc("POST /api/v1/domain/asset-acceptance", s.submitAssetAcceptance)
	api.HandleFunc("GET /api/v1/domain/asset-acceptance/{task_id}", s.assetAcceptanceProgress)
	api.HandleFunc("POST /api/v1/domain/tickets", s.submitDomainTicket)
	api.HandleFunc("GET /api/v1/domain/ticket-operations/{id}", s.domainTicketProgress)
	api.HandleFunc("GET /api/v1/domain/benchmark-profiles", s.benchmarkProfiles)
	api.HandleFunc("POST /api/v1/domain/benchmark-profiles", s.publishBenchmarkProfile)
	api.HandleFunc("GET /api/v1/domain/benchmark-profiles/{key}/versions", s.benchmarkProfileVersions)
	api.HandleFunc("GET /api/v1/domain/benchmark-profiles/{key}/versions/{version}", s.benchmarkProfileVersion)
	api.HandleFunc("GET /api/v1/domain/benchmark-chat-targets/{target}", s.benchmarkChatTarget)
	api.HandleFunc("POST /api/v1/domain/benchmark-preflight", s.benchmarkPreflight)
	api.HandleFunc("POST /api/v1/conversations/{conversation}/benchmark-suggestions/{run}/preview", s.previewBenchmarkSuggestion)
	api.HandleFunc("POST /api/v1/domain/benchmark-admission/preview", s.benchmarkAdmissionPreview)
	api.HandleFunc("POST /api/v1/domain/benchmark-admission/preview/{id}/confirm", s.confirmBenchmarkAdmission)
	api.HandleFunc("GET /api/v1/domain/benchmark-profiles/{key}/draft", s.readBenchmarkDraft)
	api.HandleFunc("PUT /api/v1/domain/benchmark-profiles/{key}/draft", s.saveBenchmarkDraft)
	api.HandleFunc("POST /api/v1/domain/benchmark-profiles/{key}/draft/publish", s.publishBenchmarkDraft)
	api.HandleFunc("POST /api/v1/domain/benchmark-profiles/{key}/draft/start-from-published", s.startBenchmarkDraftFromPublished)
	api.HandleFunc("POST /api/v1/domain/benchmark-profiles/{key}/draft/preview-parameter-edit", s.previewBenchmarkDraftParameter)
	api.HandleFunc("POST /api/v1/domain/benchmark-profiles/{key}/draft/preview-dependency-edit", s.previewBenchmarkDraftDependency)
	api.HandleFunc("POST /api/v1/domain/benchmark-profiles/{key}/draft/preview-structured-edit", s.previewBenchmarkDraftStructuredEdit)
	api.HandleFunc("PUT /api/v1/domain/benchmark-chat-targets/{target}", s.setBenchmarkChatTarget)
	api.HandleFunc("DELETE /api/v1/domain/benchmark-chat-targets/{target}", s.clearBenchmarkChatTarget)
	api.HandleFunc("POST /api/v1/domain/benchmarks", s.submitBenchmark)
	api.HandleFunc("GET /api/v1/domain/benchmarks/{id}", s.benchmarkProgress)
	api.HandleFunc("GET /api/v1/domain/benchmarks/{id}/report", s.benchmarkReport)
	api.HandleFunc("POST /api/v1/domain/benchmarks/{id}/cases/{step}/execution-control", s.benchmarkExecutionControl)
	api.HandleFunc("GET /api/v1/domain/benchmarks/{id}/revision", s.benchmarkRevision)
	api.HandleFunc("POST /api/v1/domain/benchmarks/{id}/revision/preview", s.reviseBenchmark(false))
	api.HandleFunc("POST /api/v1/domain/benchmarks/{id}/revision/proposal", s.proposeBenchmarkRevision)
	api.HandleFunc("POST /api/v1/domain/benchmarks/{id}/revision/ai-preview", s.proposeBenchmarkRevisionAI)
	api.HandleFunc("POST /api/v1/domain/benchmarks/{id}/revision", s.reviseBenchmark(true))
	api.HandleFunc("GET /api/v1/domain/benchmarks/{id}/evidence", s.benchmarkEvidenceInventory)
	api.HandleFunc("GET /api/v1/domain/benchmarks/{id}/evidence/{artifact}", s.benchmarkEvidence)
	api.HandleFunc("POST /api/v1/domain/discovery/authorizations", s.grantDiscovery)
	api.HandleFunc("POST /api/v1/domain/discovery/plans", s.publishDiscovery)
	api.HandleFunc("DELETE /api/v1/domain/discovery/plans/{id}", s.stopDiscovery)
	api.HandleFunc("GET /api/v1/domain/discovery/plans/{id}/observations", s.discoveryObservationHistory)
	api.HandleFunc("GET /api/v1/domain/discovery/candidates", s.discoveryCandidates)
	api.HandleFunc("GET /api/v1/domain/discovery/candidates/{id}/observations", s.discoveryCandidateObservations)
	api.HandleFunc("POST /api/v1/domain/discovery/candidates/{id}/admit", s.admitDiscovery)
	api.HandleFunc("POST /api/v1/incidents", s.createIncident)
	api.HandleFunc("POST /api/v1/loans", s.lendAsset)
	api.HandleFunc("POST /api/v1/loans/return", s.returnAsset)
	api.HandleFunc("GET /api/v1/tasks", s.listTasks)
	api.HandleFunc("POST /api/v1/tasks", s.submit(true))
	api.HandleFunc("GET /api/v1/tasks/{id}", s.detail(true))
	api.HandleFunc("GET /api/v1/tasks/{id}/runs", s.taskRuns)
	api.HandleFunc("GET /api/v1/tasks/{id}/manifest", s.taskManifest)
	api.HandleFunc("POST /api/v1/runtime/reclaim", s.reclaimLeases)
	api.HandleFunc("POST /api/v1/authorizations/{id}/revoke", s.revokeAuthorization)
	api.HandleFunc("POST /api/v1/retention-policies", s.saveRetentionPolicy)
	api.HandleFunc("GET /api/v1/settings/data/storage", s.storageSnapshot)
	api.HandleFunc("GET /api/v1/tasks/{id}/runs/{run_id}", s.runRecord(true))
	api.HandleFunc("PATCH /api/v1/tasks/{id}", s.editTask)
	api.HandleFunc("POST /api/v1/tasks/{id}/acceptance", s.accept)
	api.HandleFunc("POST /api/v1/tasks/{id}/publications", s.requestPublication)
	api.HandleFunc("POST /api/v1/publications/{id}/decision", s.decidePublication)
	api.HandleFunc("POST /api/v1/publications/{id}/reconcile", s.reconcilePublication)
	api.HandleFunc("POST /api/v1/tasks/{id}/pause", s.control("pause"))
	api.HandleFunc("POST /api/v1/tasks/{id}/cancel", s.control("stop"))
	api.HandleFunc("POST /api/v1/tasks/{id}/resume", s.resume)
	api.HandleFunc("POST /api/v1/tasks/{id}/ack-read", s.acknowledgeRead)
	api.HandleFunc("GET /api/v1/conversations", func(w http.ResponseWriter, r *http.Request) {
		items, err := s.Store.Conversations(r.URL.Query().Get("archived") == "1")
		if err != nil {
			writeError(w, err)
			return
		}
		jsonResponse(w, 200, items)
	})
	api.HandleFunc("POST /api/v1/conversations", s.submit(false))
	api.HandleFunc("GET /api/v1/conversations/{id}", s.detail(false))
	api.HandleFunc("GET /api/v1/conversations/history", s.conversationHistory)
	api.HandleFunc("GET /api/v1/conversations/{id}/history", s.messageHistory)
	api.HandleFunc("GET /api/v1/conversations/{id}/events", s.conversationEvents)
	api.HandleFunc("GET /api/v1/conversations/{id}/skill-binding", s.readConversationSkillSelection)
	api.HandleFunc("PUT /api/v1/conversations/{id}/skill-binding", s.setConversationSkillSelection)
	api.HandleFunc("GET /api/v1/conversations/{id}/mcp-selection", s.readConversationMCPSelection)
	api.HandleFunc("PUT /api/v1/conversations/{id}/mcp-selection", s.setConversationMCPSelection)
	api.HandleFunc("POST /api/v1/conversations/{id}/cancel", s.cancelConversation)
	api.HandleFunc("PATCH /api/v1/conversations/{id}", s.changeConversation)
	api.HandleFunc("DELETE /api/v1/conversations/{id}", s.deleteConversation)
	api.HandleFunc("POST /api/v1/conversations/{id}/messages", s.submit(false))
	api.HandleFunc("POST /api/v1/conversations/{id}/benchmark-revision-preview", s.benchmarkRevisionChat)
	api.HandleFunc("POST /api/v1/conversations/{id}/benchmark-execution-suggestions/{run}/confirm", s.confirmBenchmarkExecutionSuggestion)
	api.HandleFunc("GET /api/v1/conversations/{id}/benchmark-revision-requests/{request_id}", s.benchmarkRevisionRequestState)
	api.HandleFunc("GET /api/v1/conversations/{id}/benchmark-revision-preview/{run_id}", s.reopenBenchmarkRevisionChat)
	api.HandleFunc("POST /api/v1/conversations/{id}/benchmark-revision-preview/{run_id}/confirm", s.confirmBenchmarkRevisionChat)
	api.HandleFunc("POST /api/v1/conversations/{id}/resume", s.resumeConversation)
	api.HandleFunc("POST /api/v1/conversations/{id}/task", s.submit(true))
	api.HandleFunc("GET /api/v1/runs/{id}", s.runRecord(false))
	api.HandleFunc("POST /api/v1/conversations/{id}/isolated-replies", s.submitIsolatedReply)
	api.HandleFunc("GET /api/v1/runs/{id}/skill-binding", s.runSkillBinding)
	api.HandleFunc("GET /api/v1/runs/{id}/mcp-bindings", s.runMCPBindings)
	api.HandleFunc("GET /api/v1/runs/{id}/events", s.stream)
	api.HandleFunc("GET /api/v1/artifacts", func(w http.ResponseWriter, r *http.Request) {
		filter := r.URL.Query()
		taskID, after := filter.Get("task_id"), filter.Get("after")
		if (taskID != "" && !validID(taskID)) || (after != "" && !validID(after)) {
			fail(w, 400, "invalid_artifact_filter", "成果筛选参数无效")
			return
		}
		items, err := s.Store.Artifacts(taskID, filter.Get("scope_label"), after)
		if err != nil {
			writeError(w, err)
			return
		}
		jsonResponse(w, 200, items)
	})
	api.HandleFunc("GET /api/v1/artifacts/{id}", func(w http.ResponseWriter, r *http.Request) {
		a, err := s.Store.Artifact(r.PathValue("id"))
		if err != nil {
			writeError(w, err)
			return
		}
		jsonResponse(w, 200, a)
	})
	api.HandleFunc("GET /api/v1/artifacts/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		a, err := s.Store.Artifact(r.PathValue("id"))
		if err != nil {
			writeError(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.Name}))
		io.WriteString(w, a.Content)
	})
	api.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "not_found", "接口不存在") })

	fileServer := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		allowed := false
		for _, host := range s.Hosts {
			if r.Host == host {
				allowed = true
				break
			}
		}
		if !allowed {
			fail(w, 403, "invalid_host", "请使用启动时显示的本机地址")
			return
		}
		if r.URL.Path == "/healthz" {
			jsonResponse(w, 200, map[string]string{"status": "ok"})
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/v1/provider/benchmark-events" {
			s.benchmarkEvent(w, r)
			return
		}

		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
			if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("Origin") != "http://"+r.Host {
				fail(w, 403, "invalid_origin", "请求来源无效")
				return
			}
			if r.URL.Path != "/api/v1/session" && !s.authenticated(r) {
				fail(w, 401, "unauthorized", "请先登录")
				return
			}
			api.ServeHTTP(w, r)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			if info, err := fs.Stat(assets, strings.TrimPrefix(r.URL.Path, "/")); err != nil || info.IsDir() {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			fileServer.ServeHTTP(w, r)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		valid := r.URL.Path == "/" || r.URL.Path == "/tasks" || r.URL.Path == "/chat" || r.URL.Path == "/artifacts" ||
			r.URL.Path == "/memory" || r.URL.Path == "/memory/new" || r.URL.Path == "/memory/candidates" ||
			r.URL.Path == "/memory/skill-proposals" || r.URL.Path == "/settings" ||
			r.URL.Path == "/connections" || r.URL.Path == "/connections/new" ||
			(len(parts) == 2 && (parts[0] == "tasks" || parts[0] == "chat" || parts[0] == "artifacts" || parts[0] == "memory" || parts[0] == "connections") && validID(parts[1]))
		if len(parts) == 3 && parts[0] == "memory" && parts[1] == "new" {
			messageID, err := strconv.ParseInt(parts[2], 10, 64)
			valid = err == nil && messageID > 0
		}
		if !valid {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		index, err := fs.ReadFile(assets, "index.html")
		if err != nil {
			http.Error(w, "Web build missing", 500)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(index)))
		if r.Method != "HEAD" {
			w.Write(index)
		}
	})
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	return strings.Trim(id, "0123456789abcdef") == ""
}

func (s Server) signature(expiry string) string {
	mac := hmac.New(sha256.New, []byte(s.Token))
	mac.Write([]byte("session:" + expiry))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s Server) authenticated(r *http.Request) bool {
	c, err := r.Cookie("clusbot_session")
	if err != nil {
		return false
	}
	expiry, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.signature(expiry))) {
		return false
	}
	t, err := strconv.ParseInt(expiry, 10, 64)
	return err == nil && time.Now().Unix() < t
}

func (s Server) login(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !hmac.Equal([]byte(input.Token), []byte(s.Token)) {
		fail(w, 401, "unauthorized", "访问口令不正确")
		return
	}
	expiry := strconv.FormatInt(time.Now().Add(12*time.Hour).Unix(), 10)
	http.SetCookie(w, &http.Cookie{Name: "clusbot_session", Value: expiry + "." + s.signature(expiry), Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	jsonResponse(w, 200, map[string]bool{"authenticated": true})
}

func (s Server) submit(task bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in submission
		if !decode(w, r, &in) {
			return
		}
		in.Text = strings.TrimSpace(in.Text)
		in.Scope = strings.TrimSpace(in.Scope)
		if in.Filename != nil {
			*in.Filename = strings.TrimSpace(*in.Filename)
		}
		if in.Criteria != nil {
			*in.Criteria = strings.TrimSpace(*in.Criteria)
			if utf8.RuneCountInString(*in.Criteria) > 1000 {
				fail(w, 400, "invalid_criteria", "验收条件最多 1000 字")
				return
			}
		}
		key := in.RequestID
		if key == "" {
			key = in.MessageID
		}
		if in.Text == "" || utf8.RuneCountInString(in.Text) > 16000 || len(key) < 8 || len(key) > 128 || utf8.RuneCountInString(in.Scope) > 40 || len(in.AttachmentIDs) > work.MaxMaterials {
			fail(w, 400, "invalid_submission", "请输入有效的工作要求或消息")
			return
		}
		for _, key := range in.AttachmentIDs {
			if !validID(key) {
				fail(w, 400, "invalid_material", "材料标识无效")
				return
			}
		}
		if in.TaskRef != nil && (task || !validID(r.PathValue("id")) || !validID(in.TaskRef.TaskID) ||
			in.BenchmarkExecutionTarget != nil || in.BenchmarkRevisionTarget != nil || in.InterpretBenchmarkExecution ||
			in.Read != nil || len(in.Reads) > 0 || in.Criteria != nil || in.Filename != nil || in.ReplaceMaterials || in.Revision != 0) {
			fail(w, 400, "invalid_task_reference", "只读任务引用只能用于当前会话的普通消息")
			return
		}
		if in.InterpretBenchmarkExecution && in.BenchmarkExecutionTarget == nil {
			benchmarkError(w, work.ErrBenchmarkInput)
			return
		}
		if target := in.BenchmarkExecutionTarget; target != nil {
			conversation := r.PathValue("id")
			if task || !validID(conversation) || !validID(target.TaskID) || in.BenchmarkRevisionTarget != nil ||
				len(in.AttachmentIDs) != 0 || in.Read != nil || len(in.Reads) != 0 || in.Criteria != nil ||
				in.Filename != nil || in.Revision != 0 || in.ReplaceMaterials {
				benchmarkError(w, work.ErrBenchmarkInput)
				return
			}
			var run work.Run
			var err error
			if in.InterpretBenchmarkExecution {
				run, err = s.Store.SuggestBenchmarkExecutionChat(r.Context(), key, s.principal(r), conversation, in.Text, in.Scope, *target, s.Settings.InterpretBenchmarkExecution)
			} else {
				run, err = s.Store.SubmitBenchmarkExecutionChat(r.Context(), key, s.principal(r), conversation, in.Text, in.Scope, *target)
			}
			if errors.Is(err, work.ErrBenchmarkExecutionModelBinding) {
				fail(w, 409, "benchmark_execution_model_binding_unavailable", err.Error())
				return
			}
			if errors.Is(err, work.ErrBenchmarkExecutionClarify) {
				fail(w, 400, "benchmark_execution_clarify", err.Error())
				return
			}
			if errors.Is(err, work.ErrBenchmarkCaseNotPending) {
				fail(w, 409, "benchmark_case_not_pending", err.Error())
				return
			}
			if err != nil {
				benchmarkError(w, err)
				return
			}
			writeSubmission(w, run)
			return
		}
		if target := in.BenchmarkRevisionTarget; target != nil {
			conversation := r.PathValue("id")
			if task || !validID(conversation) || !validID(target.TaskID) || len(in.AttachmentIDs) != 0 ||
				in.Read != nil || len(in.Reads) != 0 || in.Criteria != nil || in.Filename != nil || in.Revision != 0 || in.ReplaceMaterials {
				benchmarkError(w, work.ErrBenchmarkInput)
				return
			}
			run, err := s.Store.SubmitBenchmarkRevisionSuggestion(r.Context(), key, s.principal(r), conversation, in.Text, in.Scope, *target)
			if err != nil {
				benchmarkError(w, err)
				return
			}
			writeSubmission(w, run)
			return
		}
		if in.TaskRef == nil && !task && len(in.AttachmentIDs) == 0 && in.Read == nil && len(in.Reads) == 0 &&
			in.Criteria == nil && in.Filename == nil && !in.ReplaceMaterials {
			run, handled, err := s.Store.SubmitMemoryCommand(r.Context(), key, in.Text, in.Scope, r.PathValue("id"))
			if handled {
				if err != nil {
					writeError(w, err)
					return
				}
				writeSubmission(w, run)
				return
			}
		}
		if len(in.Reads) > work.MaxTaskReads || (in.Read != nil && len(in.Reads) > 0) {
			writeError(w, work.ErrReadSelection)
			return
		}
		reads := in.Reads
		if in.Read != nil {
			reads = []work.TaskReadRequest{*in.Read}
		}
		for _, read := range reads {
			if !validID(read.ConnectionID) || len(read.ToolName) > 200 || len(read.ToolVersion) != 64 || read.Revision < 1 || read.Arguments == nil {
				writeError(w, mcpclient.ErrArguments)
				return
			}
		}
		delivery := work.Delivery{TaskRef: in.TaskRef, Criteria: in.Criteria, Filename: in.Filename, Revision: in.Revision, Read: in.Read, Reads: in.Reads, ReplaceMaterials: in.ReplaceMaterials}
		if in.TaskRef == nil && !task && delivery.Criteria == nil && delivery.Filename == nil && delivery.Read == nil &&
			len(delivery.Reads) == 0 && !delivery.ReplaceMaterials && len(in.AttachmentIDs) == 0 {
			fields := strings.Fields(in.Text)
			var shortcut func(context.Context, string, string, string, string, string) (work.Run, bool, error)
			if len(fields) > 0 && (fields[0] == "/benchmark" || fields[0] == "/benchmark-status") {
				shortcut = func(ctx context.Context, key, actor, conversation, text, scope string) (work.Run, bool, error) {
					return s.Store.SubmitBenchmarkCommand(ctx, key, actor, conversation, text, scope, s.BenchmarkProvider)
				}
			} else {
				shortcut = s.Store.SubmitBenchmarkSuggestionShortcut
			}
			if run, handled, err := shortcut(r.Context(), key, s.principal(r), r.PathValue("id"), in.Text, in.Scope); handled {
				if err != nil {
					benchmarkError(w, err)
					return
				}
				writeSubmission(w, run)
				return
			}

			if ref := work.AssetAcceptanceQueryRef(in.Text); ref != "" {
				run, err := s.Store.SubmitChatAssetAcceptance(r.Context(), key, s.principal(r), r.PathValue("id"), in.Text, in.Scope, ref)
				if err != nil {
					writeError(w, err)
					return
				}
				writeSubmission(w, run)
				return
			}
		}
		if task || delivery.Criteria != nil || delivery.Filename != nil || len(reads) > 0 || delivery.ReplaceMaterials {
			if err := s.Store.CheckDedicatedFollowupConversation(r.Context(), r.PathValue("id")); err != nil {
				writeError(w, err)
				return
			}
		}
		// A1 持久受理：在可信主体与请求校验之后、模型分类之前提交 Inbox。
		// 同键同请求返回原回执，同键异请求返回冲突；可经 GET /api/v1/admissions/{key} 查询。
		modelAvailable := s.ModelAvailable(r.Context())
		admission, err := s.Store.AdmitChat(r.Context(), key, s.principal(r), r.PathValue("id"), in.Text, in.Scope, task, delivery, in.AttachmentIDs, modelAvailable)
		if err != nil {
			writeError(w, err)
			return
		}
		if admission.Receipt.RunID != "" {
			receipt := admission.Receipt
			if !modelAvailable {
				jsonResponse(w, 202, map[string]string{"receipt_id": receipt.ReceiptID, "task_id": receipt.TaskID, "run_id": receipt.RunID, "conversation_id": receipt.ConversationID})
				return
			}
		}
		// 模型不可用：输入已受理但等待分类，返回带原因回执（不提前 503）。
		if admission.ModelPending {
			jsonResponse(w, 202, map[string]any{
				"receipt_id":    admission.Receipt.ReceiptID,
				"request_key":   admission.Receipt.RequestKey,
				"state":         admission.Receipt.State,
				"duplicate":     admission.Receipt.Duplicate,
				"model_pending": true,
				"reason":        "模型未配置或不可用，输入已受理，待模型可用后分类",
			})
			return
		}
		var run work.Run
		if task {
			run, err = s.Store.SubmitWithDelivery(r.Context(), key, in.Text, in.Scope, r.PathValue("id"), true, delivery, in.AttachmentIDs...)
		} else {
			run, err = s.Store.SubmitRoutedChat(r.Context(), key, in.Text, in.Scope, r.PathValue("id"), delivery, s.Settings.RouteSubmission, in.AttachmentIDs...)
		}
		if err != nil {
			if r.Context().Err() == nil && !errors.Is(err, work.ErrActive) {
				if recordErr := s.Store.FailChatAdmission(r.Context(), key, err.Error()); recordErr != nil {
					slog.Error("记录 Chat 受理失败", "error", recordErr)
				}
			}
			writeError(w, err)
			return
		}
		writeSubmission(w, run)
	}
}

// principal 返回可信主体（来自会话认证，不由请求方自证）。
func (s Server) principal(r *http.Request) string {
	if s.authenticated(r) {
		return "user:session"
	}
	return "user:anonymous"
}

// modelAvailable 报告模型是否可用（不失败，仅用于受理时的挂起判断）。
func (s Server) ModelAvailable(ctx context.Context) bool {
	if s.Settings == nil {
		return false
	}
	value, err := s.Settings.View(ctx)
	return err == nil && value.Mode != "unconfigured"
}

func writeSubmission(w http.ResponseWriter, run work.Run) {
	response := map[string]any{"task_id": run.TaskID, "conversation_id": run.ConversationID, "run_id": run.ID, "status": run.Status}
	if run.RouterIntents != nil {
		response["router_intents"] = run.RouterIntents
	}
	if run.BenchmarkExecutionSuggestion != nil {
		response["benchmark_execution_suggestion"] = run.BenchmarkExecutionSuggestion
	}
	if run.BenchmarkExecutionControl != nil {
		response["benchmark_execution_control"] = run.BenchmarkExecutionControl
	}
	if run.BenchmarkRevisionSuggestion != nil {
		response["benchmark_revision_suggestion"] = run.BenchmarkRevisionSuggestion
	}
	if run.BenchmarkSuggestion != nil {
		response["benchmark_suggestion"] = run.BenchmarkSuggestion
	}
	if run.FollowupQueued {
		response["followup_queued"] = true
	}
	jsonResponse(w, 202, response)
}

func (s Server) pendingAdmissions(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.PendingChatAdmissions(r.Context(), s.principal(r))
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, items)
}

// admission 查询一个持久受理回执（A1：可查询回执，含等待原因与后续 Task 关联）。
func (s Server) admission(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if len(key) < 8 || len(key) > 128 {
		fail(w, 400, "invalid_admission_key", "受理标识无效")
		return
	}
	receipt, err := s.Store.ReadAdmission(r.Context(), key)
	if err != nil {
		fail(w, 404, "admission_not_found", err.Error())
		return
	}
	jsonResponse(w, 200, map[string]any{
		"receipt_id":      receipt.ReceiptID,
		"request_key":     receipt.RequestKey,
		"state":           receipt.State,
		"task_id":         receipt.TaskID,
		"conversation_id": receipt.ConversationID,
		"reason":          receipt.Reason,
	})
}

// submitAssetAcceptance 受理一个资产验收状态只读查询（A2：Chat 明确意图 → V3 Task/Step）。
// 支持两种输入：JSON object_ref 直接指定，或 text 中的明确"查询某资产验收状态"意图。
func (s Server) submitAssetAcceptance(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RequestID string `json:"client_request_id"`
		ObjectRef string `json:"object_ref"`
		Text      string `json:"text"`
	}
	if !decode(w, r, &in) {
		return
	}
	objectRef := strings.TrimSpace(in.ObjectRef)
	if objectRef == "" {
		objectRef = work.AssetAcceptanceQueryRef(in.Text)
	}
	if objectRef == "" {
		fail(w, 400, "invalid_asset_query", "请提供资产标识（object_ref）或明确的验收状态查询意图")
		return
	}
	if len(in.RequestID) < 8 || len(in.RequestID) > 128 {
		fail(w, 400, "invalid_submission", "需要有效的请求标识")
		return
	}
	task, err := s.Store.SubmitAssetAcceptanceQuery(r.Context(), in.RequestID, objectRef, s.principal(r))
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 202, map[string]any{
		"task_id": task.TaskID, "object_ref": task.ObjectRef, "duplicate": task.Duplicate,
	})
}

// assetAcceptanceProgress 查询资产验收状态查询任务的进度与结果（A3）。
// 任务完成必须有持久结果；回执和进度能追溯到同一任务。
func (s Server) assetAcceptanceProgress(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")
	if !validID(taskID) {
		fail(w, 400, "invalid_task", "任务标识无效")
		return
	}
	progress, err := s.Store.ReadAssetAcceptanceQuery(r.Context(), taskID, s.principal(r))
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, 404, "query_not_found", "任务不存在或无权访问")
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, progress)
}

func (s Server) submitDomainTicket(w http.ResponseWriter, r *http.Request) {
	if s.TicketProvider == nil {
		fail(w, 503, "ticket_provider_unavailable", work.ErrTicketProviderUnavailable.Error())
		return
	}
	var in struct {
		Key       string `json:"client_request_id"`
		ObjectRef string `json:"object_ref"`
		Evidence  string `json:"fault_evidence"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.ObjectRef = strings.TrimSpace(in.ObjectRef)
	in.Evidence = strings.TrimSpace(in.Evidence)
	if len(in.Key) < 8 || len(in.Key) > 128 || in.ObjectRef == "" || len(in.ObjectRef) > 256 || in.Evidence == "" || len(in.Evidence) > 8000 {
		fail(w, 400, "invalid_ticket", "请输入资产标识、故障证据和有效请求标识")
		return
	}
	result, err := s.Store.QueueTicketCreate(r.Context(), work.TicketCreateRequest{
		RequestKey: in.Key, ObjectRef: in.ObjectRef, FaultEvidence: in.Evidence, Subject: s.principal(r),
	}, s.TicketProvider)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 202, result)
}

func (s Server) domainTicketProgress(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("id")
	if !validID(key) {
		fail(w, 400, "invalid_operation", "操作标识无效")
		return
	}
	result, err := s.Store.ReadTicketOperation(r.Context(), key, s.principal(r))
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, result)
}

func (s Server) detail(task bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d, err := s.Store.Detail(r.PathValue("id"), task)
		if err != nil {
			writeError(w, err)
			return
		}
		jsonResponse(w, 200, d)
	}
}

func (s Server) taskRuns(w http.ResponseWriter, r *http.Request) {
	taskID, after := r.PathValue("id"), r.URL.Query().Get("after")
	if !validID(taskID) || (after != "" && !validID(after)) {
		fail(w, 400, "invalid_run_filter", "执行记录筛选参数无效")
		return
	}
	page, err := s.Store.TaskRuns(r.Context(), taskID, after)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, page)
}

func (s Server) runRecord(scoped bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID, taskID := r.PathValue("id"), ""
		if scoped {
			runID, taskID = r.PathValue("run_id"), runID
		}
		record, err := s.Store.RunRecord(r.Context(), runID, taskID)
		if err != nil {
			writeError(w, err)
			return
		}
		jsonResponse(w, 200, record)
	}
}

func (s Server) editTask(w http.ResponseWriter, r *http.Request) {
	var edit work.TaskEdit
	if !decode(w, r, &edit) {
		return
	}
	task, err := s.Store.EditTask(r.Context(), r.PathValue("id"), edit)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, task)
}

func (s Server) accept(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ArtifactID string `json:"artifact_id"`
		Revision   int    `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.Store.Accept(r.Context(), r.PathValue("id"), in.ArtifactID, in.Revision); err != nil {
		writeError(w, err)
		return
	}
	s.detail(true)(w, r)
}

func (s Server) overview(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := work.OverviewFilter{Scope: query.Get("scope_label"), Query: query.Get("q"), After: make(map[string]work.TaskCursor)}
	if value := query.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > work.TaskPageLimit {
			fail(w, 400, "invalid_task_filter", "每页事项数量无效")
			return
		}
		filter.Limit = limit
	}
	for _, group := range []string{"waiting", "active", "completed"} {
		if value := query.Get(group + "_after"); value != "" {
			cursor, ok := parseTaskCursor(value)
			if !ok {
				fail(w, 400, "invalid_cursor", "事项游标无效")
				return
			}
			filter.After[group] = cursor
		}
	}
	snapshot, err := s.Store.Overview(r.Context(), filter)
	if err != nil {
		writeError(w, err)
		return
	}
	pending, err := s.Store.PendingSkillProposals(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, struct {
		work.OverviewSnapshot
		PendingSkillProposals int `json:"pending_skill_proposals"`
	}{snapshot, pending})
}

func parseTaskCursor(value string) (work.TaskCursor, bool) {
	stamp, id, ok := strings.Cut(value, ":")
	timestamp, err := strconv.ParseInt(stamp, 10, 64)
	return work.TaskCursor{Timestamp: timestamp, ID: id}, ok && err == nil && timestamp >= 0 && validID(id)
}

func (s Server) listTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := work.TaskFilter{Scope: q.Get("scope_label"), Query: q.Get("q"), Status: q.Get("status")}
	switch filter.Status {
	case "", "active", "waiting_user", "interrupted", "completed", "cancelled":
	default:
		fail(w, 400, "invalid_task_filter", "事项状态无效")
		return
	}
	if value := q.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > work.TaskPageLimit {
			fail(w, 400, "invalid_task_filter", "每页事项数量无效")
			return
		}
		filter.Limit = limit
	}
	if value := q.Get("after"); value != "" {
		cursor, ok := parseTaskCursor(value)
		if !ok {
			fail(w, 400, "invalid_cursor", "事项游标无效")
			return
		}
		filter.AfterUpdatedAt, filter.AfterID = cursor.Timestamp, cursor.ID
	}
	items, err := s.Store.ListTasks(r.Context(), filter)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 200, items)
}

func (s Server) stream(w http.ResponseWriter, r *http.Request) {
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("after")
	}
	after, err := strconv.ParseInt(cursor, 10, 64)
	if cursor != "" && (err != nil || after < 0) {
		fail(w, 400, "invalid_cursor", "事件游标无效")
		return
	}
	key := r.PathValue("id")
	events, run, err := s.Store.Events(key, after)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		for _, e := range events {
			if _, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Kind, e.Data); err != nil {
				return
			}
			after = e.Seq
		}
		if len(events) < 128 && run.Status != "queued" && run.Status != "running" {
			fmt.Fprint(w, "event: done\ndata: {}\n\n")
			rc.Flush()
			return
		}
		if err = rc.Flush(); err != nil {
			return
		}
		if len(events) < 128 {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
			case <-ping.C:
				fmt.Fprint(w, ": heartbeat\n\n")
			}
		}
		events, run, err = s.Store.Events(key, after)
		if err != nil {
			return
		}
	}
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	t, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if t != "application/json" {
		fail(w, 415, "invalid_content_type", "需要 JSON 请求")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		fail(w, 400, "invalid_json", "请求内容格式无效")
		return false
	}
	if decoder.Decode(new(any)) != io.EOF {
		fail(w, 400, "invalid_json", "请求内容格式无效")
		return false
	}
	return true
}

func jsonResponse(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
func fail(w http.ResponseWriter, status int, code, message string) {
	jsonResponse(w, status, map[string]string{"code": code, "message": message})
}
func writeError(w http.ResponseWriter, err error) {
	var invalidEdit work.TaskEditError
	switch {
	case errors.As(err, &invalidEdit):
		fail(w, 400, "invalid_task_edit", invalidEdit.Error())
	case errors.Is(err, work.ErrTaskRevision):
		fail(w, 409, "task_revision", err.Error())
	case errors.Is(err, work.ErrImageSize):
		fail(w, 413, "image_too_large", err.Error())
	case errors.Is(err, work.ErrImageFormat), errors.Is(err, work.ErrImageUnsupported):
		fail(w, 400, "invalid_image", err.Error())
	case errors.Is(err, work.ErrUploadSize):
		fail(w, 413, "upload_too_large", err.Error())
	case errors.Is(err, work.ErrUpload), errors.Is(err, work.ErrUploadPart):
		fail(w, 400, "invalid_upload", err.Error())
	case errors.Is(err, work.ErrUploadChanged):
		fail(w, 409, "upload_changed", err.Error())
	case errors.Is(err, work.ErrConversationTitle):
		fail(w, 400, "invalid_conversation_title", err.Error())
	case errors.Is(err, work.ErrConversationArchived), errors.Is(err, work.ErrConversationRevision), errors.Is(err, work.ErrReadUnconfirmed):
		fail(w, 409, "conversation_conflict", err.Error())
	case errors.Is(err, sql.ErrNoRows):
		fail(w, 404, "not_found", "记录不存在")
	case errors.Is(err, work.ErrActive):
		fail(w, 409, "run_active", err.Error())
	case errors.Is(err, work.ErrClassificationUnconfirmed):
		fail(w, 502, "classification_unconfirmed", err.Error())
	case errors.Is(err, work.ErrClassification):
		fail(w, 502, "classification_failed", err.Error())
	case errors.Is(err, work.ErrConversationBindingConflict):
		fail(w, 409, "conversation_binding_conflict", err.Error())
	case errors.Is(err, work.ErrDiscoveryRecoveryUnsupported):
		fail(w, 409, "discovery_recovery_unsupported", err.Error())
	case errors.Is(err, work.ErrDeterministicTaskRecoveryUnsupported):
		fail(w, http.StatusConflict, "deterministic_task_recovery_unsupported", err.Error())
	case errors.Is(err, work.ErrBenchmarkRecoveryUnsupported):
		fail(w, 409, "benchmark_recovery_unsupported", err.Error())
	case errors.Is(err, work.ErrApprovalExpired):
		fail(w, 409, "approval_expired", err.Error())
	case errors.Is(err, mcpclient.ErrConfig), errors.Is(err, mcpclient.ErrArguments), errors.Is(err, mcpclient.ErrToolSchema), errors.Is(err, work.ErrReadSelection):
		fail(w, 400, "invalid_mcp", err.Error())
	case errors.Is(err, work.ErrToolPermission):
		fail(w, 403, "tool_denied", err.Error())
	case errors.Is(err, work.ErrConnectionRevision), errors.Is(err, work.ErrConnectionUnavailable), errors.Is(err, work.ErrConnectionDuplicate), errors.Is(err, mcpclient.ErrSchemaChanged), errors.Is(err, work.ErrReadNeedsSelection):
		fail(w, 409, "connection_conflict", err.Error())
	case errors.Is(err, mcpclient.ErrConnect), errors.Is(err, mcpclient.ErrDiscovery), errors.Is(err, mcpclient.ErrCredentials), errors.Is(err, mcpclient.ErrCall), errors.Is(err, mcpclient.ErrToolInput):
		fail(w, 502, "mcp_failed", err.Error())
	case errors.Is(err, work.ErrMemoryInvalid), errors.Is(err, work.ErrMemoryCommandContent):
		fail(w, 400, "invalid_memory", err.Error())
	case errors.Is(err, work.ErrSkillProposal), errors.Is(err, work.ErrSkillProposalConfirmation):
		fail(w, 409, "skill_proposal_conflict", err.Error())
	case errors.Is(err, work.ErrSkillReplayRequired):
		fail(w, 409, "skill_replay_required", err.Error())
	case errors.Is(err, work.ErrExperienceSource), errors.Is(err, work.ErrExperienceRevision):
		fail(w, 409, "experience_conflict", err.Error())
	case errors.Is(err, work.ErrMemoryRevision):
		fail(w, 409, "memory_revision", err.Error())
	case errors.Is(err, work.ErrMemoryDuplicate), errors.Is(err, work.ErrMemoryLimit):
		fail(w, 409, "memory_conflict", err.Error())
	case errors.Is(err, work.ErrMaterial), errors.Is(err, work.ErrMaterialSelection):
		fail(w, 400, "invalid_material", err.Error())
	case errors.Is(err, work.ErrMaterialsChanged):
		fail(w, 409, "material_changed", err.Error())
	case errors.Is(err, work.ErrFilename):
		fail(w, 400, "invalid_filename", err.Error())
	case errors.Is(err, work.ErrOverwriteTarget), errors.Is(err, work.ErrPublicationOpen), errors.Is(err, work.ErrWorkspace):
		fail(w, 409, "publication_conflict", err.Error())
	case errors.Is(err, work.ErrDeliveryPending), errors.Is(err, work.ErrDeliveryChanged), errors.Is(err, work.ErrWorkflowEvidence):
		fail(w, 409, "delivery_incomplete", err.Error())
	case errors.Is(err, work.ErrAuthorizationDenied):
		fail(w, 403, "forbidden", err.Error())
	case errors.Is(err, work.ErrConflict), errors.Is(err, work.ErrState), errors.Is(err, work.ErrReadCheckpoint):
		fail(w, 409, "conflict", err.Error())
	default:
		slog.Error("request failed", "error", err)
		fail(w, 500, "internal_error", "请求失败，请重试")
	}
}

func (s Server) control(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Revision int `json:"revision"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := s.Store.Control(r.Context(), r.PathValue("id"), action, in.Revision); err != nil {
			writeError(w, err)
			return
		}
		s.detail(true)(w, r)
	}
}

func (s Server) resume(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision  int    `json:"revision"`
		RequestID string `json:"client_request_id"`
		Retry     bool   `json:"retry"`
		Reassign  bool   `json:"reassign"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.RequestID) < 8 || len(in.RequestID) > 128 || (in.Retry && in.Reassign) {
		fail(w, 400, "invalid_submission", "需要有效的请求标识")
		return
	}
	if err := s.Store.CheckGenericTaskRecovery(r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	if !s.modelReady(w, r) {
		return
	}
	var run work.Run
	var err error
	if in.Reassign {
		run, err = s.Store.ReassignTask(r.Context(), r.PathValue("id"), in.RequestID, in.Revision)
	} else if in.Retry {
		run, err = s.Store.RetryTask(r.Context(), r.PathValue("id"), in.RequestID, in.Revision)
	} else {
		run, err = s.Store.Resume(r.Context(), r.PathValue("id"), in.RequestID, in.Revision)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 202, map[string]string{"task_id": run.TaskID, "conversation_id": run.ConversationID, "run_id": run.ID, "status": run.Status})
}

func (s Server) resumeConversation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RunID     string `json:"run_id"`
		Revision  int    `json:"revision"`
		RequestID string `json:"client_request_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !validID(in.RunID) || in.Revision < 1 || len(in.RequestID) < 8 || len(in.RequestID) > 128 {
		fail(w, 400, "invalid_submission", "需要有效的对话执行记录与请求标识")
		return
	}
	if !s.modelReady(w, r) {
		return
	}
	run, err := s.Store.ResumeConversation(r.Context(), r.PathValue("id"), in.RunID, in.RequestID, in.Revision)
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, 202, map[string]string{"task_id": run.TaskID, "conversation_id": run.ConversationID, "run_id": run.ID, "status": run.Status})
}
