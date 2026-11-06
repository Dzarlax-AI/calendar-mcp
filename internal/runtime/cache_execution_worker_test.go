package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/calendarprojection"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/eventsync"
	"calendar-mcp/internal/storage"
)

type syntheticCacheTarget struct {
	previewProvider
	creates, updates, deletes, sourceReads int
	failCreate                             bool
	marker                                 *calendar.SyncMarker
	etag                                   string
	bootstrapPage                          calendar.EventSyncPage
	bootstrapCalls                         int
	createStarted, createRelease           chan struct{}
	honorCancellation                      bool
}

func (p *syntheticCacheTarget) SyncEvents(context.Context, calendar.EventSyncRequest) (calendar.EventSyncPage, error) {
	p.bootstrapCalls++
	if p.bootstrapCalls != 1 {
		return calendar.EventSyncPage{}, errors.New("unexpected source sync")
	}
	return p.bootstrapPage, nil
}

func (p *syntheticCacheTarget) SupportsNoAttendeeMutations() bool { return true }
func (p *syntheticCacheTarget) ListEventsV2(context.Context, calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	p.sourceReads++
	return calendar.Page[calendar.EventV2]{}, errors.New("source provider must not be called")
}
func (p *syntheticCacheTarget) FindEventBySyncMarkerV2(context.Context, string, string, string) (*calendar.EventV2, error) {
	return nil, nil
}
func (p *syntheticCacheTarget) CreateEventV2(ctx context.Context, r calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	if !r.RequireNoAttendees || r.Notifications != calendar.NotificationsNone || len(r.Event.Attendees) != 0 || len(r.Event.Recurrence) != 0 {
		return nil, errors.New("unsafe create")
	}
	p.creates++
	if p.createStarted != nil {
		close(p.createStarted)
		if p.honorCancellation {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		<-p.createRelease
		return nil, context.DeadlineExceeded
	}
	if p.failCreate {
		return nil, context.DeadlineExceeded
	}
	p.marker = r.Event.SyncMarker
	p.etag = "etag-1"
	return &calendar.EventV2{ID: "synthetic-copy", Status: "confirmed", ETag: p.etag, Google: &calendar.GoogleEventExtension{PrivateProperties: map[string]string{"calendar_sync_rule": p.marker.RuleID, "calendar_source_event": p.marker.SourceEventID}}}, nil
}
func (p *syntheticCacheTarget) UpdateEventV2(_ context.Context, r calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	if !r.RequireNoAttendees || r.Notifications != calendar.NotificationsNone || r.Patch.Attendees.Present || r.ExpectedETag != p.etag || r.ExpectedSyncMarker == nil || *r.ExpectedSyncMarker != *p.marker {
		return nil, errors.New("unsafe update")
	}
	p.updates++
	p.etag = "etag-2"
	return &calendar.OperationResult{Status: "completed", Event: &calendar.EventV2{ID: r.Ref.EventID, ETag: p.etag, Google: &calendar.GoogleEventExtension{PrivateProperties: map[string]string{"calendar_sync_rule": p.marker.RuleID, "calendar_source_event": p.marker.SourceEventID}}}}, nil
}
func (p *syntheticCacheTarget) DeleteEventV2(_ context.Context, r calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	if !r.RequireNoAttendees || r.Notifications != calendar.NotificationsNone || r.ExpectedETag != p.etag || r.ExpectedSyncMarker == nil || *r.ExpectedSyncMarker != *p.marker {
		return nil, errors.New("unsafe delete")
	}
	p.deletes++
	return &calendar.OperationResult{Status: "completed"}, nil
}

func TestSyntheticCacheWorkerCreateUpdateCancellationWithoutSourceProvider(t *testing.T) {
	runSyntheticCacheWorker(t, "timeout")
}
func TestSyntheticStopDuringProviderCallParksUnknownOutcome(t *testing.T) {
	runSyntheticCacheWorker(t, "late")
}

func TestSyntheticStopCancelsWorkerContext(t *testing.T) { runSyntheticCacheWorker(t, "cancel") }

func runSyntheticCacheWorker(t *testing.T, mode string) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "cache-execution.db")
	s, err := storage.Open(ctx, "sqlite://"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	s.SetArtifactCipher(cipher)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err = s.CreateConnection(ctx, storage.Connection{ID: "account", Provider: "google", AccountFingerprint: "account", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"google:source", "google:target"} {
		if err = s.UpsertCalendar(ctx, storage.Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: id, Name: "synthetic", Timezone: "UTC", CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "google:target", State: "paused", SyncMode: "cached_instances", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
	if err = s.CreateRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	start, end := now.Add(-time.Hour), now.AddDate(0, 0, 30)
	if err = s.EnsureCalendarSyncStates(ctx, now, storage.SyncWindow{Start: start, End: end}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	startEvent := calendar.EventTime{DateTime: now.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"}
	endEvent := calendar.EventTime{DateTime: now.Add(2 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}
	event := calendar.EventV2{CalendarID: rule.SourceCalendarID, Provider: "google", ID: "instance", RecurringEventID: "series", OriginalStart: &startEvent, Start: startEvent, End: endEvent, Title: "first", SourceAttendeesComplete: true}
	identity, err := calendarprojection.Identity(rule.SourceCalendarID, event)
	if err != nil {
		t.Fatal(err)
	}
	publishFixture := func(revision int64, events []calendar.EventV2, observations []calendarprojection.RemovalObservation) {
		t.Helper()
		evidence := make([]map[string]any, 0, len(events))
		for _, e := range events {
			evidence = append(evidence, map[string]any{"event_id": e.ID, "complete": e.SourceAttendeesComplete})
		}
		payload, err := json.Marshal(map[string]any{"version": 1, "events": events, "attendee_evidence": evidence})
		if err != nil {
			t.Fatal(err)
		}
		journal, err := json.Marshal(observations)
		if err != nil {
			t.Fatal(err)
		}
		if observations == nil {
			journal = []byte(`[]`)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err = tx.ExecContext(ctx, `INSERT INTO projection_mutation_guards(calendar_id,epoch,revision) VALUES(?,0,?) ON CONFLICT(calendar_id) DO UPDATE SET revision=excluded.revision`, rule.SourceCalendarID, revision); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO projection_snapshots(calendar_id,revision,stage_id,guard_epoch,base_revision,window_start,window_end,completed_at,timezone,cursor,payload,verified) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, rule.SourceCalendarID, revision, fmt.Sprintf("synthetic-stage-%016d", revision), 0, revision-1, start, end, now, "UTC", "synthetic-cursor", payload, true); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO projection_revision_provenance(calendar_id,revision,observations) VALUES(?,?,?)`, rule.SourceCalendarID, revision, journal); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE calendar_sync_state SET status='ready',cursor=?,last_success_at=? WHERE calendar_id=?`, "synthetic-cursor", now, rule.SourceCalendarID); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	target := &syntheticCacheTarget{previewProvider: previewProvider{name: "google"}}
	run := func(id string) {
		t.Helper()
		at := time.Now().UTC()
		if err := s.EnqueueJob(ctx, storage.Job{ID: id, RuleID: rule.ID, Kind: "manual", State: "pending", AvailableAt: at, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
		job, err := s.ClaimJob(ctx, "worker", at)
		if err != nil || job == nil {
			t.Fatalf("claim: %v", err)
		}
		if err = executeJob(ctx, s, previewBuilder{providers: []calendar.Provider{target}}, *job, at); err != nil {
			t.Fatal(err)
		}
	}
	// Exercise the actual background bootstrap and verified publication against
	// one synthetic source page, then forbid any further source provider reads
	// during the cache-backed target runs.
	target.bootstrapPage = calendar.EventSyncPage{SourceTimeZone: "UTC", Complete: true, NextCursor: "synthetic-cursor", Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: event.ID}, Event: event}}}
	if _, err = db.ExecContext(ctx, `UPDATE calendar_sync_state SET next_sync_at=? WHERE calendar_id=?`, now.Add(24*time.Hour), rule.TargetCalendarID); err != nil {
		t.Fatal(err)
	}
	claimAt := time.Now().UTC()
	sourceState, err := s.ClaimDueCalendarSync(ctx, "synthetic-source", claimAt, claimAt.Add(5*time.Minute))
	if err != nil || sourceState == nil || sourceState.CalendarID != rule.SourceCalendarID {
		t.Fatalf("source bootstrap claim: %+v %v", sourceState, err)
	}
	service := eventsync.NewService(s, calendar.NewRegistry([]calendar.Provider{target}), nil)
	if err = service.RunOne(ctx, *sourceState); err != nil {
		t.Fatalf("source bootstrap: %v", err)
	}
	if target.bootstrapCalls != 1 {
		t.Fatalf("source bootstrap calls=%d", target.bootstrapCalls)
	}
	// Real guarded enable requires a successful local preview.
	dryAt := time.Now().UTC()
	if err = s.EnqueueJob(ctx, storage.Job{ID: "dry-run", RuleID: rule.ID, Kind: "dry_run", State: "pending", AvailableAt: dryAt, CreatedAt: dryAt}); err != nil {
		t.Fatal(err)
	}
	dryJob, err := s.ClaimJob(ctx, "worker", dryAt)
	if err != nil || dryJob == nil {
		t.Fatalf("dry-run claim: %v", err)
	}
	noSource := &forbiddenPreviewBuilder{}
	if err = executeJob(ctx, s, noSource, *dryJob, dryAt); err != nil || noSource.calls != 0 {
		t.Fatalf("local dry-run: %v provider builds=%d", err, noSource.calls)
	}
	if err = s.EnableCachedRuleIfReady(ctx, rule.ID, time.Now().UTC(), 15*time.Minute); err != nil {
		t.Fatalf("guarded enable: %v", err)
	}
	enabled, err := s.RuleByID(ctx, rule.ID)
	if err != nil || enabled.State != "enabled" {
		t.Fatalf("enabled rule: %+v %v", enabled, err)
	}
	run("create")
	event.Title = "changed"
	publishFixture(2, []calendar.EventV2{event}, nil)
	run("update")
	obs := calendarprojection.RemovalObservation{Removal: calendar.EventSyncRemoval{ObjectID: event.ID, Reason: calendar.EventSyncProviderCancellation}, PreviousIdentity: identity}
	publishFixture(3, []calendar.EventV2{}, []calendarprojection.RemovalObservation{obs})
	run("cancel")
	if target.creates != 1 || target.updates != 1 || target.deletes != 1 || target.sourceReads != 0 || target.bootstrapCalls != 1 {
		t.Fatalf("create=%d update=%d delete=%d source reads=%d background calls=%d", target.creates, target.updates, target.deletes, target.sourceReads, target.bootstrapCalls)
	}
	// Failure before any provider mutation releases the reservation so another
	// run can proceed; the following timeout run proves it was not parked.
	at := time.Now().UTC()
	if err = s.EnqueueJob(ctx, storage.Job{ID: "before-dispatch", RuleID: rule.ID, Kind: "manual", State: "pending", AvailableAt: at, CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	beforeJob, err := s.ClaimJob(ctx, "worker", at)
	if err != nil || beforeJob == nil {
		t.Fatalf("pre-dispatch claim: %v", err)
	}
	if err = executeJob(ctx, s, failingProviderBuilder{}, *beforeJob, at); err == nil {
		t.Fatal("expected provider setup failure")
	}
	// A provider timeout after dispatch leaves both the mapping intent and the
	// shared run reservation parked; another rule cannot start writing.
	second := event
	second.ID = "second-event"
	second.RecurringEventID = ""
	second.OriginalStart = nil
	publishFixture(4, []calendar.EventV2{second}, []calendarprojection.RemovalObservation{obs})
	target.failCreate = true
	at = time.Now().UTC()
	if err = s.EnqueueJob(ctx, storage.Job{ID: "timeout", RuleID: rule.ID, Kind: "manual", State: "pending", AvailableAt: at, CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimJob(ctx, "worker", at)
	if err != nil || job == nil {
		t.Fatalf("timeout claim: %v", err)
	}
	if mode != "timeout" {
		target.createStarted, target.createRelease = make(chan struct{}), make(chan struct{})
		target.honorCancellation = mode == "cancel"
		finished := make(chan error, 1)
		go func() {
			finished <- executeJob(ctx, s, previewBuilder{providers: []calendar.Provider{target}}, *job, at)
		}()
		select {
		case <-target.createStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("provider call did not start")
		}
		if err = s.RequestRulePause(ctx, rule.ID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if status, e := s.RuleControlStatus(ctx, rule.ID); e != nil || status != "pausing" {
			t.Fatalf("in-flight status %q %v", status, e)
		}
		if err = s.ArchiveRule(ctx, rule.ID, time.Now().UTC()); !errors.Is(err, storage.ErrSyncControlConflict) {
			t.Fatalf("archive in-flight: %v", err)
		}
		if mode == "late" {
			close(target.createRelease)
		}
		select {
		case e := <-finished:
			want := context.DeadlineExceeded
			if mode == "cancel" {
				want = context.Canceled
			}
			if !errors.Is(e, want) {
				t.Fatalf("stopped run: %v", e)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not stop")
		}
		runs, e := s.ListRuns(ctx, 10)
		if e != nil || len(runs) == 0 || runs[0].Outcome != "unknown" {
			t.Fatalf("stop result: %+v %v", runs, e)
		}
		if status, e := s.RuleControlStatus(ctx, rule.ID); e != nil || status != "needs_review" {
			t.Fatalf("unknown status %q %v", status, e)
		}
		if err = s.ArchiveRule(ctx, rule.ID, time.Now().UTC()); !errors.Is(err, storage.ErrSyncControlConflict) {
			t.Fatalf("archive unknown: %v", err)
		}
	} else if err = executeJob(ctx, s, previewBuilder{providers: []calendar.Provider{target}}, *job, at); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout run: %v", err)
	}
	probe := storage.ExclusiveDispatchRequest{OperationID: "another-synthetic-run-0001", TargetKey: storage.GlobalSyncDispatchKey, RequestHash: "0000000000000000000000000000000000000000000000000000000000000000", Kind: "run", CalendarIDs: []string{rule.TargetCalendarID}, Now: time.Now().UTC()}
	if err = s.BeginExclusiveDispatch(ctx, probe); !errors.Is(err, storage.ErrExclusiveDispatchConflict) {
		t.Fatalf("uncertain run released global guard: %v", err)
	}
}
