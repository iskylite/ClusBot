package work

import "context"

// CreateIncident 登记新故障（症状必填；影响范围与时间线可后续补充）。
func (s *Store) CreateIncident(ctx context.Context, symptoms, impactScope string) (Incident, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Incident{}, err
	}
	defer tx.Rollback()
	incident, err := createIncident(tx, Incident{Symptoms: symptoms, ImpactScope: impactScope}, now())
	if err != nil {
		return Incident{}, err
	}
	return incident, tx.Commit()
}
