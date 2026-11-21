package model

import (
	"context"
	"errors"
	"strings"
	"time"

	"clusbot/internal/work"
)

// TestCatalogModelReply sends a fixed text request, separate from the legacy
// adapter test that also exercises tool calls. It does not change model metadata.
func (s *Settings) TestCatalogModelReply(ctx context.Context, id string, revision int) (time.Duration, error) {
	if s == nil || s.Store == nil || revision < 1 {
		return 0, InvalidSettings("模型设置或目录版本无效")
	}
	profile, err := s.Store.ReadModelProfile(ctx, id)
	if err != nil {
		return 0, err
	}
	value := profile.Settings
	if value.Revision != revision {
		return 0, work.ErrModelSettingsRevision
	}
	key := keyValue(value)
	if !profile.Enabled || !profile.ProviderEnabled || !credentialAvailableWithKey(value, key) {
		return 0, InvalidSettings("模型、提供方或凭据不可用")
	}
	if err := validateProfileSettings(value); err != nil {
		return 0, err
	}
	provider, err := s.providerForValue(value, key)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := time.Now()
	result, err := provider.readText(ctx, responseRequest{
		Instructions:    "Reply with OK.",
		Input:           []any{responseMessage{Type: "message", Role: "user", Content: "ClusBot connectivity check."}},
		MaxOutputTokens: 32, noRetry: true,
	})
	if err == nil && strings.TrimSpace(result) == "" {
		err = errors.New("empty model reply")
	}
	if err != nil {
		return 0, probeFailure(ctx, "模型试答", err)
	}
	return time.Since(start), nil
}
