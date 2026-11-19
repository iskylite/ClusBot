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
