package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"clusbot/internal/model"
)

func TestModelRuntimeProjectionRequiresSession(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "runtime.sqlite"))
	defer a.close()
	const path = "/api/v1/settings/model/runtime"
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
	var projection model.RuntimeProjection
	if err := json.NewDecoder(res.Body).Decode(&projection); err != nil || res.StatusCode != http.StatusOK ||
		res.Header.Get("Cache-Control") != "no-store" || projection.Availability != "not_configured" {
		t.Fatalf("wrong no-config model projection: %+v, %d, %v", projection, res.StatusCode, err)
	}
}

func TestModelProfilesAPIDoesNotReadOrAcceptPlainKey(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "profiles-model-api.sqlite"))
	defer a.close()
	const path = "/api/v1/settings/model/profiles"
	if status, result := a.post(path, `{"name":"x","protocol":"responses","base_url":"https://fixture.invalid","model_id":"test","api_key":"secret"}`); status != http.StatusBadRequest {
		t.Fatalf("plaintext secret accepted by profile API: %d %+v", status, result)
	}
	if status, result := a.post(path, `{"name":"x","protocol":"responses","base_url":"https://fixture.invalid","model_id":"test","api_key_ref":"env:FIXTURE_INACTIVE_KEY"}`); status != http.StatusCreated || result["api_key_ref"] != nil || result["api_key"] != nil {
		t.Fatalf("sanitized profile create: %d %+v", status, result)
	}
}
