package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"clusbot/internal/work"
)

func TestRunSkillBindingUnboundWithoutModel(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "run-skill-api.sqlite"))
	defer a.close()
	run, err := a.store.Submit(t.Context(), "scope", "summarize", "reports", "", true)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/runs/" + run.ID + "/skill-binding"
	req := a.request(http.MethodGet, path, "")
	req.Header.Del("Cookie")
	res, err := a.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unexpected anonymous status %d", res.StatusCode)
	}
	res, err = a.ts.Client().Do(a.request(http.MethodGet, path, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var binding work.RunSkillBinding
	if err := json.NewDecoder(res.Body).Decode(&binding); err != nil || res.StatusCode != http.StatusOK ||
		res.Header.Get("Cache-Control") != "no-store" || binding.RunID != run.ID || binding.Binding != nil {
		t.Fatalf("unbound run was misrepresented: %+v, %d, %v", binding, res.StatusCode, err)
	}
}

func TestRunMCPBindingsUnboundWithoutModel(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "run-mcp-api.sqlite"))
	defer a.close()
	run, err := a.store.Submit(t.Context(), "mcp-scope", "summarize", "reports", "", true)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/runs/" + run.ID + "/mcp-bindings"
	req := a.request(http.MethodGet, path, "")
	req.Header.Del("Cookie")
	res, err := a.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status: %d", res.StatusCode)
	}
	res, err = a.ts.Client().Do(a.request(http.MethodGet, path, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got work.RunMCPBindings
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil || res.StatusCode != http.StatusOK ||
		res.Header.Get("Cache-Control") != "no-store" || got.RunID != run.ID || got.Bindings == nil || len(got.Bindings) != 0 {
		t.Fatalf("unbound run represented incorrectly: %+v, %d, %v", got, res.StatusCode, err)
	}
}

func TestConversationSkillSelectionExplicitAPI(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "conversation-skill-api.sqlite"))
	defer a.close()
	ctx := t.Context()
	const actor = "user:session"
	chat, handled, err := a.store.SubmitBenchmarkCommand(ctx, "existing-chat", actor, "", "跑 POD7 全套基准", "", nil)
	if err != nil || !handled {
		t.Fatalf("existing chat: %+v %v", chat, err)
	}
	v, err := a.store.DraftSkill(ctx, "reports", "Internal Skill body", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.ActivateSkill(ctx, v.SkillID, v.Version, 0, v.Digest); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/conversations/" + chat.ConversationID + "/skill-binding"
	body, err := json.Marshal(work.ConversationSkillBindingInput{Action: "bind_skill", SkillID: v.SkillID, Version: v.Version})
	if err != nil {
		t.Fatal(err)
	}
	req := a.request(http.MethodPut, path, string(body))
	req.Header.Del("Cookie")
	res, err := a.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous can select: %d", res.StatusCode)
	}
	res, err = a.ts.Client().Do(a.request(http.MethodPut, path, string(body)))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("explicit selection failed: %d", res.StatusCode)
	}
	res, err = a.ts.Client().Do(a.request(http.MethodGet, path, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out work.ConversationSkillSelection
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || res.StatusCode != http.StatusOK || res.Header.Get("Cache-Control") != "no-store" ||
		out.ConversationID != chat.ConversationID || out.Binding == nil || out.Binding.SkillID != v.SkillID || out.Binding.Version != v.Version {
		t.Fatalf("wrong no-model binding: %+v, %d, %v", out, res.StatusCode, err)
	}
	if out.BindingRevision != 1 {
		t.Fatalf("GET missing authoritative binding revision: %+v", out)
	}
	rejected, err := a.ts.Client().Do(a.request(http.MethodPut, path, `{"action":"clear_skill","expected_binding_revision":0}`))
	if err != nil {
		t.Fatal(err)
	}
	defer rejected.Body.Close()
	var conflict map[string]string
	if err := json.NewDecoder(rejected.Body).Decode(&conflict); err != nil || rejected.StatusCode != http.StatusConflict || conflict["code"] != "conversation_binding_conflict" {
		t.Fatalf("lost actionable HTTP conflict: %d %+v %v", rejected.StatusCode, conflict, err)
	}
	finally, err := a.ts.Client().Do(a.request(http.MethodPut, path, `{"action":"clear_skill","expected_binding_revision":1}`))
	if err != nil {
		t.Fatal(err)
	}
	defer finally.Body.Close()
	var cleared work.ConversationSkillSelection
	if err := json.NewDecoder(finally.Body).Decode(&cleared); err != nil || finally.StatusCode != http.StatusOK || cleared.Binding != nil || cleared.BindingRevision != 2 {
		t.Fatalf("wrong HTTP clear transition: %d %+v %v", finally.StatusCode, cleared, err)
	}

}
