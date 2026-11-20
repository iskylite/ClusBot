package model

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"clusbot/internal/work"
)

func (s *Settings) conversationReplyProvider(ctx context.Context) (work.Provider, string, bool, error) {
	view, value, key, err := s.modelRole(ctx, work.ConversationReplyModelRole)
	if err != nil {
		return nil, "", false, err
	}
	if view.Source == "legacy_default" {
		return nil, "", false, nil
	}
	p, err := s.profileForModelRole(view, value, key, work.ErrConversationReplyRoleUnavailable)
	if err != nil {
		return nil, "", true, err
	}

	// Pin an explicit generation at the existing first checkpoint. An earlier
	// checkpoint (including legacy default, or the same profile rebound after a
	// clear) cannot be reinterpreted; an in-flight provider remains immutable.
	caps, err := json.Marshal(struct {
		Selected  work.ModelAttributes
		Inherited *roleRequestEnvelope
	}{p.attributes, p.roleEnvelope})
	if err != nil {
		return nil, "", true, err
	}
	p.checkpointScope = fmt.Sprintf("conversation_reply/v2:%d:%s:%d:%x", view.Revision, view.ProfileID, view.ProfileRevision, sha256.Sum256(caps))
	identity, _ := p.ReadCheckpointIdentity()
	return conversationReplyStream{provider: p}, identity + "\nconversation/v1", true, nil
}

// The role exposes only read-free conversation replies, not the provider's
// task planner, follow-up control, or tool-call interfaces.
type conversationReplyStream struct{ provider *Provider }

func (s conversationReplyStream) Stream(ctx context.Context, input []work.Message, task bool, limit int, emit func(string) error) (*work.TokenUsage, error) {
	if task {
		return nil, work.ErrConversationReplyRoleUnavailable
	}
	return s.provider.Stream(ctx, input, false, limit, emit)
}
