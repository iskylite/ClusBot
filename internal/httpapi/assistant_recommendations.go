package httpapi

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const assistantRecommendationTool = "propose_operation_options"

type assistantRecommendationDraft struct {
	title, summary string
	options        []assistantRecommendationOption
}

type assistantRecommendationOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Detail      string `json:"detail,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
}

// A selection is deliberately held only for the same lifetime and session as
// its suggestion. It is not an authorization or a request to execute anything.
type assistantRecommendation struct {
	session  string
	expires  time.Time
	options  []assistantRecommendationOption
	selected string
}

type assistantRecommendationResult struct {
	ProposalID string                          `json:"proposal_id"`
	Title      string                          `json:"title"`
	Summary    string                          `json:"summary,omitempty"`
	Options    []assistantRecommendationOption `json:"options"`
	Origin     string                          `json:"origin"`
	Execution  string                          `json:"execution"`
	Expires    int64                           `json:"expires_at_unix_ms"`
}

// Model output is untrusted. Only the enumerated display fields are accepted;
// in particular an option cannot contain an operation, target or action.
func decodeAssistantRecommendation(raw json.RawMessage) (assistantRecommendationDraft, bool) {
	var draft assistantRecommendationDraft
	if len(raw) > 32*1024 || !utf8.Valid(raw) || !assistantUniqueJSON(raw) {
		return draft, false
	}
	outer, ok := assistantJSONObject(raw, "tool", "body")
	if !ok || len(outer) != 2 {
		return draft, false
	}
	tool, ok := assistantJSONString(outer["tool"], 80, true)
	if !ok || tool != assistantRecommendationTool {
		return draft, false
	}
	body, ok := assistantJSONObject(outer["body"], "title", "summary", "options")
	if !ok {
		return draft, false
	}
	if draft.title, ok = assistantJSONString(body["title"], 80, true); !ok {
		return draft, false
	}
	if value, exists := body["summary"]; exists {
		if draft.summary, ok = assistantJSONString(value, 240, false); !ok {
			return draft, false
		}
	}
	var options []json.RawMessage
	if !bytes.HasPrefix(bytes.TrimSpace(body["options"]), []byte("[")) || json.Unmarshal(body["options"], &options) != nil || len(options) < 1 || len(options) > 4 {
		return draft, false
	}
	labels := make(map[string]bool, len(options))
	recommended := false
	for _, value := range options {
		fields, ok := assistantJSONObject(value, "label", "detail", "recommended")
		if !ok {
			return assistantRecommendationDraft{}, false
		}
		var option assistantRecommendationOption
		if option.Label, ok = assistantJSONString(fields["label"], 80, true); !ok || labels[option.Label] {
			return assistantRecommendationDraft{}, false
		}
		labels[option.Label] = true
		if value, exists := fields["detail"]; exists {
			if option.Detail, ok = assistantJSONString(value, 240, false); !ok {
				return assistantRecommendationDraft{}, false
			}
		}
		if value, exists := fields["recommended"]; exists {
			if string(value) != "true" && string(value) != "false" {
				return assistantRecommendationDraft{}, false
			}
			option.Recommended = string(value) == "true"
			if option.Recommended && recommended {
				return assistantRecommendationDraft{}, false
			}
			recommended = recommended || option.Recommended
		}
		draft.options = append(draft.options, option)
	}
	return draft, true
}

func assistantJSONObject(raw json.RawMessage, allowed ...string) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, false
	}
	for key := range fields {
		found := false
		for _, name := range allowed {
			found = found || key == name
		}
		if !found {
			return nil, false
		}
	}
	return fields, true
}

func assistantJSONString(raw json.RawMessage, limit int, required bool) (string, bool) {
	var value string
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte(`"`)) || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	value = strings.TrimSpace(value)
	return value, (!required || value != "") && utf8.RuneCountInString(value) <= limit
}

// encoding/json otherwise silently accepts duplicate object keys. Check every
// nested object before decoding so no field can acquire conflicting meanings.
func assistantUniqueJSON(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if !assistantUniqueJSONValue(decoder) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func assistantUniqueJSONValue(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return true
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] {
				return false
			}
			seen[name] = true
			if !assistantUniqueJSONValue(decoder) {
				return false
			}
		}
		token, err = decoder.Token()
		return err == nil && token == json.Delim('}')
	case '[':
		for decoder.More() {
			if !assistantUniqueJSONValue(decoder) {
				return false
			}
		}
		token, err = decoder.Token()
		return err == nil && token == json.Delim(']')
	default:
		return false
	}
}

func (s Server) issueAssistantRecommendation(w http.ResponseWriter, r *http.Request, draft assistantRecommendationDraft) {
	bridge, ok := r.Context().Value(assistantBridgeKey{}).(*assistantBridge)
	if !ok || bridge == nil {
		fail(w, http.StatusServiceUnavailable, "assistant_unavailable", "对话工具暂不可用")
		return
	}
	session := s.assistantSession(r)
	if session == "" {
		fail(w, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	result, ok := bridge.prepareRecommendation(session, draft)
	if !ok {
		fail(w, http.StatusTooManyRequests, "assistant_preview_busy", "待确认建议过多，请稍后再试")
		return
	}
	jsonResponse(w, http.StatusOK, struct {
		State  string                        `json:"state"`
		Tool   string                        `json:"tool"`
		Result assistantRecommendationResult `json:"result"`
	}{"completed", assistantRecommendationTool, result})
}

func (b *assistantBridge) prepareRecommendation(session string, draft assistantRecommendationDraft) (assistantRecommendationResult, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if b.recommendations == nil {
		b.recommendations = make(map[string]assistantRecommendation)
	}
	for id, item := range b.recommendations {
		if !now.Before(item.expires) {
			delete(b.recommendations, id)
		}
	}
	if len(b.recommendations) >= 128 {
		return assistantRecommendationResult{}, false
	}
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		return assistantRecommendationResult{}, false
	}
	proposalID := hex.EncodeToString(random[:])
	if _, exists := b.recommendations[proposalID]; exists {
		return assistantRecommendationResult{}, false
	}
	options := append([]assistantRecommendationOption(nil), draft.options...)
	optionIDs := make(map[string]bool, len(options))
	for i := range options {
		var randomOption [16]byte
		if _, err := rand.Read(randomOption[:]); err != nil {
			return assistantRecommendationResult{}, false
		}
		options[i].ID = hex.EncodeToString(randomOption[:])
		if optionIDs[options[i].ID] {
			return assistantRecommendationResult{}, false
		}
		optionIDs[options[i].ID] = true
	}
	expires := now.Add(5 * time.Minute)
	b.recommendations[proposalID] = assistantRecommendation{session: session, expires: expires, options: options}
	return assistantRecommendationResult{ProposalID: proposalID, Title: draft.title, Summary: draft.summary, Options: options, Origin: "model", Execution: "not_evaluated", Expires: expires.UnixMilli()}, true
}

func (s Server) assistantRecommendationSelection(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if !decode(w, r, &raw) {
		return
	}
	fields, ok := assistantJSONObject(raw, "option_id")
	optionID, valid := assistantJSONString(fields["option_id"], 32, true)
	if !ok || len(fields) != 1 || !valid || len(optionID) != 32 || !assistantLowerHex(optionID) || !assistantUniqueJSON(raw) {
		fail(w, http.StatusBadRequest, "invalid_assistant_arguments", "请选择有效的建议")
		return
	}
	bridge, ok := r.Context().Value(assistantBridgeKey{}).(*assistantBridge)
	if !ok || bridge == nil {
		fail(w, http.StatusServiceUnavailable, "assistant_unavailable", "对话工具暂不可用")
		return
	}
	proposalID := r.PathValue("id")
	if !bridge.selectRecommendation(proposalID, optionID, s.assistantSession(r)) {
		fail(w, http.StatusConflict, "assistant_recommendation_unavailable", "建议已失效或选择冲突，请重新提出")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{
		"proposal_id": proposalID, "option_id": optionID,
		"state": "confirmed", "execution": "not_evaluated",
	})
}

func assistantLowerHex(value string) bool {
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func (b *assistantBridge) selectRecommendation(id, optionID, session string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	item, found := b.recommendations[id]
	if !found || session == "" || item.session != session || !time.Now().Before(item.expires) {
		return false
	}
	valid := false
	for _, option := range item.options {
		valid = valid || option.ID == optionID
	}
	if !valid || (item.selected != "" && item.selected != optionID) {
		return false
	}
	item.selected = optionID
	b.recommendations[id] = item
	return true
}
