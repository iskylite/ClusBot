package work

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

var ErrSkillDocument = errors.New("SKILL.md 主文档不符合导入限制；只支持有 name、description 和正文的单个 UTF-8 文档")
var ErrSkillDocumentSource = errors.New("导入内容和已预览的 SHA256 不一致，请重新预览")
var singleSkillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

type SkillDocumentInput struct {
	Filename string `json:"filename"`
	Document string `json:"document"`
	Scope    string `json:"scope_label"`
	Revision int    `json:"revision"`
}

type SkillDocumentSource struct {
	Kind                string   `json:"kind"`
	Name                string   `json:"name"`
	Description         string   `json:"description"`
	SourceSHA256        string   `json:"source_sha256"`
	ResourceStatus      string   `json:"resource_status"`
	OptionalUnsupported []string `json:"optional_unsupported,omitempty"`
}

type SkillDocumentPreview struct {
	Source         SkillDocumentSource `json:"source"`
	BodySHA256     string              `json:"body_sha256"`
	Instructions   string              `json:"instructions"`
	Scope          string              `json:"scope_label"`
	Revision       int                 `json:"revision"`
	PreviewBinding string              `json:"preview_binding"`
}

func parseSkillDocument(input SkillDocumentInput) (SkillDocumentPreview, error) {
	raw := input.Document
	scope := strings.TrimSpace(input.Scope)
	if input.Filename != "SKILL.md" || input.Revision < 0 || scope == "" || utf8.RuneCountInString(scope) > 40 ||
		len(raw) > 48*1024 || !utf8.ValidString(raw) || strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsControl(r) && r != '\r' && r != '\n' && r != '\t' }) {
		return SkillDocumentPreview{}, ErrSkillDocument
	}
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	if len(lines) < 4 || lines[0] != "---" {
		return SkillDocumentPreview{}, ErrSkillDocument
	}
	end := 0
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end == 0 {
		return SkillDocumentPreview{}, ErrSkillDocument
	}
	header := strings.Join(lines[1:end], "\n")
	if len(header) > 8*1024 {
		return SkillDocumentPreview{}, ErrSkillDocument
	}
	body := strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
	if body == "" || utf8.RuneCountInString(body) > 4000 {
		return SkillDocumentPreview{}, ErrSkillDocument
	}
	// Reject YAML constructs that could resolve extra documents, aliases,
	// complex keys, dates or typed values before we interpret metadata.
	dec := yaml.NewDecoder(strings.NewReader(header))
	var doc, extra yaml.Node
	if err := dec.Decode(&doc); err != nil || len(doc.Content) != 1 || dec.Decode(&extra) != io.EOF {
		return SkillDocumentPreview{}, ErrSkillDocument
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode || root.Tag != "!!map" || root.Anchor != "" || len(root.Content) > 12 {
		return SkillDocumentPreview{}, ErrSkillDocument
	}
	isString := func(n *yaml.Node, max int) bool {
		return n.Kind == yaml.ScalarNode && n.Tag == "!!str" && n.Anchor == "" && utf8.RuneCountInString(n.Value) <= max &&
			!strings.ContainsRune(n.Value, 0)
	}
	metadata := func(n *yaml.Node) bool {
		if n.Kind != yaml.MappingNode || n.Tag != "!!map" || n.Anchor != "" || len(n.Content) > 32 {
			return false
		}
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			if !isString(key, 128) || !isString(value, 1024) || key.Value == "" || seen[key.Value] {
				return false
			}
			seen[key.Value] = true
		}
		return true
	}
	out := SkillDocumentSource{Kind: "skill_md_main_document", SourceSHA256: digest(raw), ResourceStatus: "main_document_only"}
	seen := map[string]bool{}
	for i := 0; i < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if !isString(key, 64) || seen[key.Value] {
			return SkillDocumentPreview{}, ErrSkillDocument
		}
		seen[key.Value] = true
		switch key.Value {
		case "name":
			if !isString(value, 64) || !singleSkillName.MatchString(value.Value) {
				return SkillDocumentPreview{}, ErrSkillDocument
			}
			out.Name = value.Value
		case "description":
			if !isString(value, 1024) || strings.TrimSpace(value.Value) == "" || strings.ContainsFunc(value.Value, func(r rune) bool {
				return unicode.IsControl(r) && r != '\n' && r != '\t'
			}) {
				return SkillDocumentPreview{}, ErrSkillDocument
			}
			out.Description = strings.TrimSpace(value.Value)
		case "metadata":
			if !metadata(value) {
				return SkillDocumentPreview{}, ErrSkillDocument
			}
			out.OptionalUnsupported = append(out.OptionalUnsupported, key.Value)
		case "license", "compatibility", "allowed-tools":
			if !isString(value, 1024) {
				return SkillDocumentPreview{}, ErrSkillDocument
			}
			out.OptionalUnsupported = append(out.OptionalUnsupported, key.Value)
		default:
			return SkillDocumentPreview{}, ErrSkillDocument
		}
	}
	if out.Name == "" || out.Description == "" {
		return SkillDocumentPreview{}, ErrSkillDocument
	}
	return SkillDocumentPreview{Source: out, BodySHA256: digest(body), Instructions: body, Scope: scope, Revision: input.Revision}, nil
}

func (s *Store) PreviewSkillDocument(ctx context.Context, input SkillDocumentInput) (SkillDocumentPreview, error) {
	preview, err := parseSkillDocument(input)
	if err != nil {
		return SkillDocumentPreview{}, err
	}
	var revision int
	err = s.db.QueryRowContext(ctx, "SELECT revision FROM personal_skills WHERE scope_label=?", preview.Scope).Scan(&revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SkillDocumentPreview{}, err
	}
	if revision != preview.Revision {
		return SkillDocumentPreview{}, ErrTaskRevision
	}
	preview.PreviewBinding = s.skillDocumentBinding(preview)
	return preview, nil
}

func (s *Store) skillDocumentBinding(preview SkillDocumentPreview) string {
	mac := hmac.New(sha256.New, s.connectionKey)
	_, _ = mac.Write([]byte("skill-main-document\x00" + toJSON([]any{preview.Scope, preview.Revision, preview.Source.SourceSHA256})))
	return hex.EncodeToString(mac.Sum(nil))
}

func migrateSkillDocumentSources(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS skill_document_sources (
 skill_id TEXT NOT NULL, version INTEGER NOT NULL, base_revision INTEGER NOT NULL,
 source_sha256 TEXT NOT NULL, metadata TEXT NOT NULL,
 PRIMARY KEY(skill_id,version), UNIQUE(skill_id,base_revision,source_sha256),
 FOREIGN KEY(skill_id,version) REFERENCES personal_skill_versions(skill_id,version) ON DELETE CASCADE
)`)
	return err
}

func (s *Store) ImportSkillDocument(ctx context.Context, input SkillDocumentInput, expectedHash, previewBinding string) (SkillVersion, error) {
	out, err := parseSkillDocument(input)
	if err != nil {
		return SkillVersion{}, err
	}
	if expectedHash != out.Source.SourceSHA256 || len(previewBinding) != 64 || !hmac.Equal([]byte(previewBinding), []byte(s.skillDocumentBinding(out))) {
		return SkillVersion{}, ErrSkillDocumentSource
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SkillVersion{}, err
	}
	defer tx.Rollback()
	// Separate identity from historical manually authored plain text, even
	// when both drafts have identical instructions.
	prior, err := scanSkill(tx.QueryRow(skillSelect+` JOIN skill_document_sources d ON d.skill_id=v.skill_id AND d.version=v.version
WHERE s.scope_label=? AND d.base_revision=? AND d.source_sha256=?`, out.Scope, out.Revision, expectedHash))
	if err == nil {
		prior.Source, err = readSkillDocumentSource(tx, prior.SkillID, prior.Version)
		return prior, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return SkillVersion{}, err
	}
	v, err := draftSkillTx(tx, out.Scope, out.Instructions, out.Revision, false)
	if err != nil {
		return SkillVersion{}, err
	}
	raw, err := json.Marshal(out.Source)
	if err != nil {
		return SkillVersion{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO skill_document_sources VALUES(?,?,?,?,?)`,
		v.SkillID, v.Version, v.BaseRevision, out.Source.SourceSHA256, string(raw)); err != nil {
		return SkillVersion{}, err
	}
	v.Source = &out.Source
	return v, tx.Commit()
}

func readSkillDocumentSource(q recordsQuerier, skill string, version int) (*SkillDocumentSource, error) {
	var raw string
	err := q.QueryRow(`SELECT metadata FROM skill_document_sources WHERE skill_id=? AND version=?`, skill, version).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var source SkillDocumentSource
	err = json.Unmarshal([]byte(raw), &source)
	return &source, err
}
