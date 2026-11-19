package work

import (
	"context"
	"database/sql"
	"strings"
)

// ModelReceipt is provider-reported metadata, not independently verified identity.
type ModelReceipt struct {
	Protocol      string `json:"protocol"`
	State         string `json:"state"`
	ReportedModel string `json:"reported_model,omitempty"`
}

func ValidModelReceiptIdentifier(model string) bool {
	if len(model) == 0 || len(model) > 128 || strings.Contains(model, "://") || strings.HasPrefix(model, "/") {
		return false
	}
	lower := strings.ToLower(model)
	for _, prefix := range []string{"sk-", "sk_", "ghp_", "github_pat_", "xoxb-", "xoxp-"} {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	for i, ch := range model {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			continue
		}
		if i == 0 || !strings.ContainsRune("._:/-", ch) {
			return false
		}
	}
	return true
}

func migrateModelReceipts(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS model_receipts (
 manifest_id INTEGER PRIMARY KEY REFERENCES context_manifests(id) ON DELETE CASCADE,
 protocol TEXT NOT NULL CHECK(protocol IN ('responses','chat_completions')),
 state TEXT NOT NULL CHECK(state IN ('reported','not_reported','conflicting')),
 reported_model TEXT NOT NULL DEFAULT ''
)`)
	return err
}

func (s *Store) recordModelReceipt(ctx context.Context, manifestID int64, run Run, receipt ModelReceipt) error {
	if (receipt.Protocol != "responses" && receipt.Protocol != "chat_completions") ||
		(receipt.State != "reported" && receipt.State != "not_reported" && receipt.State != "conflicting") ||
		(receipt.State == "reported" && !ValidModelReceiptIdentifier(receipt.ReportedModel)) ||
		(receipt.State != "reported" && receipt.ReportedModel != "") {
		return ErrState
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO model_receipts(manifest_id,protocol,state,reported_model)
SELECT id,?,?,? FROM context_manifests WHERE id=? AND run_id=? AND generation=?
ON CONFLICT(manifest_id) DO NOTHING`, receipt.Protocol, receipt.State, receipt.ReportedModel, manifestID, run.ID, run.Generation)
	return err
}
