package work

import (
	"context"
	"database/sql"
	"encoding/json"
)

const MaxModelProfiles = 20

func migrateModelProfiles(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS model_connection_profiles (
 profile_id TEXT PRIMARY KEY, name TEXT NOT NULL COLLATE NOCASE UNIQUE, revision INTEGER NOT NULL,
 protocol TEXT NOT NULL, base_url TEXT NOT NULL, model_id TEXT NOT NULL, key_ref TEXT NOT NULL,
 attributes TEXT NOT NULL
)`)
	return err
}

// ModelConnectionProfile intentionally persists only a key reference, not an
// API key. The legacy default settings row remains independent.
type ModelConnectionProfile struct {
	ID       string
	Name     string
	Settings ModelSettings
}

const modelProfileCols = "profile_id,name,revision,protocol,base_url,model_id,key_ref,attributes"

func scanModelProfile(row scanner) (ModelConnectionProfile, error) {
	var p ModelConnectionProfile
	var attrs string
	err := row.Scan(&p.ID, &p.Name, &p.Settings.Revision, &p.Settings.Protocol, &p.Settings.BaseURL, &p.Settings.ModelID, &p.Settings.KeyRef, &attrs)
	if err == nil {
		err = json.Unmarshal([]byte(attrs), &p.Settings.Attributes)
	}
	return p, err
}

func (s *Store) ReadModelProfile(ctx context.Context, key string) (ModelConnectionProfile, error) {
	return scanModelProfile(s.db.QueryRowContext(ctx, "SELECT "+modelProfileCols+" FROM model_connection_profiles WHERE profile_id=?", key))
}

func (s *Store) ModelProfiles(ctx context.Context) ([]ModelConnectionProfile, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+modelProfileCols+" FROM model_connection_profiles ORDER BY name,profile_id LIMIT ?", MaxModelProfiles)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelConnectionProfile{}
	for rows.Next() {
		p, err := scanModelProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) SaveModelProfile(ctx context.Context, p ModelConnectionProfile) (ModelConnectionProfile, error) {
	if p.Settings.APIKey != "" {
		return ModelConnectionProfile{}, ErrState
	}
	attrs, err := json.Marshal(p.Settings.Attributes)
	if err != nil {
		return ModelConnectionProfile{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelConnectionProfile{}, err
	}
	defer tx.Rollback()
	var duplicate bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM model_connection_profiles WHERE name=? COLLATE NOCASE AND profile_id<>?)`,
		p.Name, p.ID).Scan(&duplicate); err != nil {
		return ModelConnectionProfile{}, err
	}
	if duplicate {
		return ModelConnectionProfile{}, ErrConflict
	}
	v := p.Settings
	if p.ID == "" {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM model_connection_profiles").Scan(&count); err != nil {
			return ModelConnectionProfile{}, err
		}
		if count >= MaxModelProfiles {
			return ModelConnectionProfile{}, ErrState
		}
		p.ID, p.Settings.Revision = id(), 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO model_connection_profiles VALUES(?,?,1,?,?,?,?,?)`,
			p.ID, p.Name, v.Protocol, v.BaseURL, v.ModelID, v.KeyRef, string(attrs)); err != nil {
			return ModelConnectionProfile{}, err
		}
	} else {
		res, err := tx.ExecContext(ctx, `UPDATE model_connection_profiles SET name=?,protocol=?,base_url=?,model_id=?,key_ref=?,attributes=?,revision=revision+1
WHERE profile_id=? AND revision=?`, p.Name, v.Protocol, v.BaseURL, v.ModelID, v.KeyRef, string(attrs), p.ID, v.Revision)
		if err != nil {
			return ModelConnectionProfile{}, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return ModelConnectionProfile{}, err
		} else if n != 1 {
			return ModelConnectionProfile{}, ErrModelSettingsRevision
		}
		p.Settings.Revision++
	}
	return p, tx.Commit()
}

func (s *Store) DeleteModelProfile(ctx context.Context, key string, expected int) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM model_connection_profiles WHERE profile_id=? AND revision=?", key, expected)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrModelSettingsRevision
	}
	return nil
}
