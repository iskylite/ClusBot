package httpapi

import (
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"clusbot/internal/work"
)

// This receipt describes only a destination and locally resolved identifiers.
// It is never a write request, and it deliberately cannot contain user text.
type assistantSetupPrefill struct {
	Role             string `json:"role,omitempty"`
	ProfileID        string `json:"profile_id,omitempty"`
	ProfileRevision  int    `json:"profile_revision,omitempty"`
	ProviderID       string `json:"provider_id,omitempty"`
	ProviderRevision int    `json:"provider_revision,omitempty"`
}

type assistantSetupForm struct {
	State   string                `json:"state"`
	Tool    string                `json:"tool"`
	Form    string                `json:"form"`
	Prefill assistantSetupPrefill `json:"prefill"`
}

var assistantSetupRoles = map[string][]string{
	work.GeneralRouterModelRole:      {"通用路由", "general_router"},
	work.ConversationReplyModelRole:  {"对话回复", "conversation_reply"},
	work.TaskWorkerModelRole:         {"任务执行", "task_worker"},
	work.BenchmarkAdmissionModelRole: {"基准受理", "benchmark_admission"},
	work.BenchmarkRevisionModelRole:  {"基准方案修订", "benchmark_revision"},
	work.BenchmarkExecutionModelRole: {"基准执行建议", "benchmark_execution"},
}

var assistantSetupClauses = regexp.MustCompile(`[，,；;。！!？?\n]|顺便|另外|同时`)

func setupContainsAny(text string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}

// ASCII identifier characters must end at an identifier boundary, so a
// profile named "Qwen" does not match the distinct "Qwen-Long" identifier.
// CJK text naturally adjoins particles and verbs and is matched literally.
func assistantSetupMention(text, value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	text = strings.ToLower(text)
	if value == "" {
		return false
	}
	identifier := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("_-./", r)
	}
	for offset := 0; offset < len(text); {
		i := strings.Index(text[offset:], value)
		if i < 0 {
			return false
		}
		i += offset
		end := i + len(value)
		first, _ := utf8.DecodeRuneInString(value)
		last, _ := utf8.DecodeLastRuneInString(value)
		before, _ := utf8.DecodeLastRuneInString(text[:i])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if !(identifier(first) && identifier(before)) && !(identifier(last) && identifier(after)) {
			return true
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		offset = i + size
	}
	return false
}

// assistantModelSetupChat catches explicit settings requests before a model
// sees their text. Resolution uses only the authenticated local catalog; no
// guessed names, credentials, URLs or direct write actions enter the receipt.
func (s Server) assistantModelSetupChat(w http.ResponseWriter, r *http.Request, text string) bool {
	var roleClauses, providerClauses []string
	for _, clause := range assistantSetupClauses.Split(strings.ToLower(text), -1) {
		negative := setupContainsAny(clause, "不要", "不用", "不需要", "不想", "别", "暂不", "无需", "不切", "不换", "不改", "不修改", "不绑定", "不调整", "不设", "不配", "不使用", "不选", "不填", "不补", "不加", "不录", "不更换", "不更新", "不编辑", "如果", "假如", "除非", "将来", "稍后", "don't", "do not", "not switch", "not change", "without", "unless")
		change := setupContainsAny(clause, "切", "换", "改", "调整", "绑定", "设", "配", "使用", "选用", "用", "change", "switch", "bind", "set ", "configure")
		credential := setupContainsAny(clause, "凭据", "密钥", "api key", "api_key", "apikey", "credential")
		if negative && credential && assistantCredentialValue.MatchString(clause) {
			fail(w, http.StatusBadRequest, "sensitive_input", "请不要在对话中发送凭据；需要修改时请使用可信表单。")
			return true
		}
		providerChange := setupContainsAny(clause, "补", "填", "录", "加", "配置", "设置", "编辑", "更新", "更换", "修改", "修复", "维护", "换", "改", "set ", "update", "configure", "add ")
		roleMention := setupContainsAny(clause, "角色绑定", "模型角色", "角色路由")
		for _, names := range assistantSetupRoles {
			for _, name := range names {
				roleMention = roleMention || assistantSetupMention(clause, name)
			}
		}
		if !negative && change && roleMention {
			roleClauses = append(roleClauses, clause)
		}
		if !negative && credential && (providerChange || assistantCredentialValue.MatchString(clause)) && !setupContainsAny(clause, "mcp", "密码", "password", "passwd") {
			providerClauses = append(providerClauses, clause)
		}
	}
	if len(roleClauses) == 0 && len(providerClauses) == 0 {
		return false
	}
	account, hasAccount := accountFromContext(r)
	if !s.assistantFormAllowed(r, "configure_model_role") || !s.assistantFormAllowed(r, "configure_model_provider") ||
		!hasAccount || account.MustChangePassword {
		fail(w, http.StatusForbidden, "forbidden", "只有有权管理模型设置的用户可以打开此表单")
		return true
	}
	if s.Settings == nil || s.Settings.Store == nil {
		fail(w, http.StatusServiceUnavailable, "model_settings_unavailable", "模型设置暂不可用")
		return true
	}

	var forms []assistantSetupForm
	if len(roleClauses) != 0 {
		profileViews, err := s.Settings.Profiles(r.Context())
		if err != nil {
			writeModelSettingsError(w, err)
			return true
		}
		roleText := strings.Join(roleClauses, " ")
		roles := map[string]bool{}
		for role, names := range assistantSetupRoles {
			for _, name := range names {
				if assistantSetupMention(roleText, name) {
					roles[role] = true
				}
			}
		}
		prefill := assistantSetupPrefill{}
		if len(roles) == 1 {
			for role := range roles {
				prefill.Role = role
			}
		}
		matches := map[string]assistantSetupPrefill{}
		for _, profile := range profileViews {
			modelID := s.assistantSafeModelID(r.Context(), profile.ID, profile.ModelID)
			if assistantSetupMention(roleText, profile.ID) || assistantSetupMention(roleText, profile.Name) || assistantSetupMention(roleText, modelID) {
				matches[profile.ID] = assistantSetupPrefill{ProfileID: profile.ID, ProfileRevision: profile.Revision, ProviderRevision: profile.ProviderRevision}
			}
		}
		if len(matches) == 1 {
			for _, match := range matches {
				prefill.ProfileID, prefill.ProfileRevision, prefill.ProviderRevision = match.ProfileID, match.ProfileRevision, match.ProviderRevision
			}
		}
		forms = append(forms, assistantSetupForm{State: "form_required", Tool: "role_binding_prefill", Form: "model_role_binding", Prefill: prefill})
	}
	if len(providerClauses) != 0 {
		providers, err := s.Settings.Providers(r.Context())
		if err != nil {
			writeModelSettingsError(w, err)
			return true
		}
		providerText := strings.Join(providerClauses, " ")
		matches := map[string]assistantSetupPrefill{}
		for _, provider := range providers {
			if assistantSetupMention(providerText, provider.ID) || assistantSetupMention(providerText, provider.Name) {
				matches[provider.ID] = assistantSetupPrefill{ProviderID: provider.ID, ProviderRevision: provider.Revision}
			}
		}
		prefill := assistantSetupPrefill{}
		if len(matches) == 1 {
			for _, match := range matches {
				prefill = match
			}
		}
		forms = append(forms, assistantSetupForm{State: "form_required", Tool: "provider_credentials_prefill", Form: "model_provider_credentials", Prefill: prefill})
	}
	w.Header().Set("Cache-Control", "no-store")
	if len(forms) == 1 {
		jsonResponse(w, http.StatusOK, forms[0])
	} else {
		jsonResponse(w, http.StatusOK, struct {
			State string               `json:"state"`
			Items []assistantSetupForm `json:"items"`
		}{State: "batch_completed", Items: forms})
	}
	return true
}
