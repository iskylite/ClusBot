package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/work"
)

func loginLocalCookie(t *testing.T, handler http.Handler, token string) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/session", strings.NewReader(`{"token":"`+token+`"}`))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || len(rec.Result().Cookies()) == 0 {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	return rec.Result().Cookies()[0].String()
}

func doAuthedJSON(t *testing.T, handler http.Handler, method, path, cookie string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Host = "127.0.0.1:8080"
	if method != "GET" && method != "HEAD" {
		req.Header.Set("Origin", "http://127.0.0.1:8080")
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestFaultSegmentsAndIncidentFiveStepClosure(t *testing.T) {
	ctx := context.Background()
	store, err := work.Open(t.TempDir() + "/clusbot.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.ImportDomainAssets(ctx, []work.DomainAssetEntry{{
		ObjectRef: "node-01",
		Kind:      "node",
		Identity:  "sn-node-01",
	}}); err != nil {
		t.Fatal(err)
	}

	srv := Server{
		Store: store,
		Token: "test-token",
		Hosts: []string{"127.0.0.1:8080"},
		TicketEffectChecker: func(ctx context.Context, objectRef string) (bool, string, error) {
			return true, "independent health check ok for " + objectRef, nil
		},
	}
	handler := srv.Handler(fstest.MapFS{"index.html": {Data: []byte("test")}})
	cookie := loginLocalCookie(t, handler, "test-token")

	// 1. Record fault segment + observation -> ClusterTrendStats becomes live.
	rec := doAuthedJSON(t, handler, "POST", "/api/v1/domain/fault-segments", cookie, map[string]any{
		"fault_key":  "nvlink_down",
		"object_ref": "node-01",
		"state":      "active",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/domain/fault-segments = %d: %s", rec.Code, rec.Body.String())
	}
	trends, err := store.ClusterTrendStats(ctx, "local-owner", "7d", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if trends.FaultSourceStatus != "live" {
		t.Fatalf("expected FaultSourceStatus=live, got %q", trends.FaultSourceStatus)
	}

	// 2. Create incident -> add hypothesis -> add evidence -> upsert self-heal policy -> execute -> resolve.
	inc, err := store.CreateIncident(ctx, "node-01 nvlink degraded", `{"nodes":["node-01"]}`)
	if err != nil {
		t.Fatal(err)
	}
	hypRec := doAuthedJSON(t, handler, "POST", "/api/v1/incidents/"+inc.IncidentID+"/hypotheses", cookie, map[string]any{
		"statement":    "GPU fabric link reset needed",
		"unknowns":     []string{"cable_health"},
		"next_queries": []string{"fault_check"},
	})
	if hypRec.Code != http.StatusCreated {
		t.Fatalf("POST hypothesis = %d: %s", hypRec.Code, hypRec.Body.String())
	}
	var hyp work.IncidentHypothesis
	if err := json.Unmarshal(hypRec.Body.Bytes(), &hyp); err != nil {
		t.Fatal(err)
	}
	evRec := doAuthedJSON(t, handler, "POST", "/api/v1/incidents/"+inc.IncidentID+"/hypotheses/"+hyp.HypothesisID+"/evidence", cookie, map[string]any{
		"evidence_ref": "obs:nvlink_down",
		"supporting":   true,
		"state":        "supported",
	})
	if evRec.Code != http.StatusOK {
		t.Fatalf("POST hypothesis evidence = %d: %s", evRec.Code, evRec.Body.String())
	}

	// Observe-only policy first: must not dispatch write.
	polRec := doAuthedJSON(t, handler, "POST", "/api/v1/self-heal/policies", cookie, map[string]any{
		"policy_key":        "gpu.fabric.reset",
		"version":           1,
		"action_capability": "ops.self_heal",
		"state":             "active",
		"observe_only":      true,
	})
	if polRec.Code != http.StatusOK {
		t.Fatalf("POST self-heal policy = %d: %s", polRec.Code, polRec.Body.String())
	}
	var pol work.SelfHealPolicy
	if err := json.Unmarshal(polRec.Body.Bytes(), &pol); err != nil {
		t.Fatal(err)
	}
	healRec := doAuthedJSON(t, handler, "POST", "/api/v1/incidents/"+inc.IncidentID+"/self-heal", cookie, map[string]any{
		"policy_id":  pol.PolicyID,
		"object_ref": "node-01",
	})
	if healRec.Code != http.StatusOK {
		t.Fatalf("POST self-heal = %d: %s", healRec.Code, healRec.Body.String())
	}
	var exec work.SelfHealExecution
	if err := json.Unmarshal(healRec.Body.Bytes(), &exec); err != nil {
		t.Fatal(err)
	}
	if exec.State != "proposed" || exec.VerificationOutcome != "observe_only" {
		t.Fatalf("expected observe_only proposed execution, got %+v", exec)
	}

	// Non-observe-only self-heal policy must be blocked at the HTTP layer (403).
	writePolRec := doAuthedJSON(t, handler, "POST", "/api/v1/self-heal/policies", cookie, map[string]any{
		"policy_key":        "gpu.fabric.reset.active",
		"version":           1,
		"action_capability": "ops.self_heal",
		"state":             "active",
		"observe_only":      false,
	})
	if writePolRec.Code != http.StatusOK {
		t.Fatalf("POST write self-heal policy = %d: %s", writePolRec.Code, writePolRec.Body.String())
	}
	var writePol work.SelfHealPolicy
	if err := json.Unmarshal(writePolRec.Body.Bytes(), &writePol); err != nil {
		t.Fatal(err)
	}
	blockedHealRec := doAuthedJSON(t, handler, "POST", "/api/v1/incidents/"+inc.IncidentID+"/self-heal", cookie, map[string]any{
		"policy_id":  writePol.PolicyID,
		"object_ref": "node-01",
	})
	if blockedHealRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-observe-only HTTP self-heal, got %d: %s", blockedHealRec.Code, blockedHealRec.Body.String())
	}

	// Overclaimed root cause must be rejected when root_cause_proven=false.
	badResolve := doAuthedJSON(t, handler, "POST", "/api/v1/incidents/"+inc.IncidentID+"/resolve", cookie, map[string]any{
		"recovery_verified": true,
		"recovery_evidence": "health check passed",
		"root_cause_proven": false,
		"root_cause":        "unverified guess",
	})
	if badResolve.Code == http.StatusOK {
		t.Fatalf("expected overclaimed root cause to be rejected, got 200: %s", badResolve.Body.String())
	}

	// Valid resolve with recovery_verified=true and root_cause_proven=false (root_cause empty).
	okResolve := doAuthedJSON(t, handler, "POST", "/api/v1/incidents/"+inc.IncidentID+"/resolve", cookie, map[string]any{
		"recovery_verified": true,
		"recovery_evidence": "health check passed",
		"root_cause_proven": false,
	})
	if okResolve.Code != http.StatusOK {
		t.Fatalf("POST resolve = %d: %s", okResolve.Code, okResolve.Body.String())
	}
	if !strings.Contains(okResolve.Body.String(), `"State":"resolved"`) {
		t.Fatalf("expected resolved incident state, got %s", okResolve.Body.String())
	}

	wf, err := store.IncidentWorkflow(ctx, inc.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(wf.Stages) < 3 || wf.Stages[1].DataStatus != "live" || wf.Stages[2].DataStatus != "live" {
		t.Fatalf("expected Stage 2 and Stage 3 DataStatus=live, got %+v", wf.Stages)
	}
}
