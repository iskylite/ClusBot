package work

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

const skillFixtureDocument = "---\nname: safe-example\ndescription: Local instructions\nallowed-tools: Bash(test)\nmetadata:\n  locale: zh-CN\n---\nRead [guide](references/guide.md).\n"

func TestSkillDocumentImportRetainsDisabledVersionAndProvenance(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "skill-document.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	s.SetConnectionCredentialKey("fixture-key")
	const scope = "fixture skill"
	base, err := s.DraftSkill(ctx, scope, "Current approved instructions", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateSkill(ctx, base.SkillID, base.Version, base.BaseRevision, base.Digest); err != nil {
		t.Fatal(err)
	}
	in := SkillDocumentInput{Filename: "SKILL.md", Document: skillFixtureDocument, Scope: scope, Revision: 1}
	preview, err := s.PreviewSkillDocument(ctx, in)
	if err != nil || preview.Source.SourceSHA256 != digest(skillFixtureDocument) || preview.BodySHA256 != digest(preview.Instructions) ||
		preview.BodySHA256 == preview.Source.SourceSHA256 || len(preview.Source.OptionalUnsupported) != 2 {
		t.Fatalf("bad preview: %+v %v", preview, err)
	}
	if list, err := s.Skills(ctx); err != nil || len(list) != 1 {
		t.Fatalf("preview mutated local drafts: %+v %v", list, err)
	}
	altered := in
	altered.Scope = "different scope"
	if _, err := s.ImportSkillDocument(ctx, altered, preview.Source.SourceSHA256, preview.PreviewBinding); !errors.Is(err, ErrSkillDocumentSource) {
		t.Fatalf("preview token reused for different scope: %v", err)
	}
	altered = in
	altered.Document += "\nmodified\n"
	if _, err := s.ImportSkillDocument(ctx, altered, preview.Source.SourceSHA256, preview.PreviewBinding); !errors.Is(err, ErrSkillDocumentSource) {
		t.Fatalf("preview token accepted different bytes: %v", err)
	}
	imported, err := s.ImportSkillDocument(ctx, in, preview.Source.SourceSHA256, preview.PreviewBinding)
	if err != nil || imported.Active || imported.ApprovedAt != nil || imported.Version != base.Version+1 || imported.Source == nil ||
		imported.Digest != preview.BodySHA256 || imported.Source.ResourceStatus != "main_document_only" {
		t.Fatalf("incorrect import %+v %v", imported, err)
	}
	generation, active := 0, 0
	if err := s.db.QueryRow("SELECT revision,active_version FROM personal_skills WHERE id=?", base.SkillID).Scan(&generation, &active); err != nil ||
		generation != 1 || active != base.Version {
		t.Fatalf("import changed established selection: %d %d %v", generation, active, err)
	}
	conversation, _, err := s.SubmitMemoryCommand(ctx, "skill-import-idle", "记住 local example", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetConversationSkillSelection(ctx, "user:session", conversation.ConversationID, ConversationSkillBindingInput{
		Action: "bind_skill", SkillID: imported.SkillID, Version: imported.Version,
	}); !errors.Is(err, ErrState) {
		t.Fatal("imported inactive version can be selected before approval")
	}
	if _, err := s.Submit(ctx, "skill-document-test-task", "current task", scope, "", true); err != nil {
		t.Fatal(err)
	}

	claimed, err := s.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runSkill, err := s.skillForRun(ctx, claimed)
	if err != nil || runSkill == nil || runSkill.Version != base.Version {
		t.Fatalf("run used an unapproved import instead of existing approved version: %+v %v", runSkill, err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.SetConnectionCredentialKey("fixture-key")
	again, err := s.ImportSkillDocument(ctx, in, preview.Source.SourceSHA256, preview.PreviewBinding)
	if err != nil || again.Version != imported.Version || again.Active || again.Source == nil {
		t.Fatalf("import replay %+v %v", again, err)
	}
	list, err := s.Skills(ctx)
	if err != nil || len(list) != 2 || list[0].Source == nil || list[1].Source != nil || !list[1].Active {
		t.Fatalf("history projections distinguish wrong versions: %+v %v", list, err)
	}
	if err := s.ActivateSkill(ctx, imported.SkillID, imported.Version, 1, imported.Digest); err != nil {
		t.Fatalf("ordinary explicit activation does not support imported draft: %v", err)
	}
}

func TestSkillDocumentParserRejectsUnsupportedYAMLAndPaths(t *testing.T) {
	for _, tc := range []struct{ name, filename, document string }{
		{"external-path", "/secrets/SKILL.md", skillFixtureDocument},
		{"remote-url", "https://example.invalid/SKILL.md", skillFixtureDocument},
		{"alias", "SKILL.md", "---\nname: &x sample\ndescription: *x\n---\nbody"},
		{"duplicate", "SKILL.md", "---\nname: sample\nname: example\ndescription: plain\n---\nbody"},
		{"typed-name", "SKILL.md", "---\nname: 123\ndescription: plain\n---\nbody"},
		{"bad-hyphens", "SKILL.md", "---\nname: some--name\ndescription: plain\n---\nbody"},
		{"nested-meta", "SKILL.md", "---\nname: sample\ndescription: plain\nmetadata: {k: [x]}\n---\nbody"},
		{"merged-meta", "SKILL.md", "---\nname: sample\ndescription: plain\nmetadata: {<<: {x: y}}\n---\nbody"},
		{"multi-doc", "SKILL.md", "---\nname: sample\ndescription: plain\n...\nname: another\n---\nbody"},
		{"unknown-mode", "SKILL.md", "---\nname: sample\ndescription: plain\nexecution-policy: trusted\n---\nbody"},
		{"long-body", "SKILL.md", "---\nname: sample\ndescription: plain\n---\n" + strings.Repeat("文", 4001)},
		{"nul-body", "SKILL.md", "---\nname: sample\ndescription: plain\n---\nbody\x00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseSkillDocument(SkillDocumentInput{Filename: tc.filename, Document: tc.document, Scope: "s"}); !errors.Is(err, ErrSkillDocument) {
				t.Fatalf("invalid payload accepted: %v", err)
			}
		})
	}
}
