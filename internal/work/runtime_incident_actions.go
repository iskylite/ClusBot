package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// IncidentTicketPreview preserves exactly what the operator will confirm.
// Description is a human report, not proof that a diagnosis is correct.
type IncidentTicketPreview struct {
	PreviewID   string `json:"preview_id"`
	IncidentID  string `json:"incident_id"`
	ObjectRef   string `json:"object_ref"`
	Description string `json:"description"`
	ExpiresAt   int64  `json:"expires_at"`
}

func incidentAssetPermission(tx *sql.Tx, subject, ref string, stamp int64) error {
	for _, action := range []string{"read", "repair"} {
		decision, err := decideAuthorization(tx, subject, action, ref, "", stamp, false)
		if err != nil {
			return err
		}
		if decision.Outcome != "allowed" {
			return ErrAuthorizationDenied
		}
	}
	return nil
}

func incidentHasTicket(tx *sql.Tx, incident, ref string) (bool, error) {
	var exists int
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tickets WHERE incident_id=? AND object_ref=?) OR EXISTS(
SELECT 1 FROM incident_ticket_operations i JOIN operations o ON o.operation_id=i.operation_id
WHERE i.incident_id=? AND i.object_ref=? AND o.state NOT IN ('failed','cancelled'))`, incident, ref, incident, ref).Scan(&exists)
	return exists != 0, err
}

// PreviewIncidentTicket checks the target and snapshots the current state.
// The chosen asset is an explicit operator association, never inferred from
// the free-form incident scope or an unverified proposal.
func (s *Store) PreviewIncidentTicket(ctx context.Context, subject, incident, ref, description string) (IncidentTicketPreview, error) {
	description = strings.TrimSpace(description)
	if subject == "" || incident == "" || ref == "" || len(ref) > 256 || description == "" || len(description) > 8000 {
		return IncidentTicketPreview{}, errors.New("请选择资产并填写故障描述")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IncidentTicketPreview{}, err
	}
	defer tx.Rollback()
	stamp := now()
	var updated int64
	var state string
	var recovered int
	if err := tx.QueryRow(`SELECT updated_at,state,recovery_verified FROM incidents WHERE incident_id=?`, incident).Scan(&updated, &state, &recovered); err != nil {
		return IncidentTicketPreview{}, err
	}
	if state == "resolved" || state == "closed" || recovered != 0 {
		return IncidentTicketPreview{}, fmt.Errorf("%w：事件已恢复或关闭", ErrState)
	}
	if err := incidentAssetPermission(tx, subject, ref, stamp); err != nil {
		return IncidentTicketPreview{}, err
	}
	dispatch, err := decideAuthorization(tx, subject, "dispatch", "ticket.create", "", stamp, false)
	if err != nil {
		return IncidentTicketPreview{}, err
	}
	if dispatch.Outcome != "allowed" {
		return IncidentTicketPreview{}, fmt.Errorf("%w：缺少工单派发授权", ErrAuthorizationDenied)
	}
	asset, err := readAsset(tx, ref)
	if err != nil {
		return IncidentTicketPreview{}, err
	}
	if asset.State != AssetStateMaintenance && asset.State != AssetStateLent {
		return IncidentTicketPreview{}, fmt.Errorf("%w：资产须已进入维护接管或借出专项维修", ErrState)
	}
	if asset.FrozenAt > 0 {
		return IncidentTicketPreview{}, fmt.Errorf("%w：资产已冻结", ErrState)
	}
	if err := ensureLentAssetRepairAuthorized(tx, ref, subject, stamp); err != nil {
		return IncidentTicketPreview{}, err
	}
	exists, err := incidentHasTicket(tx, incident, ref)
	if err != nil {
		return IncidentTicketPreview{}, err
	}
	if exists {
		return IncidentTicketPreview{}, fmt.Errorf("%w：事件已有该资产的工单或待核对请求", ErrConflict)
	}
	p := IncidentTicketPreview{PreviewID: id(), IncidentID: incident, ObjectRef: ref, Description: description, ExpiresAt: stamp + int64(5*time.Minute/time.Millisecond)}
	_, err = tx.Exec(`INSERT INTO incident_action_previews(preview_id,incident_id,subject,action,object_ref,description,incident_updated_at,asset_identity,asset_revision,ownership_epoch,expires_at)
VALUES(?,?,?,'ticket',?,?,?,?,?,?,?)`, p.PreviewID, incident, subject, ref, description, updated, asset.Identity, asset.Revision, asset.OwnershipEpoch, p.ExpiresAt)
	if err != nil {
		return IncidentTicketPreview{}, err
	}
	return p, tx.Commit()
}

// ConfirmIncidentTicket uses the preview ID as the stable dispatch key. An
// uncertain response can be retried with that same ID without a second ticket.
func (s *Store) ConfirmIncidentTicket(ctx context.Context, subject, incident, previewID string, provider *TicketProvider) (TicketCreateResult, error) {
	if provider == nil {
		return TicketCreateResult{}, ErrTicketProviderUnavailable
	}
	var req TicketCreateRequest
	var expires int64
	err := s.db.QueryRowContext(ctx, `SELECT object_ref,description,incident_updated_at,asset_identity,asset_revision,ownership_epoch,expires_at
FROM incident_action_previews WHERE preview_id=? AND incident_id=? AND subject=? AND action='ticket'`, previewID, incident, subject).
		Scan(&req.ObjectRef, &req.FaultEvidence, &req.IncidentUpdatedAt, &req.AssetIdentity, &req.AssetRevision, &req.OwnershipEpoch, &expires)
	if err != nil {
		return TicketCreateResult{}, err
	}
	var existing int
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE idempotency_key=?)`, previewID).Scan(&existing); err != nil {
		return TicketCreateResult{}, err
	}
	if existing == 0 && now() >= expires {
		return TicketCreateResult{}, fmt.Errorf("%w：确认已过期，请重新预览", ErrConflict)
	}
	req.RequestKey, req.IncidentID, req.Subject = previewID, incident, subject
	return s.QueueTicketCreate(ctx, req, provider)
}

type IncidentTicketOption struct {
	TicketID  string `json:"ticket_id"`
	ObjectRef string `json:"object_ref"`
	State     string `json:"state"`
}

type IncidentRepairAsset struct {
	ObjectRef string `json:"object_ref"`
	Identity  string `json:"identity"`
	State     string `json:"state"`
}

type IncidentTicketOperation struct {
	OperationID string `json:"operation_id"`
	ObjectRef   string `json:"object_ref"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
}

type IncidentTicketOptions struct {
	Tickets     []IncidentTicketOption    `json:"tickets"`
	Assets      []IncidentRepairAsset     `json:"assets"`
	Operations  []IncidentTicketOperation `json:"operations"`
	CanDispatch bool                      `json:"can_dispatch"`
	NextCursor  string                    `json:"next_cursor,omitempty"`
}

// IncidentTicketOptions does not reveal tickets for assets the caller cannot
// both read and repair. The per-account store supplies the incident boundary.
func (s *Store) IncidentTicketOptions(ctx context.Context, subject, incident, after string) (IncidentTicketOptions, error) {
	out := IncidentTicketOptions{Tickets: []IncidentTicketOption{}, Assets: []IncidentRepairAsset{}, Operations: []IncidentTicketOperation{}}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM incidents WHERE incident_id=?)`, incident).Scan(&exists); err != nil {
		return out, err
	}
	if exists == 0 {
		return out, sql.ErrNoRows
	}
	rows, err := tx.Query(`SELECT ticket_id,object_ref,state FROM tickets WHERE incident_id=? ORDER BY created_at DESC,ticket_id LIMIT 100`, incident)
	if err != nil {
		return out, err
	}
	items := []IncidentTicketOption{}
	for rows.Next() {
		var item IncidentTicketOption
		if err := rows.Scan(&item.TicketID, &item.ObjectRef, &item.State); err != nil {
			rows.Close()
			return out, err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for _, item := range items {
		if err := incidentAssetPermission(tx, subject, item.ObjectRef, now()); err != nil {
			if errors.Is(err, ErrAuthorizationDenied) {
				continue
			}
			return out, err
		}
		out.Tickets = append(out.Tickets, item)
	}
	stamp := now()
	dispatch, err := decideAuthorization(tx, subject, "dispatch", "ticket.create", "", stamp, false)
	if err != nil {
		return out, err
	}
	out.CanDispatch = dispatch.Outcome == "allowed"
	rows, err = tx.Query(`SELECT i.operation_id,i.object_ref,o.state,o.failure_reason FROM incident_ticket_operations i JOIN operations o ON o.operation_id=i.operation_id WHERE i.incident_id=? AND i.subject=? ORDER BY i.created_at DESC,i.operation_id LIMIT 50`, incident, subject)
	if err != nil {
		return out, err
	}
	pending := []IncidentTicketOperation{}
	for rows.Next() {
		var op IncidentTicketOperation
		if err = rows.Scan(&op.OperationID, &op.ObjectRef, &op.State, &op.Reason); err != nil {
			rows.Close()
			return out, err
		}
		pending = append(pending, op)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for _, op := range pending {
		if err = incidentAssetPermission(tx, subject, op.ObjectRef, stamp); err != nil {
			if errors.Is(err, ErrAuthorizationDenied) {
				continue
			}
			return out, err
		}
		out.Operations = append(out.Operations, op)
	}
	rows, err = tx.Query(`SELECT a.object_ref,a.identity,a.state FROM ops_assets a
WHERE a.object_ref>? AND a.state IN (?,?) AND a.frozen_at=0
AND EXISTS(SELECT 1 FROM authorizations g WHERE g.subject=? AND g.action='read' AND g.object_ref=a.object_ref AND g.revoked_at=0 AND g.valid_from<=? AND (g.expires_at=0 OR g.expires_at>?))
AND EXISTS(SELECT 1 FROM authorizations g WHERE g.subject=? AND g.action='repair' AND g.object_ref=a.object_ref AND g.revoked_at=0 AND g.valid_from<=? AND (g.expires_at=0 OR g.expires_at>?))
ORDER BY a.object_ref LIMIT 25`, after, AssetStateMaintenance, AssetStateLent, subject, stamp, stamp, subject, stamp, stamp)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var asset IncidentRepairAsset
		if err = rows.Scan(&asset.ObjectRef, &asset.Identity, &asset.State); err != nil {
			rows.Close()
			return out, err
		}
		if len(out.Assets) < 24 {
			out.Assets = append(out.Assets, asset)
		} else {
			out.NextCursor = out.Assets[len(out.Assets)-1].ObjectRef
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

type IncidentRecheckPreview struct {
	PreviewID  string `json:"preview_id"`
	IncidentID string `json:"incident_id"`
	TicketID   string `json:"ticket_id"`
	ObjectRef  string `json:"object_ref"`
	ExpiresAt  int64  `json:"expires_at"`
}

type IncidentRecheckResult struct {
	TicketID           string `json:"ticket_id"`
	ObjectRef          string `json:"object_ref"`
	ConfirmedAt        int64  `json:"confirmed_at"`
	Duplicate          bool   `json:"duplicate,omitempty"`
	IncidentAcceptance string `json:"incident_acceptance"`
}

// A passing ticket receipt alone is insufficient. Confirmation always runs a
// new independent health check and records an asset-only recheck.
func (s *Store) PreviewIncidentRecheck(ctx context.Context, subject, incident, ticketID string, checker EffectChecker) (IncidentRecheckPreview, error) {
	if checker == nil {
		return IncidentRecheckPreview{}, errors.New("尚未配置独立设备健康验证服务")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IncidentRecheckPreview{}, err
	}
	defer tx.Rollback()
	var updated int64
	var state string
	var recovered int
	if err := tx.QueryRow(`SELECT updated_at,state,recovery_verified FROM incidents WHERE incident_id=?`, incident).Scan(&updated, &state, &recovered); err != nil {
		return IncidentRecheckPreview{}, err
	}
	if state == "closed" || state == "resolved" || recovered != 0 {
		return IncidentRecheckPreview{}, fmt.Errorf("%w：事件已恢复或关闭", ErrState)
	}
	ticket, err := readTicket(tx, ticketID)
	if err != nil {
		return IncidentRecheckPreview{}, err
	}
	if ticket.IncidentID != incident {
		return IncidentRecheckPreview{}, sql.ErrNoRows
	}
	if err := incidentAssetPermission(tx, subject, ticket.ObjectRef, now()); err != nil {
		return IncidentRecheckPreview{}, err
	}
	if ticket.State != "closed" && ticket.State != "verified" {
		return IncidentRecheckPreview{}, fmt.Errorf("%w：工单尚未关闭，不能复检", ErrState)
	}
	asset, err := readAsset(tx, ticket.ObjectRef)
	if err != nil {
		return IncidentRecheckPreview{}, err
	}
	stamp := now()
	p := IncidentRecheckPreview{PreviewID: id(), IncidentID: incident, TicketID: ticketID, ObjectRef: ticket.ObjectRef, ExpiresAt: stamp + int64(5*time.Minute/time.Millisecond)}
	_, err = tx.Exec(`INSERT INTO incident_action_previews(preview_id,incident_id,subject,action,object_ref,description,incident_updated_at,asset_identity,asset_revision,ownership_epoch,expires_at)
VALUES(?,?,?,'recheck',?,?,?,?,?,?,?)`, p.PreviewID, incident, subject, ticket.ObjectRef, ticketID, updated, asset.Identity, asset.Revision, asset.OwnershipEpoch, p.ExpiresAt)
	if err != nil {
		return IncidentRecheckPreview{}, err
	}
	return p, tx.Commit()
}

// refreshVerifiedTicketHealth queries the same independent source for a
// previously verified ticket; it does not change asset or incident state.
func (s *Store) refreshVerifiedTicketHealth(ctx context.Context, ticket Ticket, subject string, checker EffectChecker) error {
	if checker == nil {
		return errors.New("尚未配置独立设备健康验证服务")
	}
	expected, err := readAsset(s.db, ticket.ObjectRef)
	if err != nil {
		return err
	}
	verified, evidence, err := checker(ctx, ticket.ObjectRef)
	if err != nil {
		return err
	}
	if strings.TrimSpace(evidence) == "" || len(evidence) > 8000 {
		return errors.New("独立健康服务未返回可用验证证据")
	}
	if !verified {
		return fmt.Errorf("%w：独立健康检查未通过，事件仍待验收", ErrState)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := incidentAssetPermission(tx, subject, ticket.ObjectRef, now()); err != nil {
		return err
	}
	current, err := readTicket(tx, ticket.TicketID)
	if err != nil {
		return err
	}
	asset, err := readAsset(tx, ticket.ObjectRef)
	if err != nil {
		return err
	}
	if current.IncidentID != ticket.IncidentID || current.ObjectRef != ticket.ObjectRef || current.State != "verified" || current.ExternalStateVersion != ticket.ExternalStateVersion ||
		asset.Identity != expected.Identity || asset.Revision != expected.Revision || asset.OwnershipEpoch != expected.OwnershipEpoch {
		return ErrConflict
	}
	stamp := now()
	_, err = tx.Exec(`INSERT INTO ticket_health_verifications(ticket_id,object_ref,asset_identity,checked_asset_revision,asset_revision,ownership_epoch,external_state_version,evidence,verified_at)
VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(ticket_id) DO UPDATE SET object_ref=excluded.object_ref,asset_identity=excluded.asset_identity,
checked_asset_revision=excluded.checked_asset_revision,asset_revision=excluded.asset_revision,ownership_epoch=excluded.ownership_epoch,
external_state_version=excluded.external_state_version,evidence=excluded.evidence,verified_at=excluded.verified_at`,
		ticket.TicketID, ticket.ObjectRef, asset.Identity, asset.Revision, asset.Revision, asset.OwnershipEpoch, ticket.ExternalStateVersion, evidence, stamp)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ConfirmIncidentRecheck(ctx context.Context, subject, incident, previewID string, checker EffectChecker) (IncidentRecheckResult, error) {
	if checker == nil {
		return IncidentRecheckResult{}, errors.New("尚未配置独立设备健康验证服务")
	}
	var ref, ticketID, identity string
	var eventUpdated, expires int64
	var revision, epoch int
	err := s.db.QueryRowContext(ctx, `SELECT object_ref,description,incident_updated_at,asset_identity,asset_revision,ownership_epoch,expires_at
FROM incident_action_previews WHERE preview_id=? AND incident_id=? AND subject=? AND action='recheck'`, previewID, incident, subject).
		Scan(&ref, &ticketID, &eventUpdated, &identity, &revision, &epoch, &expires)
	if err != nil {
		return IncidentRecheckResult{}, err
	}
	// A completed confirmation is a receipt, even if the response was lost.
	var recorded int64
	err = s.db.QueryRowContext(ctx, `SELECT confirmed_at FROM incident_ticket_rechecks WHERE preview_id=? AND subject=?`, previewID, subject).Scan(&recorded)
	if err == nil {
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return IncidentRecheckResult{}, e
		}
		defer tx.Rollback()
		if e = incidentAssetPermission(tx, subject, ref, now()); e != nil {
			return IncidentRecheckResult{}, e
		}
		var state string
		var recovered int
		if e = tx.QueryRow(`SELECT state,recovery_verified FROM incidents WHERE incident_id=?`, incident).Scan(&state, &recovered); e != nil {
			return IncidentRecheckResult{}, e
		}
		if e = tx.Commit(); e != nil {
			return IncidentRecheckResult{}, e
		}
		acceptance := "pending"
		if state == "closed" || state == "resolved" || recovered != 0 {
			acceptance = "changed"
		}
		return IncidentRecheckResult{TicketID: ticketID, ObjectRef: ref, ConfirmedAt: recorded, Duplicate: true, IncidentAcceptance: acceptance}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return IncidentRecheckResult{}, err
	}
	if now() >= expires {
		return IncidentRecheckResult{}, fmt.Errorf("%w：确认已过期，请重新预览", ErrConflict)
	}
	// Check the preview snapshot and authorization immediately before probing.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IncidentRecheckResult{}, err
	}
	if err = incidentAssetPermission(tx, subject, ref, now()); err != nil {
		tx.Rollback()
		return IncidentRecheckResult{}, err
	}
	var currentUpdated int64
	var incidentState string
	var recovered int
	if err = tx.QueryRow(`SELECT updated_at,state,recovery_verified FROM incidents WHERE incident_id=?`, incident).Scan(&currentUpdated, &incidentState, &recovered); err != nil {
		tx.Rollback()
		return IncidentRecheckResult{}, err
	}
	asset, err := readAsset(tx, ref)
	if err != nil {
		tx.Rollback()
		return IncidentRecheckResult{}, err
	}
	ticket, err := readTicket(tx, ticketID)
	if err != nil {
		tx.Rollback()
		return IncidentRecheckResult{}, err
	}
	if currentUpdated != eventUpdated || incidentState == "closed" || incidentState == "resolved" || recovered != 0 || ticket.IncidentID != incident || ticket.ObjectRef != ref ||
		asset.Identity != identity || asset.Revision != revision || asset.OwnershipEpoch != epoch || (ticket.State != "closed" && ticket.State != "verified") {
		tx.Rollback()
		return IncidentRecheckResult{}, fmt.Errorf("%w：事件、工单或资产状态已变化，请重新预览", ErrConflict)
	}
	if err = tx.Commit(); err != nil {
		return IncidentRecheckResult{}, err
	}
	wasClosed := ticket.State == "closed"
	if wasClosed {
		err = s.VerifyTicketClosed(ctx, ticketID, subject, checker)
	} else {
		err = s.refreshVerifiedTicketHealth(ctx, ticket, subject, checker)
	}
	if err != nil {
		return IncidentRecheckResult{}, err
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return IncidentRecheckResult{}, err
	}
	defer tx.Rollback()
	if err = incidentAssetPermission(tx, subject, ref, now()); err != nil {
		return IncidentRecheckResult{}, err
	}
	if err = tx.QueryRow(`SELECT updated_at,state,recovery_verified FROM incidents WHERE incident_id=?`, incident).Scan(&currentUpdated, &incidentState, &recovered); err != nil {
		return IncidentRecheckResult{}, err
	}
	asset, err = readAsset(tx, ref)
	if err != nil {
		return IncidentRecheckResult{}, err
	}
	ticket, err = readTicket(tx, ticketID)
	if err != nil {
		return IncidentRecheckResult{}, err
	}
	var evidence, receiptIdentity string
	var receiptChecked, receiptRevision, receiptEpoch, receiptVersion int
	var verifiedAt int64
	err = tx.QueryRow(`SELECT evidence,asset_identity,checked_asset_revision,asset_revision,ownership_epoch,external_state_version,verified_at FROM ticket_health_verifications WHERE ticket_id=? AND object_ref=?`, ticketID, ref).
		Scan(&evidence, &receiptIdentity, &receiptChecked, &receiptRevision, &receiptEpoch, &receiptVersion, &verifiedAt)
	if err != nil {
		return IncidentRecheckResult{}, err
	}
	// Verification may itself move a maintenance asset to recheck, advancing
	// both revision and ownership epoch. Accept only the transition recorded by
	// this verification; any unrelated concurrent change remains a conflict.
	assetVersionMatches := asset.Revision == revision && asset.OwnershipEpoch == epoch
	if !assetVersionMatches && wasClosed && asset.State == AssetStateRecheck &&
		asset.Revision == revision+1 && asset.OwnershipEpoch == epoch+1 &&
		receiptRevision == asset.Revision && receiptEpoch == asset.OwnershipEpoch {
		var transitioned int
		err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM asset_transitions
WHERE object_ref=? AND from_state=? AND to_state=? AND trigger_kind='ticket_repair_verified'
AND expected_revision=? AND expected_ownership_epoch=? AND actor=? AND evidence_refs=? AND occurred_at=?)`,
			ref, AssetStateMaintenance, AssetStateRecheck, revision, epoch, subject, digest(evidence), verifiedAt).Scan(&transitioned)
		if err != nil {
			return IncidentRecheckResult{}, err
		}
		assetVersionMatches = transitioned != 0
	}
	if currentUpdated != eventUpdated || incidentState == "closed" || incidentState == "resolved" || recovered != 0 || ticket.IncidentID != incident || ticket.State != "verified" ||
		asset.Identity != identity || receiptIdentity != identity || !assetVersionMatches || asset.Revision != receiptRevision || asset.OwnershipEpoch != receiptEpoch ||
		receiptChecked != revision || ticket.ExternalStateVersion != receiptVersion || strings.TrimSpace(evidence) == "" {
		return IncidentRecheckResult{}, fmt.Errorf("%w：复检期间事件、工单或资产状态已变化", ErrConflict)
	}
	stamp := now()
	if _, err = tx.Exec(`INSERT OR IGNORE INTO incident_ticket_rechecks(preview_id,incident_id,ticket_id,subject,evidence_digest,confirmed_at) VALUES(?,?,?,?,?,?)`, previewID, incident, ticketID, subject, digest(evidence), stamp); err != nil {
		return IncidentRecheckResult{}, err
	}
	if err = tx.QueryRow(`SELECT confirmed_at FROM incident_ticket_rechecks WHERE preview_id=?`, previewID).Scan(&recorded); err != nil {
		return IncidentRecheckResult{}, err
	}
	if err = recordAudit(tx, AuditEvent{Event: "incident_asset_recheck_confirmed", Actor: subject, Summary: fmt.Sprintf("事件 %s 的资产 %s 已独立复检；事件仍待验收", incident, ref), OccurredAt: stamp}); err != nil {
		return IncidentRecheckResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return IncidentRecheckResult{}, err
	}
	return IncidentRecheckResult{TicketID: ticketID, ObjectRef: ref, ConfirmedAt: recorded, IncidentAcceptance: "pending"}, nil
}
