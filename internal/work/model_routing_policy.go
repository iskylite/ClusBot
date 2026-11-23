package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
)

// A policy is an explicit ordered list. An empty list clears this layer while
// retaining its revision, so an outdated editor cannot restore a removed rule.
type ModelRoutingCandidate struct {
	ProfileID         string `json:"profile_id"`
	ProfileRevision   int    `json:"profile_revision"`
	CredentialBinding string `json:"credential_binding"`
}

type ModelRoutingPolicy struct {
	Business           string                  `json:"business"`
	Role               string                  `json:"role"`
	Revision           int                     `json:"revision"`
	AllowCrossProvider bool                    `json:"allow_cross_provider"`
	Candidates         []ModelRoutingCandidate `json:"candidates"`
}

const MaxModelRoutingCandidates = 4

func ValidModelRoutingScope(business, role string) bool {
	roles := []string{GeneralRouterModelRole, ConversationReplyModelRole, TaskWorkerModelRole,
		BenchmarkAdmissionModelRole, BenchmarkRevisionModelRole, BenchmarkExecutionModelRole}
	if !slices.Contains(roles, role) {
		return false
	}
	switch business {
	case "default":
		return true
	case "conversation":
		return role == GeneralRouterModelRole || role == ConversationReplyModelRole
	case "task":
		return role == TaskWorkerModelRole
	case "benchmark":
		return role == BenchmarkAdmissionModelRole || role == BenchmarkRevisionModelRole || role == BenchmarkExecutionModelRole
	default:
		return false
	}
}

func migrateModelRoutingPolicies(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS model_routing_policies (
 business TEXT NOT NULL, role TEXT NOT NULL, revision INTEGER NOT NULL,
 allow_cross_provider INTEGER NOT NULL CHECK(allow_cross_provider IN (0,1)),
 candidates TEXT NOT NULL, PRIMARY KEY(business,role)
)`)
	return err
}

func readModelRoutingPolicy(ctx context.Context, tx *sql.Tx, business, role string) (ModelRoutingPolicy, error) {
	p := ModelRoutingPolicy{Business: business, Role: role, Candidates: []ModelRoutingCandidate{}}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT revision,allow_cross_provider,candidates FROM model_routing_policies WHERE business=? AND role=?`, business, role).Scan(&p.Revision, &p.AllowCrossProvider, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err == nil {
		err = json.Unmarshal([]byte(raw), &p.Candidates)
	}
	return p, err
}

func (s *Store) ReadModelRoutingPolicy(ctx context.Context, business, role string) (ModelRoutingPolicy, error) {
	if !ValidModelRoutingScope(business, role) {
		return ModelRoutingPolicy{}, ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelRoutingPolicy{}, err
	}
	defer tx.Rollback()
	return readModelRoutingPolicy(ctx, tx, business, role)
}

func (s *Store) ModelRoutingPolicies(ctx context.Context) ([]ModelRoutingPolicy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT business,role,revision,allow_cross_provider,candidates FROM model_routing_policies ORDER BY business,role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelRoutingPolicy{}
	for rows.Next() {
		var p ModelRoutingPolicy
		var raw string
		if err := rows.Scan(&p.Business, &p.Role, &p.Revision, &p.AllowCrossProvider, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &p.Candidates); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func sameModelRoutingCandidates(a, b []ModelRoutingCandidate) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ProfileID != b[i].ProfileID || a[i].ProfileRevision != b[i].ProfileRevision || a[i].CredentialBinding != b[i].CredentialBinding {
			return false
		}
	}
	return true
}

func (s *Store) SaveModelRoutingPolicy(ctx context.Context, p ModelRoutingPolicy) (ModelRoutingPolicy, error) {
	if !ValidModelRoutingScope(p.Business, p.Role) || p.Revision < 0 || len(p.Candidates) > MaxModelRoutingCandidates {
		return ModelRoutingPolicy{}, ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelRoutingPolicy{}, err
	}
	defer tx.Rollback()
	old, err := readModelRoutingPolicy(ctx, tx, p.Business, p.Role)
	if err != nil {
		return ModelRoutingPolicy{}, err
	}
	if old.Revision != p.Revision {
		return ModelRoutingPolicy{}, ErrModelSettingsRevision
	}
	seen := map[string]bool{}
	provider := ""
	for _, c := range p.Candidates {
		if c.ProfileID == "" || c.ProfileRevision < 1 || c.CredentialBinding == "" || seen[c.ProfileID] {
			return ModelRoutingPolicy{}, ErrState
		}
		seen[c.ProfileID] = true
		profile, err := s.scanModelProfile(tx.QueryRowContext(ctx, "SELECT "+modelProfileCols+" FROM model_connection_profiles WHERE profile_id=?", c.ProfileID))
		if errors.Is(err, sql.ErrNoRows) {
			return ModelRoutingPolicy{}, ErrModelSettingsRevision
		}
		if err != nil {
			return ModelRoutingPolicy{}, err
		}
		if profile.Settings.Revision != c.ProfileRevision || !profile.Enabled || !profile.ProviderEnabled {
			return ModelRoutingPolicy{}, ErrModelSettingsRevision
		}
		if provider != "" && provider != profile.ProviderID && !p.AllowCrossProvider {
			return ModelRoutingPolicy{}, ErrState
		}
		provider = profile.ProviderID
	}
	if p.Candidates == nil {
		p.Candidates = []ModelRoutingCandidate{}
	}
	if len(p.Candidates) == 0 {
		p.AllowCrossProvider = false
	}
	if old.AllowCrossProvider == p.AllowCrossProvider && sameModelRoutingCandidates(old.Candidates, p.Candidates) {
		return old, nil
	}
	raw, err := json.Marshal(p.Candidates)
	if err != nil {
		return ModelRoutingPolicy{}, err
	}
	p.Revision++
	_, err = tx.ExecContext(ctx, `INSERT INTO model_routing_policies(business,role,revision,allow_cross_provider,candidates) VALUES(?,?,?,?,?)
ON CONFLICT(business,role) DO UPDATE SET revision=excluded.revision,allow_cross_provider=excluded.allow_cross_provider,candidates=excluded.candidates`,
		p.Business, p.Role, p.Revision, p.AllowCrossProvider, string(raw))
	if err != nil {
		return ModelRoutingPolicy{}, err
	}
	if err = recordAudit(tx, AuditEvent{Event: "model_routing_policy_changed", Actor: "user:session", DetailRef: p.Business + ":" + p.Role,
		Summary: "模型路由策略已更新", OccurredAt: now()}); err != nil {
		return ModelRoutingPolicy{}, err
	}
	return p, tx.Commit()
}
