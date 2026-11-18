package storage

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/google/uuid"
)

// ErrMappingConflict denotes a missing, stale, or protected mapping. No caller
// may dispatch a provider operation after this error.
var ErrMappingConflict = errors.New("event mapping unavailable or changed")

// InsertPendingMutationMapping parks an update/delete for an unmapped target.
// A successful insert is not proof of target ownership or a live job lease.
func (s *Store) InsertPendingMutationMapping(ctx context.Context, m Mapping) (string, error) {
	if m.ID == "" || m.RuleID == "" || m.SourceEventID == "" || m.TargetEventID == "" || strings.HasPrefix(m.TargetEventID, "pending:") || (m.ReconciliationState != "pending_update" && m.ReconciliationState != "pending_delete") || m.LastSeenAt.IsZero() || m.Version != 0 {
		return "", ErrMappingConflict
	}
	token, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	incarnation := "new:" + token.String()
	res, err := s.db.ExecContext(ctx, s.query(`INSERT INTO event_mappings(id,rule_id,object_kind,source_event_id,source_series_id,original_start,target_event_id,target_series_id,content_hash,last_seen_at,reconciliation_state,incarnation,target_etag) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`), m.ID, m.RuleID, m.ObjectKind, m.SourceEventID, m.SourceSeriesID, m.OriginalStart, m.TargetEventID, m.TargetSeriesID, m.ContentHash, m.LastSeenAt, m.ReconciliationState, incarnation, m.TargetETag)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return "", err
	}
	return incarnation, nil
}

// UpdateMappingCAS writes an exact version of an existing immutable identity.
// It is a storage fence, not proof of provider outcome or a live job lease.
func (s *Store) UpdateMappingCAS(ctx context.Context, m Mapping, expectedVersion int64) error {
	return s.updateMappingCAS(ctx, m, expectedVersion, false)
}
func (s *Store) updateMappingCAS(ctx context.Context, m Mapping, expectedVersion int64, legacy bool) error {
	if m.ID == "" || m.RuleID == "" || m.SourceEventID == "" || m.Incarnation == "" || expectedVersion < 1 || expectedVersion == math.MaxInt64 || m.Version != expectedVersion {
		return ErrMappingConflict
	}
	q := `UPDATE event_mappings SET object_kind=?,source_series_id=?,target_event_id=?,target_series_id=?,content_hash=?,last_seen_at=?,reconciliation_state=?,target_etag=?,version=version+1 WHERE id=? AND rule_id=? AND source_event_id=? AND original_start=? AND incarnation=? AND version=?`
	if legacy {
		q += ` AND substr(reconciliation_state,1,8)<>'pending_'`
	}
	res, err := s.db.ExecContext(ctx, s.query(q), m.ObjectKind, m.SourceSeriesID, m.TargetEventID, m.TargetSeriesID, m.ContentHash, m.LastSeenAt, m.ReconciliationState, m.TargetETag, m.ID, m.RuleID, m.SourceEventID, m.OriginalStart, m.Incarnation, expectedVersion)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrMappingConflict
	}
	return nil
}

// DeleteMappingCAS deletes only the exact observed mapping version. The caller
// must reconcile any ambiguous provider result before calling this method.
func (s *Store) DeleteMappingCAS(ctx context.Context, id, incarnation string, expectedVersion int64) error {
	if id == "" || incarnation == "" || expectedVersion < 1 {
		return ErrMappingConflict
	}
	res, err := s.db.ExecContext(ctx, s.query(`DELETE FROM event_mappings WHERE id=? AND incarnation=? AND version=?`), id, incarnation, expectedVersion)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrMappingConflict
	}
	return nil
}

// RestoreMappingAfterNoDispatchCAS restores the exact previous mapping only
// while the same job attempt still holds an unexpired dispatch lease.
func (s *Store) RestoreMappingAfterNoDispatchCAS(ctx context.Context, m Mapping, expectedVersion int64, job Job) error {
	if m.ID == "" || m.RuleID == "" || m.RuleID != job.RuleID || m.SourceEventID == "" || m.Incarnation == "" || m.Version != expectedVersion || expectedVersion < 1 || expectedVersion == math.MaxInt64 || job.ID == "" || job.ClaimedBy == "" || job.Attempt < 1 || (m.ReconciliationState != "current" && m.ReconciliationState != "legacy" && m.ReconciliationState != "") {
		return ErrMappingConflict
	}
	leaseClock := `julianday(substr(j.claimed_at,1,19) || substr(substr(j.claimed_at,20),1,instr(substr(j.claimed_at,20)||' ',' ')-1))<=julianday('now') AND julianday(substr(j.lease_until,1,19) || substr(substr(j.lease_until,20),1,instr(substr(j.lease_until,20)||' ',' ')-1))>julianday('now','+1 second')`
	if s.dialect == DialectPostgres {
		leaseClock = `j.claimed_at<=clock_timestamp() AND j.lease_until>clock_timestamp()`
	}
	q := `UPDATE event_mappings SET object_kind=?,source_series_id=?,target_event_id=?,target_series_id=?,content_hash=?,last_seen_at=?,reconciliation_state=?,target_etag=?,version=version+1 WHERE id=? AND rule_id=? AND source_event_id=? AND original_start=? AND incarnation=? AND version=? AND reconciliation_state='pending_update' AND EXISTS (SELECT 1 FROM sync_jobs j JOIN sync_rules r ON r.id=j.rule_id WHERE j.id=? AND j.rule_id=? AND j.state='running' AND j.claimed_by=? AND j.attempt=? AND j.stop_requested=? AND ` + leaseClock + ` AND (j.kind='dry_run' OR r.state='enabled'))`
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.dialect == DialectPostgres {
		var locked string
		if err := tx.QueryRowContext(ctx, s.query(`SELECT id FROM sync_jobs WHERE id=? FOR UPDATE`), job.ID).Scan(&locked); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, s.query(q), m.ObjectKind, m.SourceSeriesID, m.TargetEventID, m.TargetSeriesID, m.ContentHash, m.LastSeenAt, m.ReconciliationState, m.TargetETag, m.ID, m.RuleID, m.SourceEventID, m.OriginalStart, m.Incarnation, expectedVersion, job.ID, job.RuleID, job.ClaimedBy, job.Attempt, false)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrMappingConflict
	}
	return tx.Commit()
}
