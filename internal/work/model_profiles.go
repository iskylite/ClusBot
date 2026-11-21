package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

const MaxModelProfiles = 20
const MaxModelProviders = 200

func migrateModelProfiles(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS model_connection_profiles (
 profile_id TEXT PRIMARY KEY, name TEXT NOT NULL COLLATE NOCASE UNIQUE, revision INTEGER NOT NULL,
 protocol TEXT NOT NULL, base_url TEXT NOT NULL, model_id TEXT NOT NULL, key_ref TEXT NOT NULL,
 attributes TEXT NOT NULL
)`)
	return err
}

// migrateModelProviderCatalog preserves each legacy profile's identity and
// connection independently. In particular, equal endpoints do not imply that
// credentials or the user's intended provider are the same.
func migrateModelProviderCatalog(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS model_providers (
 provider_id TEXT PRIMARY KEY, name TEXT NOT NULL COLLATE NOCASE UNIQUE,
 revision INTEGER NOT NULL, protocol TEXT NOT NULL, base_url TEXT NOT NULL,
 key_ref TEXT NOT NULL, enabled INTEGER NOT NULL CHECK(enabled IN (0,1))
);
CREATE TABLE IF NOT EXISTS model_default_selection (
 id INTEGER PRIMARY KEY CHECK(id=1), revision INTEGER NOT NULL, profile_id TEXT NOT NULL
);
INSERT OR IGNORE INTO model_default_selection(id,revision,profile_id) VALUES(1,0,'');`); err != nil {
		return err
	}
	if err := addColumnIfMissing(tx, "model_connection_profiles", "provider_id", `ALTER TABLE model_connection_profiles ADD COLUMN provider_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := addColumnIfMissing(tx, "model_connection_profiles", "enabled", `ALTER TABLE model_connection_profiles ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1))`); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT profile_id,name,protocol,base_url,key_ref FROM model_connection_profiles WHERE provider_id='' ORDER BY profile_id`)
	if err != nil {
		return err
	}
	type oldProfile struct{ id, name, protocol, baseURL, keyRef string }
	var old []oldProfile
	for rows.Next() {
		var p oldProfile
		if err = rows.Scan(&p.id, &p.name, &p.protocol, &p.baseURL, &p.keyRef); err != nil {
			rows.Close()
			return err
		}
		old = append(old, p)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, p := range old {
		providerID := id()
		name, err := availableProviderName(tx, p.name, providerID)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO model_providers(provider_id,name,revision,protocol,base_url,key_ref,enabled) VALUES(?,?,1,?,?,?,1)`, providerID, name, p.protocol, p.baseURL, p.keyRef); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE model_connection_profiles SET provider_id=? WHERE profile_id=? AND provider_id=''`, providerID, p.id); err != nil {
			return err
		}
	}
	return nil
}

type ModelConnectionProfile struct {
	ID               string
	Name             string
	ProviderID       string
	ProviderRevision int
	Enabled          bool
	ProviderEnabled  bool
	Settings         ModelSettings
}

// Legacy connection columns remain on disk for backward-compatible migrations;
// all operational reads use the current shared provider's settings.
const modelProfileCols = `profile_id,name,revision,
 COALESCE((SELECT protocol FROM model_providers p WHERE p.provider_id=model_connection_profiles.provider_id),protocol),
 COALESCE((SELECT base_url FROM model_providers p WHERE p.provider_id=model_connection_profiles.provider_id),base_url),
 model_id,COALESCE((SELECT key_ref FROM model_providers p WHERE p.provider_id=model_connection_profiles.provider_id),key_ref),attributes,
 provider_id,enabled,COALESCE((SELECT enabled FROM model_providers p WHERE p.provider_id=model_connection_profiles.provider_id),0),
 COALESCE((SELECT revision FROM model_providers p WHERE p.provider_id=model_connection_profiles.provider_id),0),
 (SELECT api_key_ciphertext FROM model_providers p WHERE p.provider_id=model_connection_profiles.provider_id)`

func (s *Store) scanModelProfile(row scanner) (ModelConnectionProfile, error) {
	var p ModelConnectionProfile
	var attrs string
	var ciphertext []byte
	err := row.Scan(&p.ID, &p.Name, &p.Settings.Revision, &p.Settings.Protocol, &p.Settings.BaseURL, &p.Settings.ModelID, &p.Settings.KeyRef, &attrs,
		&p.ProviderID, &p.Enabled, &p.ProviderEnabled, &p.ProviderRevision, &ciphertext)
	if err == nil {
		err = json.Unmarshal([]byte(attrs), &p.Settings.Attributes)
		p.Settings.APIKey = s.openModelProviderAPIKey(p.ProviderID, ciphertext)
	}
	return p, err
}

func (s *Store) ReadModelProfile(ctx context.Context, key string) (ModelConnectionProfile, error) {
	return s.scanModelProfile(s.db.QueryRowContext(ctx, "SELECT "+modelProfileCols+" FROM model_connection_profiles WHERE profile_id=?", key))
}

func (s *Store) ModelProfiles(ctx context.Context) ([]ModelConnectionProfile, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+modelProfileCols+" FROM model_connection_profiles ORDER BY name,profile_id LIMIT ?", MaxModelProfiles)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelConnectionProfile{}
	for rows.Next() {
		p, err := s.scanModelProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func availableProviderName(tx *sql.Tx, name, providerID string) (string, error) {
	var exists bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM model_providers WHERE name=? COLLATE NOCASE)`, name).Scan(&exists); err != nil {
		return "", err
	}
	if exists {
		return fmt.Sprintf("%s (%s)", name, providerID[:8]), nil
	}
	return name, nil
}

// The legacy profile API retains its independent-connection semantics. If an
// existing profile's connection is changed, it moves to its own provider.
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
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM model_connection_profiles WHERE name=? COLLATE NOCASE AND profile_id<>?)`, p.Name, p.ID).Scan(&duplicate); err != nil {
		return ModelConnectionProfile{}, err
	}
	if duplicate {
		return ModelConnectionProfile{}, ErrConflict
	}
	var previous ModelConnectionProfile
	if p.ID != "" {
		previous, err = s.scanModelProfile(tx.QueryRowContext(ctx, "SELECT "+modelProfileCols+" FROM model_connection_profiles WHERE profile_id=?", p.ID))
		if errors.Is(err, sql.ErrNoRows) {
			return ModelConnectionProfile{}, ErrModelSettingsRevision
		}
		if err != nil {
			return ModelConnectionProfile{}, err
		}
		if previous.Settings.Revision != p.Settings.Revision {
			return ModelConnectionProfile{}, ErrModelSettingsRevision
		}
	}
	v := p.Settings
	if v.KeyRef == ModelProviderAPIKeyRef && (p.ID == "" || previous.Settings.KeyRef != v.KeyRef ||
		previous.Settings.BaseURL != v.BaseURL || previous.Settings.Protocol != v.Protocol) {
		return ModelConnectionProfile{}, ErrState
	}
	if p.ID == "" || previous.Settings.Protocol != v.Protocol || previous.Settings.BaseURL != v.BaseURL || previous.Settings.KeyRef != v.KeyRef {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM model_providers`).Scan(&count); err != nil {
			return ModelConnectionProfile{}, err
		}
		if count >= MaxModelProviders {
			return ModelConnectionProfile{}, ErrState
		}
		p.ProviderID = id()
		name, err := availableProviderName(tx, p.Name, p.ProviderID)
		if err != nil {
			return ModelConnectionProfile{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO model_providers(provider_id,name,revision,protocol,base_url,key_ref,enabled) VALUES(?,?,1,?,?,?,1)`, p.ProviderID, name, v.Protocol, v.BaseURL, v.KeyRef); err != nil {
			return ModelConnectionProfile{}, err
		}
	} else {
		p.ProviderID = previous.ProviderID
	}
	if p.ID == "" {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM model_connection_profiles").Scan(&count); err != nil {
			return ModelConnectionProfile{}, err
		}
		if count >= MaxModelProfiles {
			return ModelConnectionProfile{}, ErrState
		}
		p.ID, p.Settings.Revision = id(), 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO model_connection_profiles(profile_id,name,revision,protocol,base_url,model_id,key_ref,attributes,provider_id,enabled) VALUES(?,?,1,?,?,?,?,?,?,1)`, p.ID, p.Name, v.Protocol, v.BaseURL, v.ModelID, v.KeyRef, string(attrs), p.ProviderID); err != nil {
			return ModelConnectionProfile{}, err
		}
	} else {
		res, err := tx.ExecContext(ctx, `UPDATE model_connection_profiles SET name=?,protocol=?,base_url=?,model_id=?,key_ref=?,attributes=?,provider_id=?,revision=revision+1 WHERE profile_id=? AND revision=?`, p.Name, v.Protocol, v.BaseURL, v.ModelID, v.KeyRef, string(attrs), p.ProviderID, p.ID, v.Revision)
		if err != nil {
			return ModelConnectionProfile{}, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return ModelConnectionProfile{}, err
		} else if n != 1 {
			return ModelConnectionProfile{}, ErrModelSettingsRevision
		}
	}
	out, err := s.scanModelProfile(tx.QueryRowContext(ctx, "SELECT "+modelProfileCols+" FROM model_connection_profiles WHERE profile_id=?", p.ID))
	if err != nil {
		return ModelConnectionProfile{}, err
	}
	return out, tx.Commit()
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
