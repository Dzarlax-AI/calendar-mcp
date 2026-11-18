package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
	"github.com/google/uuid"
)

const gatewayDenialLeasePrefix = "denied:"

const maxGatewayEvents = 16000
const maxGatewayPayload = 32 << 10
const maxGatewayReadBytes = 16 << 20

var ErrGatewayQueryTooLarge = errors.New("cached query exceeds its local scan limit")

type GatewayPublicationTicket struct {
	Grant         GatewayCalendarGrant
	State         CalendarSyncState
	MutationEpoch int64
}

type gatewayEventRow struct {
	ID, Series, Kind, Start, End, Payload string
	Master, Cancelled, Oversized          bool
}

// CaptureGatewayPublication fences provider work by the current access grant
// and the generation of the normal event-cache sync.
func (s *Store) CaptureGatewayPublication(ctx context.Context, calendarID string, now time.Time) (GatewayPublicationTicket, error) {
	tx, err := s.db.BeginTx(ctx, cachedPageReadTxOptions(s.dialect))
	if err != nil {
		return GatewayPublicationTicket{}, err
	}
	defer tx.Rollback()
	grants, err := s.gatewayCalendarGrants(ctx, tx, now, calendarID)
	if err != nil {
		return GatewayPublicationTicket{}, err
	}
	if len(grants) != 1 || grants[0].Class == GatewayMetadataOnly || !now.Before(grants[0].HardVerifiedUntil) {
		return GatewayPublicationTicket{}, ErrAuthorityChanged
	}
	state, err := s.gatewayReadySync(ctx, tx, calendarID)
	if err != nil {
		return GatewayPublicationTicket{}, err
	}
	epoch, err := s.gatewayMutationEpoch(ctx, tx, calendarID)
	if err != nil {
		return GatewayPublicationTicket{}, err
	}
	return GatewayPublicationTicket{Grant: grants[0], State: state, MutationEpoch: epoch}, tx.Commit()
}

func (s *Store) gatewayMutationEpoch(ctx context.Context, tx *sql.Tx, id string) (int64, error) {
	var epoch int64
	err := tx.QueryRowContext(ctx, s.query(`SELECT COALESCE((SELECT epoch FROM projection_mutation_guards WHERE calendar_id=?),0)`), id).Scan(&epoch)
	return epoch, err
}

func (s *Store) gatewayReadySync(ctx context.Context, tx *sql.Tx, id string) (CalendarSyncState, error) {
	query := s.timeProjection(`SELECT ss.generation,ss.window_start,ss.window_end,ss.last_success_at
	FROM calendar_sync_state ss
	WHERE ss.calendar_id=? AND ss.status='ready' AND ss.last_success_at IS NOT NULL
	AND NOT EXISTS (SELECT 1 FROM projection_mutation_intents i WHERE i.calendar_id=ss.calendar_id)`,
		"ss.window_start", "ss.window_end", "ss.last_success_at")
	out := CalendarSyncState{CalendarID: id}
	start, end, success := s.scanTime(), s.scanTime(), s.scanTime()
	err := tx.QueryRowContext(ctx, s.query(query), id).Scan(&out.Generation, &start, &end, &success)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrAuthorityChanged
	}
	if err != nil {
		return out, err
	}
	out.WindowStart, out.WindowEnd, out.LastSuccessAt = start.Time, end.Time, &success.Time
	return out, nil
}

// InvalidateGatewayGooglePublication closes one strict grant for proof observed
// with the exact current credential and source binding. Ordinary cache rows
// and sync state are outside this path.
func (s *Store) InvalidateGatewayGooglePublication(ctx context.Context, connectionID string, grant GatewayCalendarGrant, revision string, digest [32]byte) error {
	if revision == "" || revision != grant.ConnectionRevision {
		return ErrAuthorityChanged
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	lease := GatewayAuthorityLease{ConnectionID: connectionID, Provider: "google", AuthorizationRevision: revision}
	if err := s.lockGatewayConnection(ctx, tx, &lease); err != nil {
		return err
	}
	var ciphertext []byte
	err = tx.QueryRowContext(ctx, s.query(`SELECT encrypted_credentials FROM connections WHERE id=? AND credential_is_bounded`), connectionID).Scan(&ciphertext)
	if err != nil {
		return err
	}
	if sha256.Sum256(ciphertext) != digest {
		return ErrAuthorityChanged
	}
	// A deadline extension alone does not change the grant. Record the
	// lease active at denial so its response can renew only other grants.
	result, err := tx.ExecContext(ctx, s.query(`UPDATE gateway_calendar_grants SET verified_until_ms=0,
	grant_revision=COALESCE((SELECT ? || a.lease_id || ':' FROM gateway_account_checks a
	WHERE a.connection_id=gateway_calendar_grants.connection_id AND a.authorization_revision=? AND a.lease_id<>''),'') || ?
	WHERE calendar_id=? AND connection_id=? AND grant_revision=? AND detail_receipt=?
	AND EXISTS (SELECT 1 FROM calendars c WHERE c.id=gateway_calendar_grants.calendar_id AND c.detail_receipt=gateway_calendar_grants.detail_receipt)`),
		gatewayDenialLeasePrefix, revision, uuid.NewString(), grant.Metadata.ID, connectionID, grant.Revision, grant.DetailReceipt)
	if err := oneGatewayAuthorityRow(result, err); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PublishGatewayEvents(ctx context.Context, ticket GatewayPublicationTicket, events []calendar.EventV2, deletionsComplete bool, observedAt, now time.Time, verifiedSeries map[string]string) error {
	if len(events) > maxGatewayEvents || observedAt.IsZero() || observedAt.After(now) || now.Sub(observedAt) >= GatewayAuthorityLifetime {
		return ErrGatewayQueryTooLarge
	}
	publicationStarted := time.Now()
	id := ticket.Grant.Metadata.ID
	grant := ticket.Grant
	rows := make([]gatewayEventRow, 0, len(events))
	seen := make(map[string]bool, len(events))
	var bytes int
	for _, event := range events {
		row, visible, err := gatewayEventForPublication(grant, event)
		if err != nil {
			return err
		}
		if !visible {
			continue
		}
		if seen[row.ID] {
			return errors.New("duplicate normalized event in gateway publication")
		}
		seen[row.ID] = true
		bytes += len(row.Payload)
		if bytes > 4*maxGatewayReadBytes {
			return ErrGatewayQueryTooLarge
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b gatewayEventRow) int { return strings.Compare(a.ID, b.ID) })
	hash := sha256.New()
	writer := json.NewEncoder(hash)
	for _, row := range rows {
		if err := writer.Encode(row); err != nil {
			return err
		}
	}
	series := make(map[string]string)
	visibleMasters := map[string]bool{}
	for _, row := range rows {
		if row.Master && !row.Cancelled {
			visibleMasters[row.ID] = true
		}
	}
	for raw, zone := range verifiedSeries {
		id := gatewayEventID(grant.Metadata.Provider, raw)
		if visibleMasters[id] {
			series[id] = zone
		}
	}
	if err := writer.Encode(series); err != nil {
		return err
	}
	seriesJSON, err := json.Marshal(series)
	if err != nil {
		return err
	}

	content := fmt.Sprintf("%x", hash.Sum(nil))
	start, end := gatewayInstant(ticket.State.WindowStart), gatewayInstant(ticket.State.WindowEnd)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockCalendarConnection(ctx, s, tx, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.query(`INSERT INTO projection_mutation_guards(calendar_id,epoch)
	VALUES(?,0) ON CONFLICT(calendar_id) DO NOTHING`), id); err != nil {
		return err
	}
	// Writers hold this same guard during their intent transaction. Do not
	// acquire a calendar row lock here because release takes that before the
	// connection lock this publisher already holds.
	if _, err := tx.ExecContext(ctx, s.query(`UPDATE projection_mutation_guards SET epoch=epoch WHERE calendar_id=?`), id); err != nil {
		return err
	}
	epoch, err := s.gatewayMutationEpoch(ctx, tx, id)
	if err != nil {
		return err
	}
	if epoch != ticket.MutationEpoch {
		return ErrAuthorityChanged
	}
	current, err := s.gatewayCalendarGrants(ctx, tx, now, id)
	if err != nil {
		return err
	}
	if len(current) != 1 || !now.Before(current[0].HardVerifiedUntil) || current[0].Revision != grant.Revision || current[0].ConnectionRevision != grant.ConnectionRevision || current[0].DetailReceipt != grant.DetailReceipt {
		return ErrAuthorityChanged
	}
	st, err := s.gatewayReadySync(ctx, tx, id)
	if err != nil {
		return err
	}
	if ticket.State.LastSuccessAt == nil || st.Generation != ticket.State.Generation || !st.LastSuccessAt.Equal(*ticket.State.LastSuccessAt) ||
		!st.WindowStart.Equal(ticket.State.WindowStart) || !st.WindowEnd.Equal(ticket.State.WindowEnd) {
		return ErrAuthorityChanged
	}
	var priorHash, priorGrant, priorStart, priorEnd string
	var priorDelete bool
	var priorObserved, priorEpoch int64
	err = tx.QueryRowContext(ctx, s.query(`SELECT content_hash,grant_revision,window_start,window_end,deletions_complete,observed_ms,mutation_epoch
	FROM gateway_event_publications WHERE calendar_id=?`), id).Scan(&priorHash, &priorGrant, &priorStart, &priorEnd, &priorDelete, &priorObserved, &priorEpoch)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if observedAt.UnixMilli() < priorObserved {
		return ErrAuthorityChanged
	}
	unchanged := priorGrant == grant.Revision && priorHash == content && priorStart == start && priorEnd == end &&
		priorDelete == deletionsComplete && priorEpoch == epoch && priorObserved > now.Add(-GatewayAuthorityLifetime).UnixMilli()
	if unchanged {
		_, err = tx.ExecContext(ctx, s.query(`UPDATE gateway_event_publications SET sync_generation=?,observed_ms=? WHERE calendar_id=?`),
			st.Generation, observedAt.UnixMilli(), id)
	} else {
		_, err = tx.ExecContext(ctx, s.query(`INSERT INTO gateway_event_publications
		(calendar_id,grant_revision,sync_generation,mutation_epoch,content_revision,content_hash,observed_ms,window_start,window_end,deletions_complete,verified_cancellation_series)
		VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(calendar_id) DO UPDATE SET
		grant_revision=excluded.grant_revision,sync_generation=excluded.sync_generation,mutation_epoch=excluded.mutation_epoch,content_revision=excluded.content_revision,
		content_hash=excluded.content_hash,observed_ms=excluded.observed_ms,window_start=excluded.window_start,
		window_end=excluded.window_end,deletions_complete=excluded.deletions_complete,verified_cancellation_series=excluded.verified_cancellation_series`),
			id, grant.Revision, st.Generation, epoch, uuid.NewString(), content, observedAt.UnixMilli(), start, end, deletionsComplete, string(seriesJSON))
	}
	if err != nil {
		return err
	}
	if !unchanged {
		if _, err := tx.ExecContext(ctx, s.query(`DELETE FROM gateway_event_rows WHERE calendar_id=?`), id); err != nil {
			return err
		}
		insert, err := tx.PrepareContext(ctx, s.query(`INSERT INTO gateway_event_rows
		(calendar_id,event_id,series_id,is_master,is_cancelled,oversized,time_kind,range_start,range_end,payload_json) VALUES (?,?,?,?,?,?,?,?,?,?)`))
		if err != nil {
			return err
		}
		defer insert.Close()
		for _, row := range rows {
			if _, err := insert.ExecContext(ctx, id, row.ID, row.Series, row.Master, row.Cancelled, row.Oversized, row.Kind, row.Start, row.End, row.Payload); err != nil {
				return err
			}
		}
	}
	if err := s.reconcileGatewayWritePoints(ctx, tx, id, grant.Revision, epoch, observedAt, rows); err != nil {
		return err
	}

	now = now.Add(time.Since(publicationStarted))
	if !now.Before(current[0].HardVerifiedUntil) || now.Sub(observedAt) >= GatewayAuthorityLifetime {
		return ErrAuthorityChanged
	}
	return tx.Commit()
}

const gatewayInstantLayout = "2006-01-02T15:04:05.000000000Z"

func gatewayInstant(t time.Time) string { return t.UTC().Format(gatewayInstantLayout) }

func gatewayEventForPublication(grant GatewayCalendarGrant, event calendar.EventV2) (gatewayEventRow, bool, error) {
	provider := grant.Metadata.Provider
	visibility := strings.ToLower(event.Visibility)
	knownPublic := visibility == "public" || provider == "microsoft" && visibility == "normal" ||
		provider == "apple" && visibility == "" || provider == "google" && (visibility == "" || visibility == "default")
	knownPrivate := visibility == "private" || visibility == "confidential" || provider == "microsoft" && visibility == "personal"
	cancelled := strings.EqualFold(event.Status, "cancelled")
	// Cancellation receipts may omit visibility. Only principals holding
	// private access can use such an ID, and their receipt has no content.
	if cancelled && visibility == "" && grant.Class != GatewayPrivateAccess {
		return gatewayEventRow{}, false, nil
	}
	if grant.Class == GatewayMetadataOnly || !knownPublic && !(grant.Class == GatewayPrivateAccess && (knownPrivate || cancelled)) {
		return gatewayEventRow{}, false, nil
	}
	if event.ID == "" || len(event.ID) > cachedPageMaxIDBytes || len(event.RecurringEventID) > cachedPageMaxIDBytes {
		return gatewayEventRow{}, false, errors.New("invalid gateway event identity")
	}
	event.Provider, event.CalendarID = provider, grant.Metadata.ID
	event.ID = gatewayEventID(provider, event.ID)
	if event.RecurringEventID != "" {
		event.RecurringEventID = gatewayEventID(provider, event.RecurringEventID)
	}
	row := gatewayEventRow{ID: event.ID, Series: event.RecurringEventID, Cancelled: cancelled,
		Master: strings.EqualFold(event.InstanceKind, "seriesMaster") || event.RecurringEventID == "" && len(event.Recurrence) > 0}
	if cancelled {
		event = calendar.EventV2{ID: event.ID, CalendarID: event.CalendarID, Provider: provider, Start: event.Start, End: event.End,
			OriginalStart: event.OriginalStart, RecurringEventID: event.RecurringEventID, InstanceKind: event.InstanceKind, Status: "cancelled"}
	}
	row.Kind = "none"
	if event.Start.IsAllDay() && event.End.IsAllDay() && event.End.Date > event.Start.Date {
		if _, err := time.Parse(calendar.DateLayout, event.Start.Date); err != nil {
			return row, false, err
		}
		if _, err := time.Parse(calendar.DateLayout, event.End.Date); err != nil {
			return row, false, err
		}
		row.Kind, row.Start, row.End = "date", event.Start.Date, event.End.Date
	} else if start, err := event.Start.Instant(); err == nil {
		if end, err := event.End.Instant(); err == nil && !end.Before(start) {
			if end.Equal(start) {
				end = end.Add(time.Nanosecond)
			}
			row.Kind, row.Start, row.End = "timed", gatewayInstant(start), gatewayInstant(end)
		}
	}
	if row.Kind == "none" && cancelled && event.OriginalStart != nil {
		original := *event.OriginalStart
		if day, err := time.Parse(calendar.DateLayout, original.Date); err == nil {
			row.Kind, row.Start, row.End = "date", original.Date, day.AddDate(0, 0, 1).Format(calendar.DateLayout)
		} else if at, err := original.Instant(); err == nil {
			row.Kind, row.Start, row.End = "timed", gatewayInstant(at), gatewayInstant(at.Add(time.Nanosecond))
		}
	}
	if row.Kind == "none" && !cancelled {
		return row, false, errors.New("invalid gateway event time range")
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return row, false, err
	}
	if len(raw) > maxGatewayPayload {
		// Keep timing and identity for the summary marker, including the
		// provider's precise civil-date or offset representation.
		row.Oversized = true
		raw, err = json.Marshal(calendar.EventV2{ID: event.ID, CalendarID: event.CalendarID, Provider: event.Provider,
			Start: event.Start, End: event.End, OriginalStart: event.OriginalStart, Status: event.Status})
		if err != nil || len(raw) > maxGatewayPayload {
			return row, false, ErrGatewayQueryTooLarge
		}
	}
	row.Payload = string(raw)
	return row, true, nil
}

func gatewayEventID(provider, id string) string {
	if strings.HasPrefix(id, provider+":") {
		return id
	}
	return provider + ":" + id
}
