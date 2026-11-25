package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"testing/fstest"

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
	ctx := t.Context()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	instance, err := work.Open(filepath.Join(root, "instance.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	const origin = "https://ops.example"
	assets := fstest.MapFS{"index.html": {Data: []byte("test")}}
	base := Server{Store: instance, Origin: origin, Hosts: []string{"ops.example"}}
	f := &accountFixture{t: t, store: instance, handler: base.Handler(assets)}
	f.setup()
	admin := f.login("admin", "correct horse battery staple")
	created := f.request(http.MethodPost, "/api/v1/admin/users", map[string]any{
		"username": "alice", "display_name": "Alice", "password": "fixture alice password",
	}, admin)
	if created.Code != http.StatusCreated {
		t.Fatalf("create Alice: %d %s", created.Code, created.Body)
	}
	var user struct{ User work.Account }
	accountJSON(t, created, &user)
	alice := f.login("alice", "fixture alice password")
	actor := user.User.PrincipalID
	personal, err := work.OpenAccountBusinessStore(filepath.Join(root, "alice.sqlite"), instance)
	if err != nil {
		t.Fatal(err)
	}
	defer personal.Close()
	if err := personal.BindAccountBusinessStore(ctx, actor); err != nil {
		t.Fatal(err)
	}
	base.PersonalAPI = func(principal string) (http.Handler, error) {
		if principal != actor {
			return nil, work.ErrAccountDenied
		}
		return base.PersonalHandler(personal), nil
	}
	f.handler = base.Handler(assets)

	chat, handled, err := personal.SubmitBenchmarkCommand(ctx, "existing-chat", actor, "", "跑 POD7 全套基准", "", nil)
	if err != nil || !handled {
		t.Fatalf("existing chat: %+v %v", chat, err)
	}
	draft := map[string]any{"scope_label": "reports", "instructions": "Internal Skill body", "revision": 0}
	if w := f.request(http.MethodPost, "/api/v1/skills", draft, alice); w.Code != http.StatusForbidden {
		t.Fatalf("personal account may not write the shared catalog: %d %s", w.Code, w.Body)
	}
	w := f.request(http.MethodPost, "/api/v1/skills", draft, admin)
	if w.Code != http.StatusCreated {
		t.Fatalf("admin shared draft: %d %s", w.Code, w.Body)
	}
	var v work.SkillVersion
	accountJSON(t, w, &v)
	w = f.request(http.MethodPost, "/api/v1/skills/"+v.SkillID+"/activate",
		map[string]any{"version": v.Version, "revision": v.Revision, "sha256": v.Digest}, admin)
	if w.Code != http.StatusNoContent {
		t.Fatalf("admin shared activation: %d %s", w.Code, w.Body)
	}
	path := "/api/v1/conversations/" + chat.ConversationID + "/skill-binding"
	input := work.ConversationSkillBindingInput{Action: "bind_skill", SkillID: v.SkillID, Version: v.Version}
	if w := f.request(http.MethodPut, path, input, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous can select: %d %s", w.Code, w.Body)
	}
	if w := f.request(http.MethodPut, path, input, alice); w.Code != http.StatusOK {
		t.Fatalf("explicit selection failed: %d %s", w.Code, w.Body)
	}
	w = f.request(http.MethodGet, path, nil, alice)
	var out work.ConversationSkillSelection
	accountJSON(t, w, &out)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" ||
		out.ConversationID != chat.ConversationID || out.Binding == nil || out.Binding.SkillID != v.SkillID || out.Binding.Version != v.Version {
		t.Fatalf("wrong no-model binding: %+v, %d", out, w.Code)
	}
	if out.BindingRevision != 1 {
		t.Fatalf("GET missing authoritative binding revision: %+v", out)
	}
	w = f.request(http.MethodPut, path, map[string]any{"action": "clear_skill", "expected_binding_revision": 0}, alice)
	var conflict map[string]string
	accountJSON(t, w, &conflict)
	if w.Code != http.StatusConflict || conflict["code"] != "conversation_binding_conflict" {
		t.Fatalf("lost actionable HTTP conflict: %d %+v", w.Code, conflict)
	}
	w = f.request(http.MethodPut, path, map[string]any{"action": "clear_skill", "expected_binding_revision": 1}, alice)
	var cleared work.ConversationSkillSelection
	accountJSON(t, w, &cleared)
	if w.Code != http.StatusOK || cleared.Binding != nil || cleared.BindingRevision != 2 {
		t.Fatalf("wrong HTTP clear transition: %d %+v", w.Code, cleared)
	}
}
