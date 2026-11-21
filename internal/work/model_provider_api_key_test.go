package work

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestModelProviderAPIKeyPersistsAcrossReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "catalog.db")
	const encryptionKey = "catalog-encryption-fixture"
	const apiKey = "catalog-provider-secret"
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	store.SetConnectionCredentialKey(encryptionKey)

	provider, err := store.SaveModelProvider(ctx, ModelProvider{
		Name: "provider", Protocol: "responses", BaseURL: "https://provider.invalid",
		KeyRef: ModelProviderAPIKeyRef, APIKey: apiKey, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := store.SaveCatalogModel(ctx, ModelConnectionProfile{
		Name: "profile", ProviderID: provider.ID, ProviderRevision: provider.Revision,
		Settings: ModelSettings{ModelID: "fixture-model"}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetModelDefault(ctx, ModelDefaultReference{ProfileID: profile.ID}, profile.Settings.Revision); err != nil {
		t.Fatal(err)
	}
	var ciphertext []byte
	if err := store.db.QueryRowContext(ctx, `SELECT api_key_ciphertext FROM model_providers WHERE provider_id=?`, provider.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if len(ciphertext) == 0 || bytes.Contains(ciphertext, []byte(apiKey)) {
		t.Fatal("provider key was not stored as ciphertext")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.SetConnectionCredentialKey(encryptionKey)
	settings, selection, err := reopened.EffectiveModelSettings(ctx)
	if err != nil || selection.ProfileID != profile.ID || settings.APIKey != apiKey {
		t.Fatalf("saved provider credential unavailable after reopening: %v", err)
	}

	provider.APIKey = "rotated-provider-secret"
	if _, err := reopened.SaveModelProvider(ctx, provider); err != nil {
		t.Fatal(err)
	}
	settings, _, err = reopened.EffectiveModelSettings(ctx)
	if err != nil || settings.APIKey != provider.APIKey || settings.Revision != profile.Settings.Revision+1 {
		t.Fatalf("provider rotation did not update the model and invalidate its old revision: %v", err)
	}

	provider.Revision++
	provider.KeyRef, provider.APIKey = "env:FIXTURE_MODEL_KEY", ""
	if _, err := reopened.SaveModelProvider(ctx, provider); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRowContext(ctx, `SELECT api_key_ciphertext FROM model_providers WHERE provider_id=?`, provider.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if len(ciphertext) != 0 {
		t.Fatal("provider ciphertext remains after switching to an environment reference")
	}
}
