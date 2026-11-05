package storage

import (
	"context"
	"database/sql"
	"encoding/json"

	"calendar-mcp/internal/calendarprojection"
)

// releaseRetainedSourceCapacityTx removes only released, unreferenced pins and
// older snapshot revisions whose complete observation multiset is already in
// the current candidate. It runs under the source and guard locks, so new
// pins, checkpoints and target intents cannot race the decision.
func (s *Store) releaseRetainedSourceCapacityTx(ctx context.Context, tx *sql.Tx, calendarID string, current int64, observations []calendarprojection.RemovalObservation) error {
	_, err := tx.ExecContext(ctx, s.query(`DELETE FROM projection_snapshot_pins WHERE calendar_id=? AND status=? AND NOT EXISTS(SELECT 1 FROM projection_rule_checkpoints c WHERE c.pin_id=projection_snapshot_pins.pin_id) AND NOT EXISTS(SELECT 1 FROM target_operation_intents i WHERE i.rule_id=projection_snapshot_pins.rule_id AND i.job_id=projection_snapshot_pins.job_id AND i.job_attempt=projection_snapshot_pins.job_attempt AND i.worker_id=projection_snapshot_pins.worker_id)`), calendarID, "released")
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, s.query(`SELECT s.revision,p.observations FROM projection_snapshots s JOIN projection_revision_provenance p ON p.calendar_id=s.calendar_id AND p.revision=s.revision WHERE s.calendar_id=? AND s.revision<? AND s.verified=? ORDER BY s.revision`), calendarID, current, true)
	if err != nil {
		return err
	}
	type old struct {
		revision int64
		raw      []byte
	}
	var candidates []old
	for rows.Next() {
		var item old
		if err = rows.Scan(&item.revision, &item.raw); err != nil {
			_ = rows.Close()
			return err
		}
		candidates = append(candidates, item)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, candidate := range candidates {
		var prior []calendarprojection.RemovalObservation
		if len(candidate.raw) == 0 || len(candidate.raw) > maxProjectionObservationBytes || json.Unmarshal(candidate.raw, &prior) != nil || !observationMultisetContains(observations, prior) {
			continue
		}
		if err = s.retireSourceSnapshotTx(ctx, tx, calendarID, candidate.revision); err != nil {
			return err
		}
	}
	return nil
}
func observationMultisetContains(all, prior []calendarprojection.RemovalObservation) bool {
	counts := make(map[string]int, len(all))
	for _, v := range all {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		counts[string(b)]++
	}
	for _, v := range prior {
		b, err := json.Marshal(v)
		if err != nil || counts[string(b)] == 0 {
			return false
		}
		counts[string(b)]--
	}
	return true
}
func (s *Store) retireSourceSnapshotTx(ctx context.Context, tx *sql.Tx, calendarID string, revision int64) error {
	_, err := tx.ExecContext(ctx, s.query(`DELETE FROM projection_snapshots WHERE calendar_id=? AND revision=?
 AND NOT EXISTS (SELECT 1 FROM projection_snapshot_pins p WHERE p.calendar_id=projection_snapshots.calendar_id AND p.revision=projection_snapshots.revision)
 AND NOT EXISTS (SELECT 1 FROM projection_rule_checkpoints p WHERE p.calendar_id=projection_snapshots.calendar_id AND p.revision=projection_snapshots.revision)
 AND NOT EXISTS (SELECT 1 FROM cache_event_mappings m WHERE m.source_calendar_id=projection_snapshots.calendar_id AND m.source_revision=projection_snapshots.revision)
 AND NOT EXISTS (SELECT 1 FROM target_operation_intents i WHERE i.source_calendar_id=projection_snapshots.calendar_id AND i.source_revision=projection_snapshots.revision)
 AND NOT EXISTS (SELECT 1 FROM projection_snapshot_staging t WHERE t.calendar_id=projection_snapshots.calendar_id AND t.base_revision=projection_snapshots.revision)
 AND NOT EXISTS (SELECT 1 FROM projection_page_attempts a WHERE a.calendar_id=projection_snapshots.calendar_id AND a.base_revision=projection_snapshots.revision)`), calendarID, revision)
	return err
}
