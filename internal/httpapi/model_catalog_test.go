package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"clusbot/internal/work"
)

func TestModelCatalogHTTPUsesSharedProviderAndRedactsCredentials(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "catalog-http.db"))
	defer a.close()
	t.Setenv("CATALOG_HTTP_SECRET_REF", "catalog-http-secret")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer catalog-http-secret" {
			t.Error("provider credential not used")
			http.Error(w, "denied", 401)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"model-one"}]}`)
	}))
	defer backend.Close()
	if err := a.store.InitModelSettings(t.Context(), work.ModelSettings{Protocol: "responses", BaseURL: "https://legacy.invalid", KeyRef: "env:LEGACY_TEST_KEY"}); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/settings/model"
	providerBody := fmt.Sprintf(`{"name":"shared","protocol":"responses","base_url":%q,"auth_mode":"environment","api_key_ref":"env:CATALOG_HTTP_SECRET_REF"}`, backend.URL)
	status, provider := a.post(path+"/providers", providerBody)
	if status != http.StatusCreated {
		t.Fatalf("create provider: %d %+v", status, provider)
	}
	providerID, ok := provider["id"].(string)
	if !ok || provider["auth_mode"] != "environment" || provider["credential_status"] != "configured" {
		t.Fatalf("invalid provider: %+v", provider)
	}
	raw, _ := json.Marshal(provider)
	if strings.Contains(string(raw), "CATALOG_HTTP_SECRET_REF") || strings.Contains(string(raw), "catalog-http-secret") {
		t.Fatalf("credential leaked: %s", raw)
	}
	body := fmt.Sprintf(`{"name":"first","provider_id":%q,"provider_revision":1,"model_id":"model-one"}`, providerID)
	status, first := a.post(path+"/catalog", body)
	if status != http.StatusCreated || first["provider_id"] != providerID {
		t.Fatalf("create model: %d %+v", status, first)
	}
	body = fmt.Sprintf(`{"name":"second","provider_id":%q,"provider_revision":1,"model_id":"model-two"}`, providerID)
	status, second := a.post(path+"/catalog", body)
	if status != http.StatusCreated || second["provider_id"] != providerID {
		t.Fatalf("create second model: %d %+v", status, second)
	}
	profileID := first["id"].(string)
	selectBody := fmt.Sprintf(`{"source":"profile","revision":0,"profile_id":%q,"profile_revision":1}`, profileID)
	req := a.request("PUT", path+"/default", selectBody)
	res, err := a.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var selection map[string]any
	json.NewDecoder(res.Body).Decode(&selection)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || selection["profile_id"] != profileID {
		t.Fatalf("select default: %d %+v", res.StatusCode, selection)
	}
	status, diag := a.post(path+"/providers/"+providerID+"/diagnose", `{"revision":1}`)
	if status != http.StatusOK || diag["connection"] != "reachable" || diag["authentication"] != "not_rejected" {
		t.Fatalf("diagnostic: %d %+v", status, diag)
	}
	res, err = a.ts.Client().Do(a.request("GET", path+"/catalog", ""))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Cache-Control") != "no-store" || strings.Contains(string(out), "CATALOG_HTTP_SECRET_REF") || strings.Contains(string(out), "catalog-http-secret") {
		t.Fatalf("catalog response: %d %s", res.StatusCode, out)
	}
	// Explicit deletion of the selected model keeps the stable reference and
	// reports it unavailable instead of silently choosing the legacy default.
	req = a.request("DELETE", path+"/catalog/"+profileID, `{"revision":1}`)
	res, err = a.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete model: %d", res.StatusCode)
	}
	res, err = a.ts.Client().Do(a.request("GET", path+"/default", ""))
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(res.Body).Decode(&selection)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || selection["profile_id"] != profileID || selection["status"] != "profile_missing" {
		t.Fatalf("deleted default silently changed: %d %+v", res.StatusCode, selection)
	}
}

func TestModelCatalogHTTPAPIKeyProvider(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "catalog-api-key-http.db"))
	defer a.close()
	a.store.SetConnectionCredentialKey("catalog-http-store-key")

	const firstKey, rotatedKey = "catalog-first-secret", "catalog-rotated-secret"
	authorizations := make(chan string, 3)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("unexpected discovery request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		authorizations <- r.Header.Get("Authorization")
		fmt.Fprint(w, `{"data":[{"id":"fixture-model"}]}`)
	}))
	defer backend.Close()

	const path = "/api/v1/settings/model/providers"
	request := func(method, url, body string, wantStatus int) []byte {
		t.Helper()
		res, err := a.ts.Client().Do(a.request(method, url, body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != wantStatus {
			t.Fatalf("%s %s: status %d, want %d: %s", method, url, res.StatusCode, wantStatus, raw)
		}
		if strings.Contains(string(raw), firstKey) || strings.Contains(string(raw), rotatedKey) {
			t.Fatalf("provider response leaked a credential: %s", raw)
		}
		return raw
	}
	checkProvider := func(raw []byte, wantRevision int) string {
		t.Helper()
		var provider map[string]any
		if err := json.Unmarshal(raw, &provider); err != nil {
			t.Fatalf("invalid provider response: %s: %v", raw, err)
		}
		id, ok := provider["id"].(string)
		if !ok || id == "" || provider["auth_mode"] != "api_key" || provider["credential_status"] != "configured" || provider["revision"] != float64(wantRevision) {
			t.Fatalf("unexpected provider: %+v", provider)
		}
		if _, ok := provider["api_key"]; ok {
			t.Fatalf("provider returned api_key: %+v", provider)
		}
		if _, ok := provider["api_key_ref"]; ok {
			t.Fatalf("provider returned api_key_ref: %+v", provider)
		}
		return id
	}
	checkList := func(id string, wantRevision int) {
		t.Helper()
		raw := request(http.MethodGet, path, "", http.StatusOK)
		var providers []json.RawMessage
		if err := json.Unmarshal(raw, &providers); err != nil || len(providers) != 1 {
			t.Fatalf("invalid provider list: %s: %v", raw, err)
		}
		if got := checkProvider(providers[0], wantRevision); got != id {
			t.Fatalf("provider list returned ID %q, want %q", got, id)
		}
	}
	discover := func(id string, revision int, wantKey string) {
		t.Helper()
		raw := request(http.MethodPost, path+"/"+id+"/discover", fmt.Sprintf(`{"revision":%d}`, revision), http.StatusOK)
		var models []map[string]any
		if err := json.Unmarshal(raw, &models); err != nil || len(models) != 1 || models[0]["id"] != "fixture-model" {
			t.Fatalf("invalid discovery response: %s: %v", raw, err)
		}
		select {
		case got := <-authorizations:
			if got != "Bearer "+wantKey {
				t.Fatal("discovery did not use the expected bearer credential")
			}
		default:
			t.Fatal("discovery did not contact the provider")
		}
	}

	baseURL := backend.URL + "/v1"
	create := fmt.Sprintf(`{"name":"direct","protocol":"responses","base_url":%q,"auth_mode":"api_key","api_key":%q}`, baseURL+"/", firstKey)
	id := checkProvider(request(http.MethodPost, path, create, http.StatusCreated), 1)
	checkList(id, 1)
	discover(id, 1, firstKey)

	// Omitting the key preserves it for the same normalized address.
	update := fmt.Sprintf(`{"name":"renamed","protocol":"responses","base_url":%q,"auth_mode":"api_key","revision":1}`, baseURL)
	checkProvider(request(http.MethodPut, path+"/"+id, update, http.StatusOK), 2)
	checkList(id, 2)
	discover(id, 2, firstKey)

	// A different address must not silently receive the old credential.
	changedAddress := fmt.Sprintf(`{"name":"renamed","protocol":"responses","base_url":%q,"auth_mode":"api_key","revision":2}`, backend.URL+"/other")
	request(http.MethodPut, path+"/"+id, changedAddress, http.StatusBadRequest)
	checkList(id, 2)

	rotate := fmt.Sprintf(`{"name":"renamed","protocol":"responses","base_url":%q,"auth_mode":"api_key","api_key":%q,"revision":2}`, baseURL, rotatedKey)
	checkProvider(request(http.MethodPut, path+"/"+id, rotate, http.StatusOK), 3)
	checkList(id, 3)
	discover(id, 3, rotatedKey)
}

func TestModelCatalogHTTPLocalProviderWithoutCredential(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "catalog-no-auth-http.db"))
	defer a.close()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/models" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected anonymous discovery request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"local-model"}]}`)
	}))
	defer backend.Close()

	const path = "/api/v1/settings/model/providers"
	body := fmt.Sprintf(`{"name":"local","protocol":"responses","base_url":%q,"auth_mode":"none"}`, backend.URL)
	status, provider := a.post(path, body)
	id, ok := provider["id"].(string)
	if status != http.StatusCreated || !ok || provider["auth_mode"] != "none" || provider["credential_status"] != "not_required" {
		t.Fatalf("local provider: %d %+v", status, provider)
	}
	res, err := a.ts.Client().Do(a.request(http.MethodPost, path+"/"+id+"/discover", `{"revision":1}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var models []map[string]any
	if err := json.NewDecoder(res.Body).Decode(&models); err != nil || res.StatusCode != http.StatusOK || len(models) != 1 || models[0]["id"] != "local-model" {
		t.Fatalf("local discovery: %d %+v %v", res.StatusCode, models, err)
	}
}
