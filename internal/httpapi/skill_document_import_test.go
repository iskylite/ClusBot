package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"clusbot/internal/work"
)

func TestSkillDocumentImportHTTP(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "skill-import-http.sqlite"))
	defer a.close()
	input := work.SkillDocumentInput{Filename: "SKILL.md", Scope: "remote-resource-is-inert", Document: "---\nname: document-test\ndescription: No tool permissions\n---\nSee [resource](https://example.invalid/guide)."}
	body, _ := json.Marshal(input)
	r := a.request(http.MethodPost, "/api/v1/skills/import-preview", string(body))
	r.Header.Del("Cookie")
	res, err := a.ts.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("unauthed preview status %d", res.StatusCode)
	}
	code, out := a.post("/api/v1/skills/import-preview", string(body))
	if code != 200 {
		t.Fatalf("preview %d %+v", code, out)
	}
	var preview work.SkillDocumentPreview
	reply, _ := json.Marshal(out)
	if err := json.Unmarshal(reply, &preview); err != nil {
		t.Fatal(err)
	}
	req := struct {
		work.SkillDocumentInput
		Source  string `json:"expected_source_sha256"`
		Binding string `json:"preview_binding"`
	}{input, preview.Source.SourceSHA256, preview.PreviewBinding}
	bad := req
	bad.Binding = ""
	bytes, _ := json.Marshal(bad)
	if code, _ := a.post("/api/v1/skills/import", string(bytes)); code != 409 {
		t.Fatalf("missing preview binding %d", code)
	}
	bytes, _ = json.Marshal(req)
	for range 2 {
		code, out = a.post("/api/v1/skills/import", string(bytes))
		if code != 201 || out["active"] != false || out["instructions"] != preview.Instructions {
			t.Fatalf("draft import %d %+v", code, out)
		}
	}
}
