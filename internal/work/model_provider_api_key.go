package work

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
)

const ModelProviderAPIKeyRef = "api_key"

func migrateModelProviderAPIKey(tx *sql.Tx) error {
	return addColumnIfMissing(tx, "model_providers", "api_key_ciphertext",
		`ALTER TABLE model_providers ADD COLUMN api_key_ciphertext BLOB`)
}

func (s *Store) modelProviderAPIKeyCipher() (cipher.AEAD, error) {
	derive := hmac.New(sha256.New, s.connectionKey)
	derive.Write([]byte("clusbot/model-provider-api-key/v1"))
	block, err := aes.NewCipher(derive.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func (s *Store) sealModelProviderAPIKey(providerID, key string) ([]byte, error) {
	if key == "" {
		return nil, nil
	}
	aead, err := s.modelProviderAPIKeyCipher()
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nil, []byte(key), []byte(providerID)), nil
}

func (s *Store) openModelProviderAPIKey(providerID string, ciphertext []byte) string {
	if len(ciphertext) == 0 {
		return ""
	}
	aead, err := s.modelProviderAPIKeyCipher()
	if err != nil {
		return ""
	}
	key, err := aead.Open(nil, nil, ciphertext, []byte(providerID))
	if err != nil {
		// A lost encryption key leaves the credential unavailable and editable.
		return ""
	}
	return string(key)
}
