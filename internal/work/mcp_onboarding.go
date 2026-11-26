package work

import (
	"context"
	"database/sql"
)

const mcpOnboardingEvent = "mcp.onboarding_requested"

// Opening setup records only a fixed, non-sensitive conversation turn. In
// particular the original free text can include a command or a private URL.
func (s *Store) OpenMCPOnboarding(ctx context.Context, actor, conversation, request string) (Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	owned, err := ownsPersonalBusiness(tx, actor)
	if err != nil {
		return Run{}, err
	}
	if !owned {
		return Run{}, ErrAuthorizationDenied
	}
	if conversation != "" {
		if err := discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
			return Run{}, err
		}
	}
	key := "mcp-onboarding:" + digest(toJSON([]string{actor, request}))
	fingerprint := digest(toJSON([]string{mcpOnboardingEvent, conversation}))
	if run, err := previousSubmission(tx, key, fingerprint); err != nil || run.ID != "" {
		return run, err
	}
	intent := submissionClassification{DelegationResult: DelegationResult{Route: &RouteDecision{Kind: "clarify", Question: "请选择 MCP 连接方式。地址、启动参数和凭据引用仅在受信表单填写；尚未创建连接。"}}}
	run, err := submitWorkTxWithMode(ctx, tx, key, fingerprint, "接入 MCP 服务", "", conversation, false, Delivery{}, &intent, nil, true)
	if err != nil {
		return Run{}, err
	}
	if err := addEvent(tx, run.ID, mcpOnboardingEvent, struct{}{}); err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}

// Both reads and actions verify the actual originating turn, not message text
// or an identifier supplied by the model.
func (s *Store) MCPOnboardingRevision(ctx context.Context, actor, conversation, run string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
		return 0, err
	}
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT c.revision FROM conversations c JOIN runs r ON r.conversation_id=c.id
	  WHERE c.id=? AND r.id=? AND EXISTS(SELECT 1 FROM run_events e WHERE e.run_id=r.id AND e.kind=?)`, conversation, run, mcpOnboardingEvent).Scan(&revision)
	return revision, err
}

// A setup turn creates at most one instance connection, even after the
// browser loses the acknowledgment or the service restarts.
func MCPOnboardingConnectionID(actor, conversation, run string) string {
	return digest(toJSON([]string{mcpOnboardingEvent, actor, conversation, run}))[:32]
}

func (s *Store) CreateMCPOnboardingConnection(ctx context.Context, actor, conversation, run string, c Connection) (Connection, error) {
	return s.saveConnection(ctx, "", c, MCPOnboardingConnectionID(actor, conversation, run))
}
