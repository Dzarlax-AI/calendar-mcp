package storage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"calendar-mcp/internal/calendar"
)

type gatewayPointWrite struct {
	calendarID, eventID string
	event               *calendar.EventV2
	deleted             bool
}

func (s *Store) publishGatewayWriteReceipt(ctx context.Context, tx *sql.Tx, operationID string, now time.Time) error {
	var phase, scopeJSON, grantsJSON, encoded string
	var confirmedMS int64
	err := tx.QueryRowContext(ctx, s.query(`SELECT phase,scope_json,grants_json,receipt_ciphertext,confirmed_ms
	FROM gateway_writes WHERE operation_id=?`), operationID).Scan(&phase, &scopeJSON, &grantsJSON, &encoded, &confirmedMS)
	if err != nil {
		return err
	}
	if phase == "not_dispatched" {
		return nil
	}
	if phase != "confirmed" || s.artifactCipher == nil || len(encoded) > 2*gatewayReceiptBytes {
		return ErrExclusiveDispatchConflict
	}
	if now.UnixMilli()-confirmedMS >= GatewayAuthorityLifetime.Milliseconds() {
		return nil
	}
	var scope calendar.GatewayWriteScope
	if err := json.Unmarshal([]byte(scopeJSON), &scope); err != nil {
		return err
	}
	var grants map[string]string
	if err := json.Unmarshal([]byte(grantsJSON), &grants); err != nil {
		return err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return err
	}
	value, err := s.artifactCipher.Decrypt(ciphertext, []byte("gateway-write-receipt:"+operationID))
	if err != nil {
		return err
	}
	points, err := gatewayReceiptPoints(scope, value)
	if err != nil {
		return err
	}
	for _, point := range points {
		current, err := s.gatewayCalendarGrants(ctx, tx, now, point.calendarID, false)
		if err != nil {
			return err
		}
		if len(current) != 1 || current[0].Class == GatewayMetadataOnly || !now.Before(current[0].HardVerifiedUntil) ||
			current[0].ConnectionRevision != scope.AuthorizationRevision || current[0].Revision != grants[point.calendarID] {
			continue
		}
		grant := current[0]
		eventID := gatewayEventID(grant.Metadata.Provider, point.eventID)
		var row gatewayEventRow
		if !point.deleted {
			var visible bool
			row, visible, err = gatewayEventForPublication(grant, *point.event)
			if err != nil || !visible {
				continue
			}
			eventID = row.ID
		}
		epoch, err := s.gatewayMutationEpoch(ctx, tx, point.calendarID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, s.query(`INSERT INTO gateway_write_points
		(calendar_id,event_id,operation_id,grant_revision,mutation_epoch,confirmed_ms,deleted,oversized,payload_json)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(calendar_id,event_id) DO UPDATE SET
		operation_id=excluded.operation_id,grant_revision=excluded.grant_revision,mutation_epoch=excluded.mutation_epoch,
		confirmed_ms=excluded.confirmed_ms,deleted=excluded.deleted,oversized=excluded.oversized,payload_json=excluded.payload_json,settled=FALSE
		WHERE gateway_write_points.mutation_epoch<=excluded.mutation_epoch`),
			point.calendarID, eventID, operationID, grant.Revision, epoch, confirmedMS, point.deleted, row.Oversized, row.Payload)
		if err != nil {
			return err
		}
	}
	return nil
}

func gatewayReceiptPoints(scope calendar.GatewayWriteScope, value []byte) ([]gatewayPointWrite, error) {
	if scope.Kind == "create" || scope.Kind == "import" {
		var event calendar.EventV2
		if err := json.Unmarshal(value, &event); err != nil {
			return nil, err
		}
		return []gatewayPointWrite{{calendarID: scope.CalendarID, eventID: event.ID, event: &event}}, nil
	}
	var result calendar.OperationResult
	if err := json.Unmarshal(value, &result); err != nil {
		return nil, err
	}
	if result.Status != "completed" {
		return nil, ErrExclusiveDispatchConflict
	}
	if scope.Scope == calendar.ScopeFollowing {
		return nil, nil
	}
	switch scope.Kind {
	case "delete":
		return []gatewayPointWrite{{calendarID: scope.CalendarID, eventID: scope.EventID, deleted: true}}, nil
	case "move":
		points := []gatewayPointWrite{{calendarID: scope.CalendarID, eventID: scope.EventID, deleted: true}}
		if result.Event != nil && result.Event.ID != "" {
			points = append(points, gatewayPointWrite{calendarID: scope.DestinationCalendarID, eventID: result.Event.ID, event: result.Event})
		}
		return points, nil
	case "update", "respond":
		if result.Event != nil && result.Event.ID != "" {
			return []gatewayPointWrite{{calendarID: scope.CalendarID, eventID: result.Event.ID, event: result.Event}}, nil
		}
	}
	return nil, nil
}

func (s *Store) gatewayReadWritePoint(ctx context.Context, tx *sql.Tx, grant GatewayCalendarGrant, eventID string, now time.Time) (GatewayReadItem, string, error) {
	var item GatewayReadItem
	var receipt, payload string
	id := gatewayEventID(grant.Metadata.Provider, eventID)
	err := tx.QueryRowContext(ctx, s.query(`SELECT p.operation_id,p.deleted,p.oversized,p.payload_json FROM gateway_write_points p
	WHERE p.calendar_id=? AND p.event_id=? AND p.grant_revision=? AND p.confirmed_ms>?
	AND (NOT p.settled OR p.deleted) AND p.mutation_epoch=COALESCE((SELECT g.epoch FROM projection_mutation_guards g WHERE g.calendar_id=p.calendar_id),0)
	AND NOT EXISTS(SELECT 1 FROM projection_mutation_intents i WHERE i.calendar_id=p.calendar_id)`),
		grant.Metadata.ID, id, grant.Revision, now.Add(-GatewayAuthorityLifetime).UnixMilli()).Scan(&receipt, &item.Cancelled, &item.Oversized, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return item, "", nil
	}
	if err != nil {
		return item, "", err
	}
	if item.Cancelled {
		item.Event = calendar.EventV2{ID: id, CalendarID: grant.Metadata.ID, Provider: grant.Metadata.Provider}
	} else if err := json.Unmarshal([]byte(payload), &item.Event); err != nil {
		return item, "", err
	}
	return item, receipt, nil
}

// Keep known writes visible to point reads until a post-write provider
// snapshot confirms the same event. A stale or missing snapshot cannot
// resurrect the previous event version.
func (s *Store) reconcileGatewayWritePoints(ctx context.Context, tx *sql.Tx, id, revision string, epoch int64, observed time.Time, rows []gatewayEventRow) error {
	current := map[string]gatewayEventRow{}
	for _, row := range rows {
		current[row.ID] = row
	}
	pending, err := tx.QueryContext(ctx, s.query(`SELECT event_id,deleted,payload_json FROM gateway_write_points
	WHERE calendar_id=? AND grant_revision=? AND mutation_epoch=? AND confirmed_ms<=? AND NOT settled`), id, revision, epoch, observed.UnixMilli())
	if err != nil {
		return err
	}
	defer pending.Close()
	var reconciled []string
	for pending.Next() {
		var eventID, payload string
		var deleted bool
		if err := pending.Scan(&eventID, &deleted, &payload); err != nil {
			return err
		}
		item, exists := current[eventID]
		if deleted {
			// A positive cancellation is retained for point reads. Once
			// the provider view agrees, it no longer prevents full pages.
			if !exists || item.Cancelled {
				reconciled = append(reconciled, eventID)
			}
		} else if exists && !item.Cancelled && sameGatewayReceiptPayload(payload, item.Payload) {
			reconciled = append(reconciled, eventID)
		}
	}
	if err := pending.Err(); err != nil {
		return err
	}
	if err := pending.Close(); err != nil {
		return err
	}
	for _, eventID := range reconciled {
		if _, err := tx.ExecContext(ctx, s.query(`UPDATE gateway_write_points SET settled=TRUE WHERE calendar_id=? AND event_id=?`), id, eventID); err != nil {
			return err
		}
	}
	return nil
}

func sameGatewayReceiptPayload(before, after string) bool {
	if before == after {
		return true
	}
	var a, b calendar.EventV2
	return json.Unmarshal([]byte(before), &a) == nil && json.Unmarshal([]byte(after), &b) == nil && a.ETag != "" && a.ETag == b.ETag
}
