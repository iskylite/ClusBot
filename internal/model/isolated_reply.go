package model

import (
	"context"

	"clusbot/internal/work"
)

func (s *Settings) isolatedReplyProfile(ctx context.Context, id string, revision int) (work.ModelSettings, string, error) {
	if s == nil || s.Store == nil || s.Demo {
		return work.ModelSettings{}, "", work.ErrIsolatedReplyUnavailable
	}
	profile, err := s.Store.ReadModelProfile(ctx, id)
	if err != nil || !profile.Enabled || !profile.ProviderEnabled || profile.Settings.Revision != revision || validateProfileSettings(profile.Settings) != nil {
		return work.ModelSettings{}, "", work.ErrIsolatedReplyUnavailable
	}
	key := keyValue(profile.Settings)
	if !credentialAvailableWithKey(profile.Settings, key) {
		return work.ModelSettings{}, "", work.ErrIsolatedReplyUnavailable
	}
	return profile.Settings, key, nil
}

func (s *Settings) IsolatedReplyCredential(ctx context.Context, id string, revision int) (string, error) {
	_, key, err := s.isolatedReplyProfile(ctx, id, revision)
	if err != nil {
		return "", err
	}
	return s.Store.ModelCredentialBinding(key), nil
}

func (s *Settings) IsolatedReplyProvider(ctx context.Context, pin work.IsolatedReplyPin) (work.Provider, error) {
	v, key, err := s.isolatedReplyProfile(ctx, pin.ProfileID, pin.ProfileRevision)
	if err != nil || s.Store.ModelCredentialBinding(key) != pin.CredentialBinding {
		return nil, work.ErrIsolatedReplyUnavailable
	}
	p, err := s.providerForValue(v, key)
	if err != nil {
		return nil, work.ErrIsolatedReplyUnavailable
	}
	return isolatedTextStream{p}, nil
}

type isolatedTextStream struct{ p *Provider }

func (s isolatedTextStream) Stream(ctx context.Context, input []work.Message, task bool, limit int, emit func(string) error) (*work.TokenUsage, error) {
	if task || len(input) != 1 || input[0].Role != "user" || input[0].ID < 1 {
		return nil, work.ErrIsolatedReplyUnavailable
	}
	message := work.Message{ID: input[0].ID, Role: "user", Text: input[0].Text, RequiredInput: true}
	req, err := conversationRequest([]work.Message{message}, false, limit)
	if err != nil {
		return nil, err
	}
	// Do not retry a possibly-sent one-shot request at the transport layer.
	req.noRetry = true
	result, err := s.p.requestStream(ctx, req, emit)
	return normalizedUsage(result.Usage), err
}
