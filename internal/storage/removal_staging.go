package storage

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"calendar-mcp/internal/calendar"
)

// RemovalStagePage is an unpublished observation for a single leased attempt.
// It is not proof of a complete provider snapshot or authority to delete copies.
type RemovalStagePage struct {
	Ordinal                                    int
	PageHash, PreviousTokenHash, NextTokenHash string
	Removals                                   []calendar.EventSyncRemoval
}

var ErrInvalidRemovalStage = errors.New("invalid removal staging page")

func validStageDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

// stageRemovals runs within ApplyEventSyncPage, after the calendar lease lock.
// There is deliberately no reader: a later publication protocol must bind these
// observations to a complete immutable snapshot and provider cursor first.
func (s *Store) stageRemovals(ctx context.Context, tx *sql.Tx, state CalendarSyncState, page RemovalStagePage, final bool) error {
	if page.Ordinal < 0 || page.Ordinal >= 250 || !validStageDigest(page.PageHash) || !validStageDigest(page.PreviousTokenHash) || !validStageDigest(page.NextTokenHash) || len(page.Removals) > 2500 {
		return ErrInvalidRemovalStage
	}
	emptyDigest := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if (page.Ordinal == 0 && page.PreviousTokenHash != emptyDigest) || (final && page.NextTokenHash != emptyDigest) || (!final && page.NextTokenHash == emptyDigest) {
		return ErrInvalidRemovalStage
	}
	for _, rem := range page.Removals {
		if rem.ObjectID == "" || (rem.Reason != calendar.EventSyncProviderCancellation && rem.Reason != calendar.EventSyncWindowEviction) {
			return ErrInvalidRemovalStage
		}
	}
	payload, err := json.Marshal(page.Removals)
	if err != nil || len(payload) > 1<<20 {
		return ErrInvalidRemovalStage
	}
	var hash, previous, next string
	var existingFinal bool
	var existingPayload []byte
	err = tx.QueryRowContext(ctx, s.query(`SELECT page_hash,previous_token_hash,next_token_hash,is_final,payload FROM event_sync_removal_staging WHERE calendar_id=? AND generation=? AND page_ordinal=?`), state.CalendarID, state.Generation, page.Ordinal).Scan(&hash, &previous, &next, &existingFinal, &existingPayload)
	if err == nil {
		if hash != page.PageHash || previous != page.PreviousTokenHash || next != page.NextTokenHash || existingFinal != final || string(existingPayload) != string(payload) {
			return ErrInvalidRemovalStage
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read removal staging page: %w", err)
	}
	if page.Ordinal == 0 {
		// Staging has no published readers; abandoned prior attempts can be discarded.
		if _, err = tx.ExecContext(ctx, s.query(`DELETE FROM event_sync_removal_staging WHERE calendar_id=? AND generation<>?`), state.CalendarID, state.Generation); err != nil {
			return fmt.Errorf("discard abandoned removal staging: %w", err)
		}
	} else {
		var priorNext string
		var priorFinal bool
		err = tx.QueryRowContext(ctx, s.query(`SELECT next_token_hash,is_final FROM event_sync_removal_staging WHERE calendar_id=? AND generation=? AND page_ordinal=?`), state.CalendarID, state.Generation, page.Ordinal-1).Scan(&priorNext, &priorFinal)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (priorFinal || priorNext != page.PreviousTokenHash)) {
			return ErrInvalidRemovalStage
		}
		if err != nil {
			return fmt.Errorf("read previous removal staging page: %w", err)
		}
	}
	var total int64
	if err = tx.QueryRowContext(ctx, s.query(`SELECT COALESCE(SUM(LENGTH(payload)),0) FROM event_sync_removal_staging WHERE calendar_id=? AND generation=?`), state.CalendarID, state.Generation).Scan(&total); err != nil {
		return fmt.Errorf("measure removal staging: %w", err)
	}
	if total+int64(len(payload)) > 16<<20 {
		return ErrInvalidRemovalStage
	}
	if _, err = tx.ExecContext(ctx, s.query(`INSERT INTO event_sync_removal_staging(calendar_id,generation,page_ordinal,page_hash,previous_token_hash,next_token_hash,is_final,payload) VALUES(?,?,?,?,?,?,?,?)`), state.CalendarID, state.Generation, page.Ordinal, page.PageHash, page.PreviousTokenHash, page.NextTokenHash, final, payload); err != nil {
		return fmt.Errorf("store removal staging: %w", err)
	}
	return nil
}
