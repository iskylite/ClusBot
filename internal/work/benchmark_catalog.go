package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type BenchmarkProfileItem struct {
	Key           string `json:"key"`
	LatestVersion int    `json:"latest_version"`
}

type BenchmarkProfilePage struct {
	Items     []BenchmarkProfileItem `json:"items"`
	NextAfter string                 `json:"next_after,omitempty"`
}

type BenchmarkVersionItem struct {
	Version     int   `json:"version"`
	PublishedAt int64 `json:"published_at_unix_ms"`
}

type BenchmarkVersionPage struct {
	Items      []BenchmarkVersionItem `json:"items"`
	NextBefore int                    `json:"next_before,omitempty"`
}

type BenchmarkProfileDetails struct {
	Profile     BenchmarkProfile     `json:"profile"`
	BaseVersion int                  `json:"base_version,omitempty"`
	Changes     *BenchmarkDraftDelta `json:"changes,omitempty"`
}

func (s *Store) ListBenchmarkProfiles(ctx context.Context, actor, after string, limit int) (BenchmarkProfilePage, error) {
	if actor == "" {
		return BenchmarkProfilePage{}, ErrAuthorizationDenied
	}
	if (after != "" && !validBenchKey(after)) || limit < 1 || limit > 100 {
		return BenchmarkProfilePage{}, ErrBenchmarkInput
	}
	rows, err := s.db.QueryContext(ctx, `SELECT profile_key,max(version) FROM benchmark_profiles WHERE profile_key>?
GROUP BY profile_key ORDER BY profile_key LIMIT ?`, after, limit+1)
	if err != nil {
		return BenchmarkProfilePage{}, err
	}
	defer rows.Close()
	page := BenchmarkProfilePage{Items: []BenchmarkProfileItem{}}
	for rows.Next() {
		var item BenchmarkProfileItem
		if err := rows.Scan(&item.Key, &item.LatestVersion); err != nil {
			return BenchmarkProfilePage{}, err
		}
		if len(page.Items) == limit {
			page.NextAfter = page.Items[limit-1].Key
		} else {
			page.Items = append(page.Items, item)
		}
	}
	return page, rows.Err()
}

func (s *Store) ListBenchmarkVersions(ctx context.Context, actor, key string, before, limit int) (BenchmarkVersionPage, error) {
	if actor == "" {
		return BenchmarkVersionPage{}, ErrAuthorizationDenied
	}
	if !validBenchKey(key) || before < 0 || limit < 1 || limit > 100 {
		return BenchmarkVersionPage{}, ErrBenchmarkInput
	}
	stmt := `SELECT version,published_at FROM benchmark_profiles WHERE profile_key=?`
	args := []any{key}
	if before > 0 {
		stmt += " AND version<?"
		args = append(args, before)
	}
	rows, err := s.db.QueryContext(ctx, stmt+" ORDER BY version DESC LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return BenchmarkVersionPage{}, err
	}
	defer rows.Close()
	page := BenchmarkVersionPage{Items: []BenchmarkVersionItem{}}
	for rows.Next() {
		var item BenchmarkVersionItem
		if err := rows.Scan(&item.Version, &item.PublishedAt); err != nil {
			return BenchmarkVersionPage{}, err
		}
		if len(page.Items) == limit {
			page.NextBefore = page.Items[limit-1].Version
		} else {
			page.Items = append(page.Items, item)
		}
	}
	return page, rows.Err()
}

func (s *Store) ReadBenchmarkProfileDetails(ctx context.Context, actor, key string, version, base int) (BenchmarkProfileDetails, error) {
	if actor == "" {
		return BenchmarkProfileDetails{}, ErrAuthorizationDenied
	}
	if !validBenchKey(key) || version < 1 || base < 0 || base >= version {
		return BenchmarkProfileDetails{}, ErrBenchmarkInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkProfileDetails{}, err
	}
	defer tx.Rollback()
	p, err := readBenchmarkProfile(tx, key, version)
	if err != nil {
		return BenchmarkProfileDetails{}, err
	}
	out := BenchmarkProfileDetails{Profile: p, BaseVersion: base}
	if base != 0 {
		old, err := readBenchmarkProfile(tx, key, base)
		if err != nil {
			return BenchmarkProfileDetails{}, err
		}
		diff := diffBenchmarkDraft(old.Cases, p.Cases)
		out.Changes = &diff
	}
	return out, nil
}

func (s *Store) ReadBenchmarkChatTarget(ctx context.Context, actor, target string) (BenchmarkProfileRef, error) {
	if actor == "" {
		return BenchmarkProfileRef{}, ErrAuthorizationDenied
	}
	if !benchmarkOrderTarget.MatchString(target) {
		return BenchmarkProfileRef{}, ErrBenchmarkInput
	}
	var ref BenchmarkProfileRef
	err := s.db.QueryRowContext(ctx, `SELECT profile_key,profile_version FROM benchmark_chat_targets WHERE target=?`,
		strings.ToUpper(target)).Scan(&ref.Profile, &ref.Version)
	return ref, err
}

type BenchmarkPreflight struct {
	Profile        BenchmarkProfileRef      `json:"profile"`
	CheckedAt      int64                    `json:"checked_at_unix_ms"`
	CanQueue       bool                     `json:"can_queue"`
	FacilityStatus string                   `json:"facility_status"`
	Cases          []BenchmarkPreflightCase `json:"cases"`
}

type BenchmarkPreflightCase struct {
	Key          string                    `json:"key"`
	Binding      BenchmarkBinding          `json:"binding"`
	Capabilities []string                  `json:"declared_capabilities"`
	Targets      []BenchmarkPreflightAsset `json:"targets"`
	Blockers     []BenchmarkPreflightIssue `json:"blockers"`
	Warnings     []BenchmarkPreflightIssue `json:"warnings"`
}

type BenchmarkPreflightAsset struct {
	Ref             string `json:"ref"`
	InventoryStatus string `json:"inventory_status"`
	Authorization   string `json:"authorization"`
	Occupancy       string `json:"benchmark_occupancy"`
}

type BenchmarkPreflightIssue struct {
	Code   string `json:"code"`
	Target string `json:"target,omitempty"`
	Detail string `json:"detail"`
}

// PreflightBenchmark checks a published plan against the same inventory and
// permission guards as submission. It never reserves resources, calls the
// provider, or claims that local inventory proves live cluster availability.
func (s *Store) PreflightBenchmark(ctx context.Context, actor string, ref BenchmarkProfileRef, provider BenchmarkProvider) (BenchmarkPreflight, error) {
	if actor == "" {
		return BenchmarkPreflight{}, ErrAuthorizationDenied
	}
	if !validBenchKey(ref.Profile) || ref.Version < 1 {
		return BenchmarkPreflight{}, ErrBenchmarkInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkPreflight{}, err
	}
	defer tx.Rollback()
	return preflightBenchmarkTx(ctx, tx, actor, ref, provider)
}

func preflightBenchmarkTx(ctx context.Context, tx *sql.Tx, actor string, ref BenchmarkProfileRef, provider BenchmarkProvider) (BenchmarkPreflight, error) {
	p, err := readBenchmarkProfile(tx, ref.Profile, ref.Version)
	if err != nil {
		return BenchmarkPreflight{}, err
	}
	out := BenchmarkPreflight{Profile: ref, CheckedAt: now(), CanQueue: true, FacilityStatus: "not_verified"}
	for _, c := range p.Cases {
		entry := BenchmarkPreflightCase{Key: c.Key, Binding: c.Binding, Capabilities: []string{},
			Blockers: []BenchmarkPreflightIssue{}, Warnings: []BenchmarkPreflightIssue{}}
		if provider == nil || !provider.Supports(c.Binding) {
			entry.Blockers = append(entry.Blockers, BenchmarkPreflightIssue{Code: "binding_unavailable", Detail: "未配置与冻结版本匹配的测试连接或工具"})
		} else {
			entry.Capabilities = []string{"start", "lookup", "get"}
			if _, ok := provider.(benchmarkCanceller); ok {
				entry.Capabilities = append(entry.Capabilities, "cancel")
			} else {
				entry.Warnings = append(entry.Warnings, BenchmarkPreflightIssue{Code: "cancel_unavailable", Detail: "适配器未声明远端取消；已发作业可能要等到终态"})
			}
		}
		entry.Warnings = append(entry.Warnings, BenchmarkPreflightIssue{Code: "live_capacity_unverified",
			Detail: "未探测实时拓扑、可用卡数、其他系统占用及连接可达性"})
		for _, target := range c.Targets {
			item := BenchmarkPreflightAsset{Ref: target, InventoryStatus: "eligible", Authorization: "allowed", Occupancy: "unknown"}
			_, aerr := benchmarkAsset(tx, target)
			switch {
			case errors.Is(aerr, ErrBenchmarkInput):
				item.InventoryStatus = "unregistered"
				entry.Blockers = append(entry.Blockers, BenchmarkPreflightIssue{"asset_unregistered", target, "目标未纳管"})
			case errors.Is(aerr, ErrState):
				item.InventoryStatus = "denied"
				entry.Blockers = append(entry.Blockers, BenchmarkPreflightIssue{"asset_ineligible", target, aerr.Error()})
			case aerr != nil:
				return BenchmarkPreflight{}, aerr
			}
			_, aerr = benchmarkAuthorization(tx, actor, target, c, out.CheckedAt)
			switch {
			case errors.Is(aerr, ErrAuthorizationDenied):
				item.Authorization = "denied"
				entry.Blockers = append(entry.Blockers, BenchmarkPreflightIssue{"authorization_denied", target, "未批准该目标、工具版本或参数"})
			case aerr != nil:
				return BenchmarkPreflight{}, aerr
			}
			if item.InventoryStatus == "eligible" && item.Authorization == "allowed" {
				held, err := targetHasLocalExclusiveHold(tx, target)
				if err != nil {
					return BenchmarkPreflight{}, fmt.Errorf("核对本地排他占用: %w", err)
				}
				item.Occupancy = "no_local_benchmark_hold"
				if held {
					item.Occupancy = "held"
					entry.Warnings = append(entry.Warnings, BenchmarkPreflightIssue{"asset_busy", target, "本地已有排他占用；可排队，不能立即派发"})
				}
			}
			entry.Targets = append(entry.Targets, item)
		}
		out.CanQueue = out.CanQueue && len(entry.Blockers) == 0
		out.Cases = append(out.Cases, entry)
	}
	return out, nil
}
