package storage

import (
	"testing"
	"time"
)

// Test-only synthetic fixture. No production API can promote a revision.
func seedVerifiedProjectionSnapshot(t *testing.T, s *Store, id string, revision int64) {
	t.Helper()
	seedVerifiedProjectionSnapshotAt(t, s, id, revision, nil)
}

func seedVerifiedProjectionSnapshotAt(t *testing.T, s *Store, id string, revision int64, completed *time.Time) {
	t.Helper()
	snapshot, err := s.GetProjectionSnapshot(t.Context(), id, revision)
	if err != nil {
		t.Fatal(err)
	}
	if completed != nil {
		snapshot.CompletedAt = *completed
	}
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(t.Context(), s.query(`DELETE FROM projection_snapshots WHERE calendar_id=? AND revision=?`), id, revision); err != nil {
		t.Fatal(err)
	}
	_, err = tx.ExecContext(t.Context(), s.query(`INSERT INTO projection_snapshots(calendar_id,revision,stage_id,guard_epoch,base_revision,window_start,window_end,completed_at,timezone,cursor,payload,verified) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`), id, revision, snapshot.StageID, snapshot.GuardEpoch, snapshot.BaseRevision, snapshot.WindowStart, snapshot.WindowEnd, snapshot.CompletedAt, snapshot.Timezone, snapshot.Cursor, snapshot.Payload, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
