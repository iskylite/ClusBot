package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

type a2uiFixtureSource struct{}

func (a2uiFixtureSource) AllowsDiscovery(source, scope string) bool {
	return source == "fixture-local" && scope == "fixture-private-scope"
}

func (a2uiFixtureSource) ScanDiscovery(_ context.Context, _, _ string) ([]work.DiscoveryObservation, error) {
	return []work.DiscoveryObservation{{Serial: "fixture-private-serial", Address: "192.0.2.184"}}, nil
}

type discoveryA2UIFixture struct {
	account   *accountFixture
	cookie    *http.Cookie
	actor     string
	instance  *work.Store
	store     *work.Store
	path      string
	server    Server
	candidate work.DiscoveryCandidate
}

func newDiscoveryA2UIFixture(t *testing.T, settings func(*work.Store) *model.Settings) *discoveryA2UIFixture {
	t.Helper()
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	instance, err := work.Open(filepath.Join(state, "instance.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { instance.Close() })
	f := &accountFixture{t: t, store: instance,
		handler: (Server{Store: instance, Origin: "https://ops.example", Hosts: []string{"ops.example"}}).Handler(fstest.MapFS{})}
	actor := f.setup()
	cookie := f.login("admin", "correct horse battery staple")
	path := filepath.Join(state, "personal.db")
	store, err := work.OpenAccountBusinessStore(path, f.store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.BindAccountBusinessStore(t.Context(), actor); err != nil {
		t.Fatal(err)
	}
	server := Server{Store: f.store, Origin: "https://ops.example", Hosts: []string{"ops.example"}}
	if settings != nil {
		server.Settings = settings(f.store)
	}
	server.PersonalAPI = func(id string) (http.Handler, error) {
		if id != actor {
			return nil, work.ErrAccountDenied
		}
		return server.PersonalHandler(store), nil
	}
	f.handler = server.Handler(fstest.MapFS{})
	source := a2uiFixtureSource{}
	if _, err := store.GrantDiscovery(t.Context(), actor, work.DiscoveryGrant{Source: "fixture-local", Scope: "fixture-private-scope", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, source); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishDiscoveryPlan(t.Context(), actor, work.DiscoveryPlanRequest{ID: "a2ui-fixture-plan", Source: "fixture-local", Scope: "fixture-private-scope", PeriodSeconds: 60, FirstAt: time.Now().Add(150 * time.Millisecond).UnixMilli()}, source); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	closed := make(chan struct{})
	go func() { defer close(closed); store.RunDiscovery(ctx, source) }()
	defer func() { cancel(); <-closed }()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		items, err := store.ListDiscoveryCandidates(t.Context(), actor)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 1 {
			return &discoveryA2UIFixture{account: f, cookie: cookie, actor: actor, instance: f.store, store: store, path: path, server: server, candidate: items[0]}
		}
	}
	t.Fatal("fixture did not ingest the discovery observation")
	return nil
}

func (f *discoveryA2UIFixture) request(method, path, requestKey string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	raw := []byte(nil)
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, "https://ops.example"+path, bytes.NewReader(raw))
	r.RemoteAddr = "127.0.0.1:12345"
	if requestKey != "" {
		r.Header.Set("X-ClusBot-Request-Key", requestKey)
	}
	if method != http.MethodGet {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://ops.example")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	f.account.handler.ServeHTTP(w, r)
	return w
}

func (f *discoveryA2UIFixture) lifecycle(t *testing.T, key string) (a2uiDiscoveryReply, map[string]any) {
	t.Helper()
	w := f.request("POST", "/api/v1/a2ui/discovery-candidates", key, map[string]string{}, f.cookie)
	var response a2uiDiscoveryReply
	accountJSON(t, w, &response)
	if w.Code != http.StatusOK || response.Conversation == "" || response.Run == "" {
		t.Fatalf("open first real conversation: %d %s", w.Code, w.Body)
	}
	return response, renderedA2UIEvent(t, response, a2uiReviewAction)
}

func renderedA2UIEvent(t *testing.T, reply a2uiDiscoveryReply, name string) map[string]any {
	t.Helper()
	if len(reply.Surface) != 1 || reply.Surface[0]["version"] != "1.0" {
		t.Fatalf("wrong A2UI 1.0 surface: %+v", reply.Surface)
	}
	surface := reply.Surface[0]["createSurface"].(map[string]any)
	if surface["catalogId"] != assistantA2UICatalog {
		t.Fatalf("wrong catalog: %+v", surface)
	}
	for _, component := range surface["components"].([]any) {
		c := component.(map[string]any)
		if c["component"] != "Button" {
			continue
		}
		event := c["action"].(map[string]any)["event"].(map[string]any)
		if event["name"] == name {
			return map[string]any{"version": "1.0", "action": map[string]any{
				"name": name, "surfaceId": surface["surfaceId"], "sourceComponentId": c["id"],
				"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "context": event["context"],
			}}
		}
	}
	t.Fatalf("no event %s in A2UI surface: %+v", name, surface)
	return nil
}

func TestDiscoveryA2UIStandardRoundTripAndScopedConfirmation(t *testing.T) {
	var calls atomic.Int32
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	allowReturn := func() { releaseOnce.Do(func() { close(release) }) }
	var mode atomic.Value
	mode.Store("")
	t.Setenv("CLUSBOT_A2UI_TEST_MODEL_KEY", "fixture-model-key")
	modelFixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callNumber := calls.Add(1)
		found, _ := io.ReadAll(r.Body)
		for _, private := range []string{"fixture-private-serial", "192.0.2.184", "fixture-local", "fixture-private-scope",
			"fixture-private-owner", "fixture-private-location", "fixture-private-role", "untrusted renderer note"} {
			if bytes.Contains(found, []byte(private)) {
				t.Errorf("model input leaked %s: %s", private, found)
			}
		}
		for _, want := range []string{"candidate_id", "revision", "allowed_results", "ask_for_details", "cannot_prepare", "admission_fields"} {
			if !bytes.Contains(found, []byte(want)) {
				t.Errorf("model did not receive scoped field %q: %s", want, found)
			}
		}
		if callNumber == 1 {
			close(reached)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		choice := mode.Load().(string)
		if choice == "" {
			choice = "show_review"
			if bytes.Contains(found, []byte(a2uiRequestAction)) {
				choice = "show_admission_preview"
			}
		}
		reply := fmt.Sprintf(`{"action":%q,"reply":"我已核对本次候选记录"}`, choice)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", reply)
	}))
	defer modelFixture.Close()
	defer allowReturn()
	f := newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		settings := &model.Settings{Store: store}
		if _, err := settings.Initialize(t.Context(), work.ModelSettings{Protocol: model.ProtocolResponses, ModelID: "local-fixture", BaseURL: modelFixture.URL, KeyRef: "env:CLUSBOT_A2UI_TEST_MODEL_KEY"}); err != nil {
			t.Fatal(err)
		}
		return settings
	})
	list, review := f.lifecycle(t, "open-candidate-first")
	if reopened, _ := f.lifecycle(t, "open-candidate-first"); reopened.Conversation != list.Conversation || reopened.Run != list.Run {
		t.Fatal("lost open response created a second conversation")
	}
	base := "/api/v1/conversations/" + list.Conversation
	payload := review["action"].(map[string]any)
	payload["userMessage"] = "untrusted renderer note"
	payload["metadata"] = map[string]any{"extensions": map[string]any{"fixture": "untrusted renderer note"}}
	post := func(key string, body map[string]any, cookie *http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := f.request("POST", base+"/a2ui/actions", key, body, cookie)
		if w.Code != want {
			t.Fatalf("action %q HTTP %d want %d: %s", key, w.Code, want, w.Body)
		}
		return w
	}
	post("anonymous-asset", review, nil, http.StatusUnauthorized)
	wrongCookie := f.account.login("admin", "correct horse battery staple")
	post("foreign-session-review", review, wrongCookie, http.StatusConflict)
	badComponent := payload["sourceComponentId"]
	payload["sourceComponentId"] = "fake_button"
	post("fake-component", review, f.cookie, http.StatusConflict)
	payload["sourceComponentId"] = badComponent
	if calls.Load() != 0 {
		t.Fatalf("invalid action reached model: %d", calls.Load())
	}

	firstResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstResponse <- f.request("POST", base+"/a2ui/actions", "candidate-review-one", review, f.cookie)
	}()
	select {
	case <-reached:
	case early := <-firstResponse:
		t.Fatalf("review did not reach the model: %d %s", early.Code, early.Body)
	case <-time.After(4 * time.Second):
		t.Fatal("review did not reach the local fixture")
	}
	if w := post("candidate-review-one", review, f.cookie, http.StatusConflict); !strings.Contains(w.Body.String(), "a2ui_action_pending") {
		t.Fatalf("duplicate pending response was not explicit: %s", w.Body)
	}
	if w := f.request("GET", base+"/a2ui/actions/candidate-review-one", "", nil, wrongCookie); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "a2ui_action_pending") {
		t.Fatalf("lost-action readback falsely completed a pending model: %d %s", w.Code, w.Body)
	}
	allowReturn()
	first := <-firstResponse
	if first.Code != http.StatusOK {
		t.Fatalf("first review: %d %s", first.Code, first.Body)
	}
	repeated := post("candidate-review-one", review, f.cookie, http.StatusOK)
	if calls.Load() != 1 || first.Body.String() != repeated.Body.String() {
		t.Fatalf("lost ACK repeated a model call or changed receipt: %d\n%s\n%s", calls.Load(), first.Body, repeated.Body)
	}
	var reviewed a2uiDiscoveryReply
	accountJSON(t, first, &reviewed)
	page, err := f.store.Detail(list.Conversation, false)
	if err != nil || len(page.Messages) != 4 || page.Task != nil || reviewed.Conversation != list.Conversation {
		t.Fatalf("review did not append solely to the current conversation: %+v %v", page, err)
	}

	admit := renderedA2UIEvent(t, reviewed, a2uiRequestAction)
	fields := admit["action"].(map[string]any)["context"].(map[string]any)
	fields["owner"], fields["location"], fields["role"] = "fixture-private-owner", "fixture-private-location", "fixture-private-role"
	mode.Store("cannot_prepare")
	w := post("candidate-model-declines", admit, f.cookie, http.StatusOK)
	var declined a2uiDiscoveryReply
	accountJSON(t, w, &declined)
	if declined.Outcome != "cannot_prepare" || declined.ConfirmationID != "" || strings.Contains(w.Body.String(), a2uiRequestAction+"\"}") {
		t.Fatalf("model declined but rendered a confirmation: %s", w.Body)
	}
	mode.Store("")
	prepared := post("candidate-prepare-one", admit, f.cookie, http.StatusOK)
	replayed := post("candidate-prepare-one", admit, f.cookie, http.StatusOK)
	if calls.Load() != 3 || prepared.Body.String() != replayed.Body.String() {
		t.Fatalf("preview retry called the model again: %d", calls.Load())
	}
	var preview a2uiDiscoveryReply
	accountJSON(t, prepared, &preview)
	if preview.State != "confirmation_required" || preview.ConfirmationID == "" || preview.Outcome != "show_admission_preview" {
		t.Fatalf("not a trusted preview: %s", prepared.Body)
	}
	fields["owner"] = "changed payload"
	post("candidate-prepare-one", admit, f.cookie, http.StatusConflict)
	current, _ := f.store.ListDiscoveryCandidates(t.Context(), f.actor)
	if len(current) != 1 || current[0].State != "observed" || current[0].Asset != "" {
		t.Fatalf("before confirmation candidate is not unchanged: %+v", current)
	}
	readback := f.request("GET", base+"/a2ui/actions/candidate-prepare-one", "", nil, wrongCookie)
	if readback.Code != http.StatusOK || strings.Contains(readback.Body.String(), preview.ConfirmationID) || !strings.Contains(readback.Body.String(), preview.Run) {
		t.Fatalf("reauthenticated readback did not safely recover outcome: %d %s", readback.Code, readback.Body)
	}

	confirm := map[string]string{"confirmation_id": preview.ConfirmationID}
	confirmPath := base + "/a2ui/actions/candidate-prepare-one/confirm"
	for _, tc := range []struct {
		path   string
		cookie *http.Cookie
	}{
		{"/api/v1/assistant/confirm", f.cookie},
		{base + "/a2ui/actions/other-request-key/confirm", f.cookie},
		{confirmPath, wrongCookie},
	} {
		if w := f.request("POST", tc.path, "", confirm, tc.cookie); w.Code != http.StatusConflict {
			t.Fatalf("incorrect route/key/session used frozen preview: %d %s", w.Code, w.Body)
		}
	}
	// A changed source observation with exactly the same display revision and
	// serial is still not the evidence the operator confirmed.
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE discovery_observations SET address=? WHERE candidate_id=? AND seen_at=?", "192.0.2.185", f.candidate.ID, f.candidate.SeenAt); err != nil {
		t.Fatal(err)
	}
	beforeStale := calls.Load()
	fields["owner"] = "fixture-private-owner"
	if staleForm := post("candidate-prepare-stale", admit, f.cookie, http.StatusConflict); !strings.Contains(staleForm.Body.String(), "a2ui_surface_stale") || calls.Load() != beforeStale {
		t.Fatalf("a form tied to superseded evidence spent a model call: %s", staleForm.Body)
	}
	if staleReview := f.request("GET", base+"/a2ui/actions/candidate-review-one", "", nil, wrongCookie); staleReview.Code != http.StatusConflict ||
		!strings.Contains(staleReview.Body.String(), "a2ui_candidate_stale") {
		t.Fatalf("a superseded review produced a new form: %d %s", staleReview.Code, staleReview.Body)
	}
	if stale := f.request("POST", confirmPath, "", confirm, f.cookie); stale.Code != http.StatusConflict ||
		!strings.Contains(stale.Body.String(), "a2ui_candidate_stale") {
		t.Fatalf("same-millisecond address replacement was not rejected: %d %s", stale.Code, stale.Body)
	}
	var admissions int
	if err := db.QueryRow("SELECT count(*) FROM discovery_admissions WHERE candidate_id=?", f.candidate.ID).Scan(&admissions); err != nil || admissions != 0 {
		t.Fatalf("stale confirmation wrote an asset: %d %v", admissions, err)
	}
	if _, err := db.Exec("UPDATE discovery_observations SET address=? WHERE candidate_id=? AND seen_at=?", f.candidate.Address, f.candidate.ID, f.candidate.SeenAt); err != nil {
		t.Fatal(err)
	}
	prepared = post("candidate-prepare-fresh", admit, f.cookie, http.StatusOK)
	accountJSON(t, prepared, &preview)
	if preview.ConfirmationID == "" || preview.State != "confirmation_required" {
		t.Fatalf("fresh preview was not actionable: %s", prepared.Body)
	}
	confirm["confirmation_id"] = preview.ConfirmationID
	confirmPath = base + "/a2ui/actions/candidate-prepare-fresh/confirm"
	completed := f.request("POST", confirmPath, "", confirm, f.cookie)
	if completed.Code != http.StatusOK || !strings.Contains(completed.Body.String(), `"state":"admitted"`) {
		t.Fatalf("trusted confirmation failed: %d %s", completed.Code, completed.Body)
	}
	if w := f.request("POST", confirmPath, "", confirm, f.cookie); w.Code != http.StatusConflict {
		t.Fatalf("confirmation replay accepted: %d %s", w.Code, w.Body)
	}
	// Lose the final ACK and reopen the real private database and HTTP handler.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = work.OpenAccountBusinessStore(f.path, f.instance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	f.server.PersonalAPI = func(id string) (http.Handler, error) {
		if id != f.actor {
			return nil, work.ErrAccountDenied
		}
		return f.server.PersonalHandler(f.store), nil
	}
	f.account.handler = f.server.Handler(fstest.MapFS{})
	terminal := f.request("GET", base+"/a2ui/actions/candidate-prepare-fresh", "", nil, wrongCookie)
	var recovery a2uiDiscoveryReply
	accountJSON(t, terminal, &recovery)
	if terminal.Code != http.StatusOK || recovery.Outcome != "admitted" || recovery.Run != preview.Run || recovery.Conversation != list.Conversation ||
		strings.Contains(terminal.Body.String(), preview.ConfirmationID) || strings.Contains(terminal.Body.String(), "fixture-private-serial") {
		t.Fatalf("final lost ACK was not recovered exactly from the original conversation, or leaked data: %d %s", terminal.Code, terminal.Body)
	}
	page, err = f.store.Detail(list.Conversation, false)
	if err != nil || page.Task != nil || !strings.Contains(page.Messages[len(page.Messages)-1].Text, "已纳管") {
		t.Fatalf("admission did not append to original conversation: %+v %v", page, err)
	}
	var owner, location, role string
	if err := db.QueryRow("SELECT owner,location,role FROM discovery_admissions WHERE candidate_id=?", f.candidate.ID).Scan(&owner, &location, &role); err != nil ||
		owner != "fixture-private-owner" || location != "fixture-private-location" || role != "fixture-private-role" {
		t.Fatalf("not exactly frozen fields: %q %q %q %v", owner, location, role, err)
	}
}

func TestDiscoveryA2UIStaleNoModelAndCrossAccount(t *testing.T) {
	f := newDiscoveryA2UIFixture(t, nil)
	list, review := f.lifecycle(t, "open-candidate-account")
	base := "/api/v1/conversations/" + list.Conversation
	created := f.account.request("POST", "/api/v1/admin/users", map[string]string{
		"username": "member", "password": "a different sufficiently long pass",
	}, f.cookie)
	if created.Code != http.StatusCreated {
		t.Fatalf("create member: %d %s", created.Code, created.Body)
	}
	member := f.account.login("member", "a different sufficiently long pass")
	w := f.account.request("POST", "/api/v1/me/password", map[string]string{"current_password": "a different sufficiently long pass", "new_password": "a different replaced long pass"}, member)
	if w.Code != http.StatusNoContent {
		t.Fatalf("change member password: %d %s", w.Code, w.Body)
	}
	member = f.account.login("member", "a different replaced long pass")
	var memberSession struct{ User work.Account }
	accountJSON(t, f.account.request("GET", "/api/v1/session", nil, member), &memberSession)
	memberStore, err := work.OpenAccountBusinessStore(filepath.Join(t.TempDir(), "member.db"), f.instance)
	if err != nil {
		t.Fatal(err)
	}
	defer memberStore.Close()
	if err := memberStore.BindAccountBusinessStore(t.Context(), memberSession.User.PrincipalID); err != nil {
		t.Fatal(err)
	}
	personal := f.server.PersonalAPI
	f.server.PersonalAPI = func(id string) (http.Handler, error) {
		if id == memberSession.User.PrincipalID {
			return f.server.PersonalHandler(memberStore), nil
		}
		return personal(id)
	}
	f.account.handler = f.server.Handler(fstest.MapFS{})
	_, review = f.lifecycle(t, "open-candidate-account")
	for _, path := range []string{base + "/a2ui/discovery-candidates", base + "/a2ui/actions/candidate-no-model"} {
		if w := f.request("GET", path, "", nil, member); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), f.candidate.ID) {
			t.Fatalf("cross-account A2UI GET returned private data: %d %s", w.Code, w.Body)
		}
	}
	if w := f.request("POST", base+"/a2ui/actions", "candidate-no-model", review, member); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), f.candidate.ID) {
		t.Fatalf("cross-account A2UI POST returned private data: %d %s", w.Code, w.Body)
	}
	none := f.request("POST", base+"/a2ui/actions", "candidate-no-model", review, f.cookie)
	if none.Code != http.StatusServiceUnavailable || !strings.Contains(none.Body.String(), "a2ui_model_unavailable") {
		t.Fatalf("no model response not actionable: %d %s", none.Code, none.Body)
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE discovery_observations SET seen_at=seen_at-1 WHERE candidate_id=?", f.candidate.ID); err != nil {
		t.Fatal(err)
	}
	w = f.request("POST", base+"/a2ui/actions", "candidate-stale-observation", review, f.cookie)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "a2ui_candidate_stale") {
		t.Fatalf("stale authenticated surface did not fail closed: %d %s", w.Code, w.Body)
	}
	detail, err := f.store.Detail(list.Conversation, false)
	if err != nil || len(detail.Messages) != 2 || detail.Task != nil {
		t.Fatalf("failed actions fabricated a turn/task: %+v %v", detail, err)
	}
}
