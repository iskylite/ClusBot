package work

import (
	"database/sql"
	_ "embed"
	"encoding/json"
)

// RouterModelProvenance records the model selected for one admission, not
// proof of which model executed. It never contains routing input or credentials.
type RouterModelProvenance struct {
	Source          string `json:"source,omitempty"`
	RoleRevision    int    `json:"role_revision,omitempty"`
	ProfileID       string `json:"profile_id,omitempty"`
	ProfileRevision int    `json:"profile_revision,omitempty"`
	DefaultRevision int    `json:"default_revision,omitempty"`
	Protocol        string `json:"protocol,omitempty"`
	RequestedModel  string `json:"requested_model,omitempty"`
	ReportedModel   string `json:"reported_model,omitempty"`
	State           string `json:"state"`
}

//go:embed submission_classifications.sql
var submissionClassificationsSchema string

func migrateRouterModelProvenance(tx *sql.Tx) error {
	// Older installations upgraded through the baseline migrations may lack
	// the table entirely: it previously existed only in the bootstrap schema.
	if _, err := tx.Exec(submissionClassificationsSchema); err != nil {
		return err
	}
	return addColumnIfMissing(tx, "submission_classifications", "router_model_provenance", `ALTER TABLE submission_classifications ADD COLUMN router_model_provenance TEXT NOT NULL DEFAULT ''`)
}

func decodeRouterModelProvenance(raw string) (*RouterModelProvenance, error) {
	if raw == "" {
		return nil, nil
	}
	var value RouterModelProvenance
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, err
	}
	return &value, nil
}
