package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const v31BenchmarkPlanSchema = `
CREATE TABLE IF NOT EXISTS benchmark_profiles (
  profile_key TEXT NOT NULL,
  version INTEGER NOT NULL,
  document TEXT NOT NULL,
  digest TEXT NOT NULL,
  published_by TEXT NOT NULL,
  published_at INTEGER NOT NULL,
  PRIMARY KEY(profile_key,version)
);
`

const benchmarkValidatorV1 = "structured-metrics@1"

type BenchmarkProfile struct {
	Key     string          `json:"key"`
	Version int             `json:"version"`
	Cases   []BenchmarkCase `json:"cases"`
}

type BenchmarkCase struct {
	Key              string                    `json:"key"`
	Binding          BenchmarkBinding          `json:"binding"`
	Targets          []string                  `json:"targets"`
	DependsOn        []string                  `json:"depends_on,omitempty"`
	Parameters       map[string]string         `json:"parameters,omitempty"`
	ActualConfig     map[string]string         `json:"actual_config,omitempty"`
	RequiredMetrics  map[string]string         `json:"required_metrics"`
	NeedsRawEvidence bool                      `json:"needs_raw_evidence,omitempty"`
	Threshold        *BenchmarkMetricThreshold `json:"threshold,omitempty"`
}

type BenchmarkMetricThreshold struct {
	Metric  string  `json:"metric"`
	Minimum float64 `json:"minimum"`
}

type BenchmarkBinding struct {
	Connection string `json:"connection"`
	Revision   int    `json:"revision"`
	Tool       string `json:"tool"`
	Validator  string `json:"validator"`
}

func validBenchKey(key string) bool {
	return key != "" && len(key) <= 128 && strings.TrimSpace(key) == key && !strings.ContainsAny(key, "\\/:\r\n\t ")
}

func validateBenchmarkProfile(p BenchmarkProfile) error {
	if !validBenchKey(p.Key) || p.Version < 1 || len(p.Cases) == 0 || len(p.Cases) > 64 {
		return fmt.Errorf("%w：测试方案需含标识、版本和 1 到 64 个必测项", ErrBenchmarkInput)
	}
	defs := make(map[string]BenchmarkCase, len(p.Cases))
	matrix := 0
	for _, c := range p.Cases {
		matrix += len(c.Targets)
		if !validBenchKey(c.Key) || defs[c.Key].Key != "" || !validBenchKey(c.Binding.Connection) ||
			c.Binding.Revision < 1 || !validBenchKey(c.Binding.Tool) || c.Binding.Validator != benchmarkValidatorV1 ||
			len(c.Targets) == 0 || matrix > 1024 || len(c.DependsOn) > 64 ||
			len(c.RequiredMetrics) == 0 || len(c.RequiredMetrics) > 64 || len(c.Parameters) > 32 || len(c.ActualConfig) > 32 {
			return fmt.Errorf("%w：测项 %q 的标识、范围、工具、核验器或输入不受支持", ErrBenchmarkInput, c.Key)
		}
		targets := make(map[string]bool)
		for _, target := range c.Targets {
			if target == "" || strings.TrimSpace(target) != target || len(target) > 256 || targets[target] {
				return fmt.Errorf("%w：测项 %q 目标为空或重复", ErrBenchmarkInput, c.Key)
			}
			targets[target] = true
		}
		for _, values := range []map[string]string{c.Parameters, c.ActualConfig, c.RequiredMetrics} {
			for k, v := range values {
				if !validBenchKey(k) || len(v) == 0 || len(v) > 256 || strings.TrimSpace(v) != v {
					return fmt.Errorf("%w：测项 %q 字段 %q 无效", ErrBenchmarkInput, c.Key, k)
				}
			}
		}
		if th := c.Threshold; th != nil && (c.RequiredMetrics[th.Metric] == "" || !finiteBenchmark(th.Minimum)) {
			return fmt.Errorf("%w：测项 %q 达标线未引用必测指标", ErrBenchmarkInput, c.Key)
		}
		defs[c.Key] = c
	}
	visiting, visited := make(map[string]bool), make(map[string]bool)
	var visit func(string) error
	visit = func(key string) error {
		if visited[key] {
			return nil
		}
		if visiting[key] || defs[key].Key == "" {
			return fmt.Errorf("%w：依赖 %q 不存在或存在环", ErrBenchmarkInput, key)
		}
		visiting[key] = true
		siblings := make(map[string]bool)
		for _, dep := range defs[key].DependsOn {
			if siblings[dep] {
				return fmt.Errorf("%w：测项 %q 有重复依赖", ErrBenchmarkInput, key)
			}
			siblings[dep] = true
			if err := visit(dep); err != nil {
				return err
			}
		}
		delete(visiting, key)
		visited[key] = true
		return nil
	}
	for _, c := range p.Cases {
		if err := visit(c.Key); err != nil {
			return err
		}
	}
	return nil
}

// PublishBenchmarkProfile is operator-only. Publishing a plan does not grant
// execution rights, and a plan cannot replace the bytes of a published version.
func (s *Store) PublishBenchmarkProfile(ctx context.Context, actor string, p BenchmarkProfile) error {
	if actor == "" {
		return ErrAuthorizationDenied
	}
	if err := validateBenchmarkProfile(p); err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	d := digest(string(raw))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT digest FROM benchmark_profiles WHERE profile_key=? AND version=?`, p.Key, p.Version).Scan(&existing)
	switch {
	case err == nil:
		if existing != d {
			return fmt.Errorf("%w：已发布的方案版本不可覆盖", ErrConflict)
		}
		return tx.Commit()
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	stamp := now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_profiles VALUES(?,?,?,?,?,?)`, p.Key, p.Version, string(raw), d, actor, stamp); err != nil {
		return err
	}
	if err := recordAudit(tx, AuditEvent{Event: "benchmark_profile_published", Actor: actor, Summary: fmt.Sprintf("%s@%d", p.Key, p.Version), OccurredAt: stamp}); err != nil {
		return err
	}
	return tx.Commit()
}

func readBenchmarkProfile(q recordsQuerier, key string, version int) (BenchmarkProfile, error) {
	var raw string
	err := q.QueryRow(`SELECT document FROM benchmark_profiles WHERE profile_key=? AND version=?`, key, version).Scan(&raw)
	if err != nil {
		return BenchmarkProfile{}, err
	}
	var p BenchmarkProfile
	if err = json.Unmarshal([]byte(raw), &p); err != nil {
		return BenchmarkProfile{}, err
	}
	return p, validateBenchmarkProfile(p)
}

var ErrBenchmarkInput = errors.New("基准方案或请求无效")
var ErrBenchmarkBinding = errors.New("基准绑定不可用")

// benchmarkAuthorization checks explicitly approved per-object tool parameters.
// A grant for a different tool or without enumerated values is not a wildcard.
func benchmarkAuthorization(tx *sql.Tx, actor, target string, c BenchmarkCase, stamp int64) (string, error) {
	type parameters struct {
		Connection string              `json:"connection"`
		Tool       string              `json:"tool"`
		Allowed    map[string][]string `json:"allowed"`
	}
	rows, err := tx.Query(`SELECT auth_id,param_constraints FROM authorizations
WHERE subject=? AND action='benchmark.execute' AND object_ref=? AND revoked_at=0
AND valid_from<=? AND (expires_at=0 OR expires_at>?) ORDER BY policy_version DESC,auth_id`, actor, target, stamp, stamp)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var aid, raw string
		if err := rows.Scan(&aid, &raw); err != nil {
			return "", err
		}
		var p parameters
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if dec.Decode(&p) != nil || !json.Valid([]byte(raw)) || p.Connection != c.Binding.Connection || p.Tool != c.Binding.Tool || len(p.Allowed) != len(c.Parameters) {
			continue
		}
		match := true
		for k, v := range c.Parameters {
			allowed := false
			for _, v1 := range p.Allowed[k] {
				if v1 == v {
					allowed = true
				}
			}
			if !allowed {
				match = false
				break
			}
		}
		if match {
			return aid, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%w：未批准该资产、测试工具或参数", ErrAuthorizationDenied)
}

func migrateV31Benchmark(tx *sql.Tx) error {
	if _, err := tx.Exec(v31BenchmarkPlanSchema); err != nil {
		return err
	}
	_, err := tx.Exec(v31BenchmarkRunSchema)
	return err
}
