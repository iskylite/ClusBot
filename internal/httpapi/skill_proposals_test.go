package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
)

func TestExperienceEvaluationAndProposalAPI(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "rsi-api.sqlite"))
	defer a.close()

	// 1. Anonymous request is rejected.
	req := a.request(http.MethodGet, "/api/v1/experience-evaluations", "")
	req.Header.Del("Cookie")
	res, err := a.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /experience-evaluations status = %d", res.StatusCode)
	}

	// 2. Authenticated GET returns empty list initially.
	res, err = a.ts.Client().Do(a.request(http.MethodGet, "/api/v1/experience-evaluations", ""))
	if err != nil {
		t.Fatal(err)
	}
	var runs []map[string]any
	if err := json.NewDecoder(res.Body).Decode(&runs); err != nil || res.StatusCode != http.StatusOK || len(runs) != 0 {
		res.Body.Close()
		t.Fatalf("unexpected GET /experience-evaluations: status=%d len=%d err=%v", res.StatusCode, len(runs), err)
	}
	res.Body.Close()

	// 3. POST /experience-evaluations without configured model returns 503 or 404 (never 200 or 500).
	body := `{"suite":{"cases":[{"id":"c1","prompt":"hello","goal":"test","scope_label":"s1","required_text":["hello"]}]}}`
	res, err = a.ts.Client().Do(a.request(http.MethodPost, "/api/v1/experience-evaluations", body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable && res.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /experience-evaluations without model status = %d, want 503 or 404", res.StatusCode)
	}

	// 4. POST /skill-proposals with unknown evaluation_run_id is rejected as 400 invalid_skill_proposal.
	res, err = a.ts.Client().Do(a.request(http.MethodPost, "/api/v1/skill-proposals", `{"scope_label":"s1","goal":"test","evaluation_run_id":"missing-run"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /skill-proposals with missing evaluation_run_id status = %d, want 400", res.StatusCode)
	}
}
