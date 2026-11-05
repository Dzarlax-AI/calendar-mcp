package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// experimentalBeginTargetGuard is a private storage-only experiment. It binds
// a target operation intent to a target calendar's durable mutation guard; it
// neither validates provider ownership nor authorizes provider dispatch. A
// successful source verified bit also does not substitute for the complete
// provider provenance/readiness contract. This non-exclusive variant also
// does not check connection status, quarantine, cursor or timezone. Cross-rule
// target ownership is not established, and the old exported PrepareTargetIntent
// still exists without this guard. These are runtime blockers: do not dispatch
// or install this helper until all paths enforce the full contract.
func (s *Store) experimentalBeginTargetGuard(ctx context.Context, in TargetIntentRequest, pin SnapshotPinRequest) error {
	return s.experimentalBeginTargetGuardWithReservation(ctx, in, pin, "")
}

// experimentalBeginExclusiveTargetGuard adds an exact-key reservation to the
// same transaction as the target intent and projection guard. It establishes
// no physical identity, alias equivalence, common-writer barrier or permission
// to dispatch. It remains unconnected to runtime.
func (s *Store) experimentalBeginExclusiveTargetGuard(ctx context.Context, in TargetIntentRequest, pin SnapshotPinRequest, targetKey string) error {
	if !validExclusiveTargetKey(targetKey) {
		return ErrExclusiveDispatchConflict
	}
	return s.experimentalBeginTargetGuardWithReservation(ctx, in, pin, targetKey)
}

func (s *Store) experimentalBeginTargetGuardWithReservation(ctx context.Context, in TargetIntentRequest, pin SnapshotPinRequest, targetKey string) error {
	if !validProjectionMutationID(in.OperationID) || in.RuleID == "" || in.JobID == "" || in.WorkerID == "" || in.SourceCalendarID == "" || in.TargetCalendarID == "" || in.SourceCalendarID == in.TargetCalendarID || in.JobAttempt < 1 || in.RuleVersion < 1 || in.SourceRevision < 1 || in.Now.IsZero() || in.WindowStart.IsZero() || !in.WindowEnd.After(in.WindowStart) || in.LeaseDuration <= 0 || in.LeaseDuration > maxTargetIntentLease || in.SnapshotMaxAge <= 0 || !targetDigest(in.IdentityHash) || !targetDigest(in.PayloadHash) || len(in.RuleID) > 512 || len(in.JobID) > 512 || len(in.WorkerID) > 512 || len(in.SourceCalendarID) > 1024 || len(in.TargetCalendarID) > 1024 || len(in.TargetEventID) > 1024 || len(in.ExpectedETag) > 1024 || in.OwnershipMarker == "" || len(in.OwnershipMarker) > 1024 {
		return ErrTargetIntentBlocked
	}
	switch in.Kind {
	case "create":
		if in.TargetEventID != "" || in.ExpectedETag != "" {
			return ErrTargetIntentBlocked
		}
	case "update", "delete":
		if in.TargetEventID == "" || in.ExpectedETag == "" {
			return ErrTargetIntentBlocked
		}
	default:
		return ErrTargetIntentBlocked
	}
	pin.ExpiresAt = pin.ExpiresAt.UTC().Truncate(time.Microsecond)
	if !validPinRequest(pin) || pin.RuleID != in.RuleID || pin.RuleVersion != in.RuleVersion || pin.JobID != in.JobID || pin.JobAttempt != in.JobAttempt || pin.WorkerID != in.WorkerID || pin.CalendarID != in.SourceCalendarID || pin.Revision != in.SourceRevision || pin.LeaseDuration != in.LeaseDuration {
		return ErrTargetIntentBlocked
	}
	// Reject an obviously mismatched route before creating even a tentative
	// ledger row. The rule is rechecked under lock below, since this read is
	// not authoritative when configuration changes concurrently.
	var expectedSource, expectedTarget string
	err := s.db.QueryRowContext(ctx, s.query(`SELECT source_calendar_id,target_calendar_id FROM sync_rules WHERE id=?`), in.RuleID).Scan(&expectedSource, &expectedTarget)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (expectedSource != in.SourceCalendarID || expectedTarget != in.TargetCalendarID) {
		return ErrTargetIntentBlocked
	}
	if err != nil {
		return err
	}
	_, encoded, err := normalizeMutationCalendars([]string{in.TargetCalendarID})
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Lock order is ledger -> sorted calendars -> source guard -> rule -> job ->
	// pin -> target guard. Conflicting multi-calendar writers lock calendars in
	// the same sorted order before acquiring any projection guard.
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_operations(operation_id,calendar_ids,started_at) VALUES(?,?,?)`), in.OperationID, encoded, in.Now.UTC()); err != nil {
		return fmt.Errorf("insert experimental mutation ledger: %w", err)
	}
	if targetKey != "" {
		reservation := ExclusiveDispatchRequest{OperationID: in.OperationID, TargetKey: targetKey, RequestHash: in.PayloadHash, Kind: in.Kind, Now: in.Now}
		if err = s.insertExclusiveReservation(ctx, tx, reservation); err != nil {
			return err
		}
		// Match source writers' order: sorted calendars, connection, sync state,
		// then projection guard. Readiness remains a transaction-time check;
		// it is not a replacement for the missing all-writers dispatch barrier.
		ids := []string{in.SourceCalendarID, in.TargetCalendarID}
		if ids[0] > ids[1] {
			ids[0], ids[1] = ids[1], ids[0]
		}
		if err = lockMutationCalendars(ctx, s, tx, ids); err != nil {
			return err
		}
		if err = lockProjectionPageSource(ctx, s, tx, in.SourceCalendarID); err != nil {
			return err
		}
		if _, err = selectCalendarSyncStateForUpdate(ctx, s, tx, in.SourceCalendarID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrTargetIntentBlocked
			}
			return err
		}
	}
	claimed, err := s.lockSnapshotPinFence(ctx, tx, pin)
	if err != nil {
		if errors.Is(err, ErrSnapshotPinBlocked) {
			return ErrTargetIntentBlocked
		}
		return err
	}
	var trustedDeadline sql.NullTime
	if targetKey != "" {
		if err = tx.QueryRowContext(ctx, s.query(`SELECT lease_until FROM sync_jobs WHERE id=? AND rule_id=?`), in.JobID, in.RuleID).Scan(&trustedDeadline); err != nil {
			return err
		}
		if !trustedDeadline.Valid || !trustedDeadline.Time.After(time.Now().UTC()) || pin.ExpiresAt.After(trustedDeadline.Time) {
			return ErrTargetIntentBlocked
		}
	}
	// lockSnapshotPinFence has locked the rule in the shared guard -> rule
	// order. Verify the requested destination against that locked version
	// before looking up or changing any target-specific guard/intent.
	var lockedSource, lockedTarget string
	if err = tx.QueryRowContext(ctx, s.query(`SELECT source_calendar_id,target_calendar_id FROM sync_rules WHERE id=?`), in.RuleID).Scan(&lockedSource, &lockedTarget); err != nil {
		return err
	}
	if lockedSource != in.SourceCalendarID || lockedTarget != in.TargetCalendarID {
		return ErrTargetIntentBlocked
	}
	var sourceReadable, targetWritable bool
	if err = tx.QueryRowContext(ctx, s.query(`SELECT can_read FROM calendars WHERE id=?`), in.SourceCalendarID).Scan(&sourceReadable); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, s.query(`SELECT can_write FROM calendars WHERE id=?`), in.TargetCalendarID).Scan(&targetWritable); err != nil {
		return err
	}
	if !sourceReadable || !targetWritable {
		return ErrTargetIntentBlocked
	}
	q := `SELECT status,expires_at FROM projection_snapshot_pins WHERE pin_id=? AND calendar_id=? AND revision=? AND rule_id=? AND rule_version=? AND job_id=? AND job_attempt=? AND worker_id=?`
	if s.dialect == DialectPostgres {
		q += ` FOR NO KEY UPDATE`
	}
	var status string
	var expiry time.Time
	err = tx.QueryRowContext(ctx, s.query(q), pin.PinID, pin.CalendarID, pin.Revision, pin.RuleID, pin.RuleVersion, pin.JobID, pin.JobAttempt, pin.WorkerID).Scan(&status, &expiry)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (status != "active" || !expiry.Equal(pin.ExpiresAt)) {
		return ErrTargetIntentBlocked
	}
	if err != nil {
		return err
	}
	var epoch, revision, snapshotEpoch, pending int64
	var verified bool
	var start, end, completed time.Time
	err = tx.QueryRowContext(ctx, s.query(`SELECT g.epoch,g.revision,p.guard_epoch,p.window_start,p.window_end,p.completed_at,p.verified,(SELECT COUNT(*) FROM projection_mutation_intents i WHERE i.calendar_id=g.calendar_id) FROM projection_mutation_guards g JOIN projection_snapshots p ON p.calendar_id=g.calendar_id AND p.revision=? WHERE g.calendar_id=?`), in.SourceRevision, in.SourceCalendarID).Scan(&epoch, &revision, &snapshotEpoch, &start, &end, &completed, &verified, &pending)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (!verified || revision != in.SourceRevision || epoch != snapshotEpoch || pending != 0 || start.After(in.WindowStart) || end.Before(in.WindowEnd) || completed.After(in.Now) || completed.Before(in.Now.Add(-in.SnapshotMaxAge))) {
		return ErrTargetIntentBlocked
	}
	if err != nil {
		return err
	}
	if targetKey != "" {
		if err = s.checkExperimentalSourceReadiness(ctx, tx, in); err != nil {
			return err
		}
	}
	var ruleState, jobKind string
	if err = tx.QueryRowContext(ctx, s.query(`SELECT r.state,j.kind FROM sync_rules r JOIN sync_jobs j ON j.rule_id=r.id WHERE r.id=? AND j.id=?`), in.RuleID, in.JobID).Scan(&ruleState, &jobKind); err != nil {
		return err
	}
	if ruleState != "enabled" || jobKind == "dry_run" {
		return ErrTargetIntentBlocked
	}
	var count int
	if err = tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM target_operation_intents WHERE rule_id=?`), in.RuleID).Scan(&count); err != nil {
		return err
	}
	if count >= maxTargetIntentsPerRule {
		return ErrTargetIntentBlocked
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, s.query(`SELECT EXISTS(SELECT 1 FROM target_operation_intents WHERE operation_id=? OR (rule_id=? AND identity_hash=? AND status IN ('pending','ambiguous')))`), in.OperationID, in.RuleID, in.IdentityHash).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrTargetIntentBlocked
	}
	_, err = tx.ExecContext(ctx, s.query(`INSERT INTO target_operation_intents(operation_id,rule_id,source_calendar_id,target_calendar_id,source_revision,window_start,window_end,identity_hash,target_event_id,expected_etag,ownership_marker,payload_hash,operation_kind,rule_version,job_id,job_attempt,worker_id,status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), in.OperationID, in.RuleID, in.SourceCalendarID, in.TargetCalendarID, in.SourceRevision, in.WindowStart.UTC(), in.WindowEnd.UTC(), in.IdentityHash, in.TargetEventID, in.ExpectedETag, in.OwnershipMarker, in.PayloadHash, in.Kind, in.RuleVersion, in.JobID, in.JobAttempt, in.WorkerID, "pending", in.Now.UTC())
	if err != nil {
		return fmt.Errorf("insert experimental target intent: %w", err)
	}
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_guards(calendar_id,epoch) VALUES(?,0) ON CONFLICT(calendar_id) DO NOTHING`), in.TargetCalendarID); err != nil {
		return err
	}
	if err = advanceProjectionMutationGuard(ctx, s, tx, in.TargetCalendarID); err != nil {
		return err
	}
	var guardPending int64
	if err = tx.QueryRowContext(ctx, s.query(`SELECT COUNT(*) FROM projection_mutation_intents WHERE calendar_id=?`), in.TargetCalendarID).Scan(&guardPending); err != nil {
		return err
	}
	if guardPending >= maxPendingProjectionMutations {
		return ErrProjectionMutationLimit
	}
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_intents(calendar_id,mutation_id,started_at) VALUES(?,?,?)`), in.TargetCalendarID, ":"+in.OperationID, in.Now.UTC()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_operation_members(operation_id,calendar_id,intent_id) VALUES(?,?,?)`), in.OperationID, in.TargetCalendarID, ":"+in.OperationID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO target_operation_guard_links(operation_id,calendar_id) VALUES(?,?)`), in.OperationID, in.TargetCalendarID); err != nil {
		return err
	}
	if targetKey != "" {
		if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO exclusive_dispatch_guard_members(operation_id,calendar_id) VALUES(?,?)`), in.OperationID, in.TargetCalendarID); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	if !expiry.After(now) || !claimed.Add(in.LeaseDuration).After(now) || targetKey != "" && (!trustedDeadline.Valid || !trustedDeadline.Time.After(now)) || completed.After(now) || completed.Before(now.Add(-in.SnapshotMaxAge)) {
		return ErrTargetIntentBlocked
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit experimental target operation: %w", err)
	}
	return nil
}

// checkExperimentalSourceReadiness requires source calendar, connection, sync
// state and projection guard locks. It checks retained provenance metadata,
// not provider completeness, alias equivalence or permission to dispatch.
func (s *Store) checkExperimentalSourceReadiness(ctx context.Context, tx *sql.Tx, in TargetIntentRequest) error {
	var zone, snapshotZone, cursor, snapshotCursor, connectionStatus, provider, status string
	var syncStart, syncEnd, snapshotStart, snapshotEnd, completed time.Time
	var lastSuccess sql.NullTime
	var enabled, readable, quarantined bool
	err := tx.QueryRowContext(ctx, s.query(`SELECT c.timezone,p.timezone,st.cursor,p.cursor,conn.status,conn.provider,st.status,st.window_start,st.window_end,p.window_start,p.window_end,p.completed_at,st.last_success_at,c.enabled,c.can_read,EXISTS(SELECT 1 FROM calendar_sync_quarantine q WHERE q.calendar_id=c.id AND q.active=?) FROM calendars c JOIN connections conn ON conn.id=c.connection_id JOIN calendar_sync_state st ON st.calendar_id=c.id JOIN projection_snapshots p ON p.calendar_id=c.id AND p.revision=? WHERE c.id=?`), true, in.SourceRevision, in.SourceCalendarID).Scan(&zone, &snapshotZone, &cursor, &snapshotCursor, &connectionStatus, &provider, &status, &syncStart, &syncEnd, &snapshotStart, &snapshotEnd, &completed, &lastSuccess, &enabled, &readable, &quarantined)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTargetIntentBlocked
	}
	if err != nil {
		return fmt.Errorf("inspect experimental source readiness: %w", err)
	}
	_, zoneErr := time.LoadLocation(zone)
	if !enabled || !readable || provider != "google" || connectionStatus != "connected" || status != "ready" || quarantined || zone == "" || zone == "Local" || zoneErr != nil || zone != snapshotZone || snapshotCursor == "" || cursor != snapshotCursor || !lastSuccess.Valid || lastSuccess.Time.Before(completed) || !syncStart.Equal(snapshotStart) || !syncEnd.Equal(snapshotEnd) {
		return ErrTargetIntentBlocked
	}
	return nil
}
