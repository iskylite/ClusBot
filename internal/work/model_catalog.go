package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

var ErrModelDefaultUnavailable = errors.New("所选默认模型不可用，请检查模型及提供方状态")

type ModelProvider struct {
	ID, Name, Protocol, BaseURL, KeyRef string
	APIKey                              string `json:"-"`
	Revision, ModelCount                int
	Enabled                             bool
}

func (s *Store) scanModelProvider(row scanner) (ModelProvider, error) {
	var p ModelProvider
	var ciphertext []byte
	err := row.Scan(&p.ID, &p.Name, &p.Revision, &p.Protocol, &p.BaseURL, &p.KeyRef, &p.Enabled, &p.ModelCount, &ciphertext)
	if err == nil {
		p.APIKey = s.openModelProviderAPIKey(p.ID, ciphertext)
	}
	return p, err
}

const modelProviderCols = `provider_id,name,revision,protocol,base_url,key_ref,enabled,
 (SELECT COUNT(*) FROM model_connection_profiles m WHERE m.provider_id=model_providers.provider_id),api_key_ciphertext`

func (s *Store) ReadModelProvider(ctx context.Context, key string) (ModelProvider, error) {
	return s.scanModelProvider(s.db.QueryRowContext(ctx, "SELECT "+modelProviderCols+" FROM model_providers WHERE provider_id=?", key))
}

func (s *Store) ModelProviders(ctx context.Context) ([]ModelProvider, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+modelProviderCols+" FROM model_providers ORDER BY name,provider_id LIMIT ?", MaxModelProviders)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelProvider{}
	for rows.Next() {
		p, err := s.scanModelProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) SaveModelProvider(ctx context.Context, p ModelProvider) (ModelProvider, error) {
	if (p.KeyRef == ModelProviderAPIKeyRef) != (p.APIKey != "") {
		return ModelProvider{}, ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelProvider{}, err
	}
	defer tx.Rollback()
	var duplicate bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM model_providers WHERE name=? COLLATE NOCASE AND provider_id<>?)`, p.Name, p.ID).Scan(&duplicate); err != nil {
		return ModelProvider{}, err
	}
	if duplicate {
		return ModelProvider{}, ErrConflict
	}
	if p.ID == "" {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM model_providers`).Scan(&n); err != nil {
			return ModelProvider{}, err
		}
		if n >= MaxModelProviders {
			return ModelProvider{}, ErrState
		}
		p.ID, p.Revision = id(), 1
		ciphertext, err := s.sealModelProviderAPIKey(p.ID, p.APIKey)
		if err != nil {
			return ModelProvider{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO model_providers(provider_id,name,revision,protocol,base_url,key_ref,enabled,api_key_ciphertext) VALUES(?,?,?,?,?,?,?,?)`, p.ID, p.Name, p.Revision, p.Protocol, p.BaseURL, p.KeyRef, p.Enabled, ciphertext); err != nil {
			return ModelProvider{}, err
		}
	} else {
		prev, err := s.scanModelProvider(tx.QueryRowContext(ctx, "SELECT "+modelProviderCols+" FROM model_providers WHERE provider_id=?", p.ID))
		if errors.Is(err, sql.ErrNoRows) {
			return ModelProvider{}, ErrModelSettingsRevision
		}
		if err != nil {
			return ModelProvider{}, err
		}
		if prev.Revision != p.Revision {
			return ModelProvider{}, ErrModelSettingsRevision
		}
		ciphertext, err := s.sealModelProviderAPIKey(p.ID, p.APIKey)
		if err != nil {
			return ModelProvider{}, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE model_providers SET name=?,protocol=?,base_url=?,key_ref=?,enabled=?,api_key_ciphertext=?,revision=revision+1 WHERE provider_id=? AND revision=?`, p.Name, p.Protocol, p.BaseURL, p.KeyRef, p.Enabled, ciphertext, p.ID, p.Revision)
		if err != nil {
			return ModelProvider{}, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return ModelProvider{}, err
		}
		if n != 1 {
			return ModelProvider{}, ErrModelSettingsRevision
		}
		if prev.Protocol != p.Protocol || prev.BaseURL != p.BaseURL || prev.KeyRef != p.KeyRef || prev.APIKey != p.APIKey || prev.Enabled != p.Enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE model_connection_profiles SET revision=revision+1 WHERE provider_id=?`, p.ID); err != nil {
				return ModelProvider{}, err
			}
		}
		p.Revision++
		p.ModelCount = prev.ModelCount
	}
	return p, tx.Commit()
}

func (s *Store) DeleteModelProvider(ctx context.Context, key string, revision int) error {
	// A provider with models cannot be deleted accidentally. Removing a model
	// leaves its explicit role/default references intact and unavailable.
	res, err := s.db.ExecContext(ctx, `DELETE FROM model_providers WHERE provider_id=? AND revision=? AND NOT EXISTS (SELECT 1 FROM model_connection_profiles WHERE provider_id=?)`, key, revision, key)
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

// SaveCatalogModel associates the existing profile identity with a provider.
// The provider version is checked within the same transaction.
func (s *Store) SaveCatalogModel(ctx context.Context, p ModelConnectionProfile) (ModelConnectionProfile, error) {
	if p.Settings.APIKey != "" || p.ProviderID == "" || p.ProviderRevision < 1 {
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
	provider, err := s.scanModelProvider(tx.QueryRowContext(ctx, "SELECT "+modelProviderCols+" FROM model_providers WHERE provider_id=?", p.ProviderID))
	if errors.Is(err, sql.ErrNoRows) {
		return ModelConnectionProfile{}, ErrModelSettingsRevision
	}
	if err != nil {
		return ModelConnectionProfile{}, err
	}
	if provider.Revision != p.ProviderRevision {
		return ModelConnectionProfile{}, ErrModelSettingsRevision
	}
	var duplicate bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM model_connection_profiles WHERE profile_id<>? AND (name=? COLLATE NOCASE OR (provider_id=? AND model_id=?)))`, p.ID, p.Name, p.ProviderID, p.Settings.ModelID).Scan(&duplicate); err != nil {
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
		p.ID = id()
		_, err = tx.ExecContext(ctx, `INSERT INTO model_connection_profiles(profile_id,name,revision,protocol,base_url,model_id,key_ref,attributes,provider_id,enabled) VALUES(?,?,1,?,?,?,?,?,?,?)`, p.ID, p.Name, provider.Protocol, provider.BaseURL, v.ModelID, provider.KeyRef, string(attrs), p.ProviderID, p.Enabled)
		if err != nil {
			return ModelConnectionProfile{}, err
		}
	} else {
		res, err := tx.ExecContext(ctx, `UPDATE model_connection_profiles SET name=?,protocol=?,base_url=?,model_id=?,key_ref=?,attributes=?,provider_id=?,enabled=?,revision=revision+1 WHERE profile_id=? AND revision=?`, p.Name, provider.Protocol, provider.BaseURL, v.ModelID, provider.KeyRef, string(attrs), p.ProviderID, p.Enabled, p.ID, v.Revision)
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

type ModelDefaultReference struct {
	Revision  int
	ProfileID string
}

func (s *Store) ModelDefault(ctx context.Context) (ModelDefaultReference, error) {
	var d ModelDefaultReference
	err := s.db.QueryRowContext(ctx, `SELECT revision,profile_id FROM model_default_selection WHERE id=1`).Scan(&d.Revision, &d.ProfileID)
	return d, err
}

func (s *Store) SetModelDefault(ctx context.Context, d ModelDefaultReference, profileRevision int) (ModelDefaultReference, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelDefaultReference{}, err
	}
	defer tx.Rollback()
	if d.ProfileID != "" {
		p, err := s.scanModelProfile(tx.QueryRowContext(ctx, "SELECT "+modelProfileCols+" FROM model_connection_profiles WHERE profile_id=?", d.ProfileID))
		if errors.Is(err, sql.ErrNoRows) {
			return ModelDefaultReference{}, ErrModelSettingsRevision
		}
		if err != nil {
			return ModelDefaultReference{}, err
		}
		if p.Settings.Revision != profileRevision {
			return ModelDefaultReference{}, ErrModelSettingsRevision
		}
		if !p.Enabled || !p.ProviderEnabled {
			return ModelDefaultReference{}, ErrModelDefaultUnavailable
		}
	} else if profileRevision != 0 {
		return ModelDefaultReference{}, ErrState
	}
	res, err := tx.ExecContext(ctx, `UPDATE model_default_selection SET profile_id=?,revision=revision+1 WHERE id=1 AND revision=?`, d.ProfileID, d.Revision)
	if err != nil {
		return ModelDefaultReference{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return ModelDefaultReference{}, err
	} else if n != 1 {
		return ModelDefaultReference{}, ErrModelSettingsRevision
	}
	d.Revision++
	return d, tx.Commit()
}

// EffectiveModelSettings resolves the stable selection on each call; deletion
// or disabling never causes an implicit fallback to the legacy default.
func (s *Store) EffectiveModelSettings(ctx context.Context) (ModelSettings, ModelDefaultReference, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelSettings{}, ModelDefaultReference{}, err
	}
	defer tx.Rollback()
	var d ModelDefaultReference
	if err := tx.QueryRowContext(ctx, `SELECT revision,profile_id FROM model_default_selection WHERE id=1`).Scan(&d.Revision, &d.ProfileID); err != nil {
		return ModelSettings{}, d, err
	}
	var v ModelSettings
	if d.ProfileID != "" {
		p, err := s.scanModelProfile(tx.QueryRowContext(ctx, "SELECT "+modelProfileCols+" FROM model_connection_profiles WHERE profile_id=?", d.ProfileID))
		if errors.Is(err, sql.ErrNoRows) {
			return ModelSettings{}, d, ErrModelDefaultUnavailable
		}
		if err != nil {
			return ModelSettings{}, d, err
		}
		if !p.Enabled || !p.ProviderEnabled {
			return ModelSettings{}, d, ErrModelDefaultUnavailable
		}
		v = p.Settings
	} else {
		var attributes string
		err := tx.QueryRowContext(ctx, `SELECT base_url,model_id,api_key_ref,protocol,revision,api_key,COALESCE((SELECT attributes FROM model_attributes WHERE id=1),'null') FROM model_settings WHERE id=1`).Scan(&v.BaseURL, &v.ModelID, &v.KeyRef, &v.Protocol, &v.Revision, &v.APIKey, &attributes)
		if err != nil {
			return ModelSettings{}, d, err
		}
		if err := json.Unmarshal([]byte(attributes), &v.Attributes); err != nil {
			return ModelSettings{}, d, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ModelSettings{}, d, err
	}
	return v, d, nil
}
