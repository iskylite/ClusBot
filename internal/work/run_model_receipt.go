package work

import (
	"context"
	"database/sql"
)

type RunModelReceiptItem struct {
	Generation     int           `json:"execution_generation"`
	RecordedAt     int64         `json:"recorded_at"`
	RequestedModel string        `json:"requested_model,omitempty"`
	Receipt        *ModelReceipt `json:"model_receipt,omitempty"`
}

type RunModelReceiptProjection struct {
	Status   string
	Usage    *ExecutionUsage
	Receipts []RunModelReceiptItem
	More     bool
}

// RunReplyOrigin pins history reads to the owning account and exact persisted
// reply. An archived conversation is still readable by the account that owns it.
type RunReplyOrigin struct {
	Actor, Conversation string
	Message             int64
}

// ReadRunModelReceipt projects only recorded model identifiers and accounting.
// Unlike RunRecord it does not load prompt text, full manifests, materials, or results into Go.
// A nil origin is the existing account-scoped Run inspection API, not the chat
// disclosure; that caller can inspect even if a Run emitted no assistant reply.
func (s *Store) ReadRunModelReceipt(ctx context.Context, run string, origin *RunReplyOrigin) (RunModelReceiptProjection, error) {
	out := RunModelReceiptProjection{Receipts: []RunModelReceiptItem{}}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	query := `SELECT r.status,r.usage_tracked,r.model_requests,r.reported_requests,r.input_tokens,r.output_tokens
FROM runs r WHERE r.id=?`
	args := []any{run}
	limit := 32
	if origin != nil {
		allowed, err := ownsPersonalBusiness(tx, origin.Actor)
		if err != nil {
			return out, err
		}
		if !allowed || origin.Conversation == "" || origin.Message < 1 {
			return out, sql.ErrNoRows
		}
		query += ` AND r.conversation_id=? AND EXISTS(SELECT 1 FROM messages reply
WHERE reply.seq=? AND reply.run_id=r.id AND reply.conversation_id=r.conversation_id AND reply.role='assistant' AND length(reply.text)>0)
AND NOT ` + revokedMCPReply("r.id")
		args = append(args, origin.Conversation, origin.Message)
		limit = 8
	}
	var tracked bool
	var reported int
	var usage ExecutionUsage
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&out.Status, &tracked, &usage.ModelRequests, &reported,
		&usage.InputTokens, &usage.OutputTokens); err != nil {
		return out, err
	}
	if tracked && usage.ModelRequests > 0 {
		usage.UnreportedRequests = max(usage.ModelRequests-reported, 0)
		out.Usage = &usage
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.generation,m.assembled_at,
CASE WHEN json_valid(m.manifest) THEN CASE WHEN json_type(m.manifest,'$.requested_model')='text'
THEN substr(json_extract(m.manifest,'$.requested_model'),1,129) ELSE '' END ELSE '' END,
coalesce(mr.protocol,''),coalesce(mr.state,''),coalesce(mr.reported_model,'')
FROM context_manifests m JOIN run_bindings b ON b.run_id=m.run_id AND b.generation=m.generation
LEFT JOIN model_receipts mr ON mr.manifest_id=m.id WHERE m.run_id=? ORDER BY m.id DESC LIMIT ?`, run, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(out.Receipts) == limit {
			out.More = true
			break
		}
		var item RunModelReceiptItem
		var receipt ModelReceipt
		if err := rows.Scan(&item.Generation, &item.RecordedAt, &item.RequestedModel, &receipt.Protocol, &receipt.State, &receipt.ReportedModel); err != nil {
			return out, err
		}
		if !ValidModelReceiptIdentifier(item.RequestedModel) {
			item.RequestedModel = ""
		}
		if !ValidModelReceiptIdentifier(receipt.ReportedModel) || receipt.State != "reported" {
			receipt.ReportedModel = ""
		}
		if receipt.State != "" {
			item.Receipt = &receipt
		}
		out.Receipts = append(out.Receipts, item)
	}
	return out, rows.Err()
}
