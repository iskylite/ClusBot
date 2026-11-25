package work

import (
	"database/sql"
	"encoding/json"
	"errors"
)

const accountRSISkillSchema = `
CREATE TABLE IF NOT EXISTS account_rsi_skills (
 skill_id TEXT PRIMARY KEY REFERENCES personal_skills(id) ON DELETE CASCADE,
 shared_base_id TEXT NOT NULL DEFAULT '',
 shared_base_version INTEGER NOT NULL DEFAULT 0,
 shared_base_digest TEXT NOT NULL DEFAULT '',
 CHECK ((shared_base_id='' AND shared_base_version=0 AND shared_base_digest='')
 OR (shared_base_id<>'' AND shared_base_version>0 AND length(shared_base_digest)=64))
);`

func migrateAccountRSISkills(tx *sql.Tx) error {
	_, err := tx.Exec(accountRSISkillSchema)
	return err
}

type rsiProposalBase struct {
	SkillID       string
	Revision      int
	Version       sql.NullInt64
	Instructions  string
	SharedSkillID string
	SharedVersion int
	SharedDigest  string
}

func accountOwnsRSIStore(tx *sql.Tx) (bool, error) {
	var personal bool
	err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM account_business_owner WHERE singleton=1)").Scan(&personal)
	return personal, err
}

// readRSIProposalBase reads the private active tier first. A first private
// draft may depend on the currently approved shared ordinary Skill, but never
// changes its row or snapshots a second independently authorized copy.
func readRSIProposalBase(tx *sql.Tx, scope, goal string) (rsiProposalBase, error) {
	var b rsiProposalBase
	personal, err := accountOwnsRSIStore(tx)
	if err != nil {
		return b, err
	}
	if !personal {
		err = tx.QueryRow("SELECT id,revision,active_version FROM personal_skills WHERE scope_label=?", scope).
			Scan(&b.SkillID, &b.Revision, &b.Version)
	} else {
		err = tx.QueryRow(`SELECT s.id,s.revision,s.active_version,k.shared_base_id,k.shared_base_version,k.shared_base_digest
FROM main.personal_skills s JOIN main.account_rsi_skills k ON k.skill_id=s.id WHERE s.scope_label=?`, scope).
			Scan(&b.SkillID, &b.Revision, &b.Version, &b.SharedSkillID, &b.SharedVersion, &b.SharedDigest)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRow(`SELECT s.id,v.version,v.sha256,v.instructions
FROM personal_skills s JOIN personal_skill_versions v ON v.skill_id=s.id AND v.version=s.active_version
WHERE s.scope_label=? AND v.approved_at IS NOT NULL
AND NOT EXISTS(SELECT 1 FROM main.account_rsi_skills k WHERE k.skill_id=s.id)`, scope).
				Scan(&b.SharedSkillID, &b.SharedVersion, &b.SharedDigest, &b.Instructions)
			if err == nil && (b.SharedDigest == "" || digest(b.Instructions) != b.SharedDigest) {
				return b, ErrExperienceSource
			}
			if errors.Is(err, sql.ErrNoRows) {
				err = nil
			}
			return b, err
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return b, nil
	}
	if err != nil {
		return b, err
	}
	if b.Version.Valid {
		b.Instructions, err = applicableSkillInstructions(tx, b.SkillID, int(b.Version.Int64), goal)
	} else if personal {
		b.Instructions, err = rsiPrivateBaseInstructions(tx, b.SkillID)
	}
	return b, err
}

// prepareRSIProposalSkill runs only after the proposal's evidence and base have
// been validated, in the same transaction as writing the draft.
func prepareRSIProposalSkill(tx *sql.Tx, scope string, base rsiProposalBase) error {
	personal, err := accountOwnsRSIStore(tx)
	if err != nil || !personal || base.SkillID != "" {
		return err
	}
	key := id()
	if _, err = tx.Exec("INSERT INTO main.personal_skills(id,scope_label,revision) VALUES(?,?,0)", key, scope); err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO main.account_rsi_skills VALUES(?,?,?,?)", key, base.SharedSkillID, base.SharedVersion, base.SharedDigest)
	return err
}

// rsiPrivateBaseInstructions follows the pinned shared grant rather than
// storing shared body copies in private DB. Disabling or rolling shared back
// below the pin revokes the dependent private Skill.
func rsiPrivateBaseInstructions(tx *sql.Tx, skill string) (string, error) {
	var scope, shared, hash string
	var version int
	err := tx.QueryRow(`SELECT s.scope_label,k.shared_base_id,k.shared_base_version,k.shared_base_digest
FROM main.account_rsi_skills k JOIN main.personal_skills s ON s.id=k.skill_id WHERE k.skill_id=?`, skill).
		Scan(&scope, &shared, &version, &hash)
	if errors.Is(err, sql.ErrNoRows) || err == nil && shared == "" {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var text, currentDigest string
	err = tx.QueryRow(`SELECT v.instructions,v.sha256 FROM personal_skills s JOIN personal_skill_versions v ON v.skill_id=s.id
WHERE s.id=? AND s.scope_label=? AND v.version=? AND v.approved_at IS NOT NULL AND s.active_version>=v.version
AND NOT EXISTS(SELECT 1 FROM main.account_rsi_skills k WHERE k.skill_id=s.id)`, shared, scope, version).Scan(&text, &currentDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrExperienceSource
	}
	if err != nil {
		return "", err
	}
	if currentDigest != hash || digest(text) != hash {
		return "", ErrExperienceSource
	}
	return text, nil
}

// privateRSISkillMatches reports whether at least one active-chain addition
// matches the real task. An unrelated private method cannot shadow an ordinary
// shared Skill even if an ancestor has text for a different task.
func privateRSISkillMatches(tx *sql.Tx, skill string, version int, goal string) (bool, error) {
	for version > 0 {
		var base sql.NullInt64
		var proposalGoal, mode, raw string
		err := tx.QueryRow(`SELECT base_version,goal,match_mode,match_keywords FROM skill_proposals WHERE skill_id=? AND version=?`,
			skill, version).Scan(&base, &proposalGoal, &mode, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		var keywords []string
		if err := json.Unmarshal([]byte(raw), &keywords); err != nil {
			return false, err
		}
		if skillProposalMatchesGoal(mode, keywords, proposalGoal, goal) {
			return true, nil
		}
		if !base.Valid {
			return false, nil
		}
		if base.Int64 >= int64(version) {
			return false, ErrState
		}
		version = int(base.Int64)
	}
	return false, nil
}

func defaultSkillForTask(tx *sql.Tx, task string) (string, int, string, error) {
	rows, err := tx.Query(`SELECT s.id,s.active_version,t.goal,k.skill_id IS NOT NULL
FROM personal_skills s JOIN tasks t ON t.scope_label=s.scope_label
LEFT JOIN main.account_rsi_skills k ON k.skill_id=s.id
WHERE t.id=? AND s.active_version IS NOT NULL
ORDER BY k.skill_id IS NOT NULL DESC,s.id`, task)
	if err != nil {
		return "", 0, "", err
	}
	type option struct {
		id, goal string
		version  int
		private  bool
	}
	var choices []option
	for rows.Next() {
		var c option
		if err := rows.Scan(&c.id, &c.version, &c.goal, &c.private); err != nil {
			rows.Close()
			return "", 0, "", err
		}
		choices = append(choices, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", 0, "", err
	}
	for _, c := range choices {
		if c.private {
			matched, err := privateRSISkillMatches(tx, c.id, c.version, c.goal)
			if err != nil {
				return "", 0, "", err
			}
			if !matched {
				continue
			}
			body, err := applicableSkillInstructions(tx, c.id, c.version, c.goal)
			if errors.Is(err, ErrExperienceSource) || err == nil && body == "" {
				continue // Never reuse revoked content on a new, default-selected run.
			}
			if err != nil {
				return "", 0, "", err
			}
		}
		return c.id, c.version, "scope_default", nil
	}
	return "", 0, "", sql.ErrNoRows
}
