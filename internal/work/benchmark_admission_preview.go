package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
)

var (
	ErrBenchmarkAdmissionClarify      = errors.New("无法确定一条可预览的全套基准委托，请明确单个已绑定目标，不搭带规模或运行条件")
	ErrBenchmarkAdmissionScale        = errors.New("当前无法核实请求的卡数、拓扑或运行条件，不能用其他规模代替")
	ErrBenchmarkAdmissionModelBinding = errors.New("基准受理模型档案已不可用，请检查绑定、版本或凭据")
	ErrBenchmarkAdmissionModel        = errors.New("没有取得有效的基准委托模型建议")
)

type BenchmarkAdmissionPreviewInput struct {
	Text string `json:"text"`
}

type BenchmarkAdmissionCandidate struct {
	Target               string              `json:"target"`
	Profile              BenchmarkProfileRef `json:"profile"`
	inventoryFingerprint string
	bindingRevision      int
	AssetRefs            []string `json:"visible_asset_refs"`
}

func (c BenchmarkAdmissionCandidate) snapshotDigest() string {
	// A pre-revision pending preview has only the inventory fingerprint.
	// Its unknown binding lifetime cannot be reconstructed from today's state.
	return digest(toJSON([]any{"selection-revision/v1", c.Target, c.bindingRevision, c.inventoryFingerprint}))
}

type BenchmarkAdmissionModelInput struct {
	Text       string                        `json:"text"`
	Candidates []BenchmarkAdmissionCandidate `json:"candidates"`
}
type BenchmarkAdmissionModel func(context.Context, BenchmarkAdmissionModelInput, int) (string, error)

type BenchmarkAdmissionPreview struct {
	PreviewID        string                      `json:"preview_id"`
	State            string                      `json:"state"`
	Selection        BenchmarkAdmissionCandidate `json:"selection"`
	Preflight        BenchmarkPreflight          `json:"preflight"`
	ExecutionStarted bool                        `json:"execution_started"`
}

var benchmarkAdmissionTokens = regexp.MustCompile(`[A-Za-z0-9_-]+`)
var benchmarkAdmissionScale = regexp.MustCompile(`(?i)[0-9０-９零〇一二两三四五六七八九十百千]+[[:space:]]*(?:张[[:space:]]*)?(?:GPU|DCU|CU|卡|台|个节点|节点)`)

func benchmarkAdmissionText(text string) error {
	if len(text) > 4096 || strings.TrimSpace(text) == "" {
		return ErrBenchmarkInput
	}
	s := strings.ToLower(text)
	for _, deny := range []string{"不要", "别跑", "先别", "不跑", "不执行", "不用跑", "无需跑", "取消", "查进度", "看进度", "查状态", "跑完了吗", "出结果了吗", "现在进度", "假设", "能否", "是否", "可不可以", "先不", "don't", "do not", "status", "progress"} {
		if strings.Contains(s, deny) {
			return ErrBenchmarkAdmissionClarify
		}
	}
	if strings.ContainsAny(s, "?？“”「」\x22") {
		return ErrBenchmarkAdmissionClarify
	}
	for _, t := range benchmarkAdmissionTokens.FindAllString(s, -1) {
		if strings.Trim(t, "0123456789") == "" {
			return ErrBenchmarkAdmissionScale
		}
	}
	if benchmarkAdmissionScale.MatchString(s) {
		return ErrBenchmarkAdmissionScale
	}
	for _, limit := range []string{"卡数", "拓扑", "规模", "可用卡", "可用节点", "空闲", "如果", "仅当", "至少", "最多", "明天", "明早", "下周", "稍后", "等到"} {
		if strings.Contains(s, limit) {
			return ErrBenchmarkAdmissionScale
		}
	}
	return nil
}

func (s *Store) benchmarkAdmissionVisible(ctx context.Context, actor, text string, provider BenchmarkProvider) (BenchmarkAdmissionCandidate, BenchmarkPreflight, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, err
	}
	defer tx.Rollback()
	return benchmarkAdmissionVisibleTx(ctx, tx, actor, text, provider)
}

func benchmarkAdmissionVisibleTx(ctx context.Context, tx *sql.Tx, actor, text string, provider BenchmarkProvider) (BenchmarkAdmissionCandidate, BenchmarkPreflight, error) {
	tokens := benchmarkAdmissionTokens.FindAllString(strings.ToUpper(text), 65)
	if len(tokens) == 0 || len(tokens) > 64 {
		return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, ErrBenchmarkAdmissionClarify
	}
	// Resolve operator-maintained names exactly. Refuse ambiguity even if
	// only one of two named clusters is authorized; never silently choose it.
	rows, err := tx.QueryContext(ctx, `SELECT p.target,p.profile_key,p.profile_version,r.revision FROM benchmark_chat_targets p
JOIN benchmark_chat_target_revisions r ON r.target=p.target
JOIN benchmark_profiles b ON b.profile_key=p.profile_key AND b.version=p.profile_version
WHERE p.target IN (SELECT value FROM json_each(?)) ORDER BY p.target LIMIT 2`, toJSON(tokens))
	if err != nil {
		return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, err
	}
	var candidates []BenchmarkAdmissionCandidate
	for rows.Next() {
		var c BenchmarkAdmissionCandidate
		if err := rows.Scan(&c.Target, &c.Profile.Profile, &c.Profile.Version, &c.bindingRevision); err != nil {
			rows.Close()
			return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, err
	}
	if len(candidates) != 1 {
		return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, ErrBenchmarkAdmissionClarify
	}
	return benchmarkAdmissionCandidateTx(ctx, tx, actor, candidates[0], provider)
}

func benchmarkAdmissionCandidateTx(ctx context.Context, tx *sql.Tx, actor string, want BenchmarkAdmissionCandidate, provider BenchmarkProvider) (BenchmarkAdmissionCandidate, BenchmarkPreflight, error) {
	flight, err := preflightBenchmarkTx(ctx, tx, actor, want.Profile, provider)
	if err != nil {
		return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, err
	}
	if !flight.CanQueue {
		for _, c := range flight.Cases {
			for _, blocker := range c.Blockers {
				if blocker.Code == "authorization_denied" {
					return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, ErrAuthorizationDenied
				}
			}
		}
		return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, ErrBenchmarkBinding
	}
	assetRefs := map[string]bool{}
	for _, c := range flight.Cases {
		for _, target := range c.Targets {
			assetRefs[target.Ref] = true
		}
	}
	if len(assetRefs) > 64 {
		return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, ErrBenchmarkAdmissionClarify
	}
	for asset := range assetRefs {
		want.AssetRefs = append(want.AssetRefs, asset)
	}
	slices.Sort(want.AssetRefs)
	var identity []any
	for _, ref := range want.AssetRefs {
		a, err := benchmarkAsset(tx, ref)
		if err != nil {
			return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, err
		}
		identity = append(identity, []any{a.ObjectRef, a.Identity, a.Revision, a.OwnershipEpoch})
	}
	profile, err := readBenchmarkProfile(tx, want.Profile.Profile, want.Profile.Version)
	if err != nil {
		return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, err
	}
	var authorizations []any
	stamp := now()
	for _, c := range profile.Cases {
		_, grants, err := freezeBenchmarkCase(ctx, tx, actor, c, provider, stamp)
		if err != nil {
			return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, err
		}
		authorizations = append(authorizations, []any{c.Key, grants})
	}
	// Include the precise target binding lifetime; never backfill a pre-CAS
	// pending preview with today's version after a clear/rebind (ABA).
	if want.bindingRevision < 1 {
		return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, ErrBenchmarkAdmissionPreviewStale
	}
	want.inventoryFingerprint = digest(toJSON([]any{identity, authorizations, profile}))
	return want, flight, nil
}

func (s *Store) PreviewBenchmarkAdmissionAI(ctx context.Context, actor string, in BenchmarkAdmissionPreviewInput, provider BenchmarkProvider, interpret BenchmarkAdmissionModel) (BenchmarkAdmissionPreview, error) {
	if actor == "" {
		return BenchmarkAdmissionPreview{}, ErrAuthorizationDenied
	}
	if err := benchmarkAdmissionText(in.Text); err != nil {
		return BenchmarkAdmissionPreview{}, err
	}
	before, _, err := s.benchmarkAdmissionVisible(ctx, actor, in.Text, provider)
	if err != nil {
		return BenchmarkAdmissionPreview{}, err
	}
	if interpret == nil {
		return BenchmarkAdmissionPreview{}, ErrBenchmarkAdmissionModel
	}
	call, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	release, err := s.acquireModelSlot(call)
	if err != nil {
		return BenchmarkAdmissionPreview{}, ErrBenchmarkAdmissionModel
	}
	answer, err := func() (string, error) {
		defer release()
		s.runtimeMu.Lock()
		budget := min(384, s.admissionLimits.OutputTokens)
		s.runtimeMu.Unlock()
		if budget < 32 {
			return "", ErrBenchmarkAdmissionModel
		}
		return interpret(call, BenchmarkAdmissionModelInput{in.Text, []BenchmarkAdmissionCandidate{before}}, budget)
	}()
	if errors.Is(err, ErrBenchmarkAdmissionModelBinding) {
		return BenchmarkAdmissionPreview{}, err
	}
	if err != nil || call.Err() != nil {
		return BenchmarkAdmissionPreview{}, ErrBenchmarkAdmissionModel
	}
	if err := parseBenchmarkAdmissionModel(answer, in.Text, before); err != nil {
		return BenchmarkAdmissionPreview{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkAdmissionPreview{}, err
	}
	defer tx.Rollback()
	fresh, preflight, err := benchmarkAdmissionVisibleTx(ctx, tx, actor, in.Text, provider)
	if err != nil {
		return BenchmarkAdmissionPreview{}, err
	}
	if before.Target != fresh.Target || before.Profile != fresh.Profile || !slices.Equal(before.AssetRefs, fresh.AssetRefs) || before.snapshotDigest() != fresh.snapshotDigest() {
		return BenchmarkAdmissionPreview{}, fmt.Errorf("%w：目标绑定、资产身份或授权已变化，请重新预览", ErrConflict)
	}
	out := BenchmarkAdmissionPreview{PreviewID: id(), State: "preview_only", Selection: fresh, Preflight: preflight}
	if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_admission_previews
(preview_id,actor,target,profile_key,profile_version,snapshot_digest,created_at) VALUES(?,?,?,?,?,?,?)`,
		out.PreviewID, actor, fresh.Target, fresh.Profile.Profile, fresh.Profile.Version, fresh.snapshotDigest(), now()); err != nil {
		return BenchmarkAdmissionPreview{}, err
	}
	return out, tx.Commit()
}

func parseBenchmarkAdmissionModel(raw, text string, want BenchmarkAdmissionCandidate) error {
	if len(raw) > 4096 {
		return ErrBenchmarkAdmissionModel
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return ErrBenchmarkAdmissionModel
	}
	var action, target, profile, quote string
	var version int
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		key, ok := t.(string)
		if err != nil || !ok || seen[key] {
			return ErrBenchmarkAdmissionModel
		}
		seen[key] = true
		switch key {
		case "action":
			err = dec.Decode(&action)
		case "target":
			err = dec.Decode(&target)
		case "profile":
			err = dec.Decode(&profile)
		case "version":
			err = dec.Decode(&version)
		case "quote":
			err = dec.Decode(&quote)
		default:
			return ErrBenchmarkAdmissionModel
		}
		if err != nil {
			return ErrBenchmarkAdmissionModel
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return ErrBenchmarkAdmissionModel
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return ErrBenchmarkAdmissionModel
	}
	if action == "clarify" && len(seen) == 1 {
		return ErrBenchmarkAdmissionClarify
	}
	if action != "preview_admission" || len(seen) != 5 || !strings.EqualFold(target, want.Target) ||
		profile != want.Profile.Profile || version != want.Profile.Version || quote == "" ||
		strings.TrimSpace(quote) != quote || !strings.Contains(text, quote) {
		return ErrBenchmarkAdmissionModel
	}
	for _, token := range benchmarkAdmissionTokens.FindAllString(strings.ToUpper(quote), -1) {
		if token == want.Target {
			return nil
		}
	}
	return ErrBenchmarkAdmissionModel
}
