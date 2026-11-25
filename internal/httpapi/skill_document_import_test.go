package httpapi

import (
	"net/http"
	"testing"

	"clusbot/internal/work"
)

func TestSkillDocumentImportHTTP(t *testing.T) {
	f := newAccountFixture(t)
	f.setup()
	admin := f.login("admin", "correct horse battery staple")
	w := f.request(http.MethodPost, "/api/v1/admin/users", map[string]any{
		"username": "reader", "display_name": "Reader", "password": "fixture reader password",
	}, admin)
	if w.Code != http.StatusCreated {
		t.Fatalf("create non-admin reader: %d %s", w.Code, w.Body)
	}
	reader := f.login("reader", "fixture reader password")
	input := work.SkillDocumentInput{Filename: "SKILL.md", Scope: "remote-resource-is-inert", Document: "---\nname: document-test\ndescription: No tool permissions\n---\nSee [resource](https://example.invalid/guide)."}
	if w := f.request(http.MethodPost, "/api/v1/skills/import-preview", input, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthed preview status %d %s", w.Code, w.Body)
	}
	if w := f.request(http.MethodPost, "/api/v1/skills/import-preview", input, reader); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin can access shared Skill import preview: %d %s", w.Code, w.Body)
	}
	w = f.request(http.MethodPost, "/api/v1/skills/import-preview", input, admin)
	if w.Code != http.StatusOK {
		t.Fatalf("admin preview %d %s", w.Code, w.Body)
	}
	var preview work.SkillDocumentPreview
	accountJSON(t, w, &preview)
	req := struct {
		work.SkillDocumentInput
		Source  string `json:"expected_source_sha256"`
		Binding string `json:"preview_binding"`
	}{input, preview.Source.SourceSHA256, preview.PreviewBinding}
	bad := req
	bad.Binding = ""
	if w := f.request(http.MethodPost, "/api/v1/skills/import", bad, admin); w.Code != http.StatusConflict {
		t.Fatalf("missing preview binding %d %s", w.Code, w.Body)
	}
	if w := f.request(http.MethodPost, "/api/v1/skills/import", req, reader); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin can use an admin preview: %d %s", w.Code, w.Body)
	}
	var firstID string
	for i := range 2 {
		w := f.request(http.MethodPost, "/api/v1/skills/import", req, admin)
		var draft work.SkillVersion
		accountJSON(t, w, &draft)
		if w.Code != http.StatusCreated || draft.Active || draft.Instructions != preview.Instructions || draft.SkillID == "" {
			t.Fatalf("draft import %d %s", w.Code, w.Body)
		}
		if i == 0 {
			firstID = draft.SkillID
		} else if firstID != draft.SkillID || draft.Version != 1 {
			t.Fatalf("same preview created a duplicate draft: %+v", draft)
		}
	}
}
