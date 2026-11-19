package runtime

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/config"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
)

type blockedPublicationListing struct {
	calendar.EventProviderV2
	started chan struct{}
}

func (*blockedPublicationListing) Name() string      { return "apple" }
func (*blockedPublicationListing) RouteName() string { return "apple@fixture" }
func (*blockedPublicationListing) OwnsCalendar(raw string) bool {
	return raw == "source" || raw == "target"
}

func (*blockedPublicationListing) SyncEvents(context.Context, calendar.EventSyncRequest) (calendar.EventSyncPage, error) {
	return calendar.EventSyncPage{NextCursor: "fixture", Complete: true}, nil
}

func (p *blockedPublicationListing) ListEventsV2(ctx context.Context, request calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	if request.AvailabilityStrict {
		select {
		case p.started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return calendar.Page[calendar.EventV2]{}, ctx.Err()
	}
	return calendar.Page[calendar.EventV2]{Complete: true}, nil
}

type publicationProviderBuilder struct{ provider calendar.Provider }

func (b publicationProviderBuilder) Build(context.Context) ([]calendar.Provider, error) {
	return []calendar.Provider{b.provider}, nil
}

func TestGatewayPublicationDoesNotBlockRuleJobs(t *testing.T) {
	ctx := t.Context()
	now := time.Now().UTC()
	earlier := now.Add(-time.Minute)
	cfg := &config.Config{GatewayCachedReadsPrepare: true, EventReadModelEnabled: true, EventCacheLookbackDays: 1, EventCacheLookaheadDays: 1,
		EventSyncAppleInterval: time.Minute, EventSyncGoogleInterval: time.Minute, EventSyncMicrosoftInterval: time.Minute}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	db, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "worker.db"))
	must(err)
	defer db.Close()
	must(db.Migrate(ctx))
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	must(err)
	db.SetArtifactCipher(cipher)
	must(db.CreateConnection(ctx, storage.Connection{ID: "fixture", Provider: "apple", Status: "connected",
		EncryptedCredentials: []byte("fixture"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: earlier, UpdatedAt: earlier}))
	source := calendar.AccountCalendarID("apple", "fixture", "source")
	target := calendar.AccountCalendarID("apple", "fixture", "target")
	lease, err := db.ClaimGatewayAuthorityCheck(ctx, "fixture", earlier, time.Minute)
	must(err)
	proof := func(id string) storage.GatewayCalendarProof {
		return storage.GatewayCalendarProof{Metadata: calendar.Calendar{ID: id, Provider: "apple"},
			Capabilities: calendar.CalendarCapabilities{CalendarID: id, Provider: "apple", Operations: calendar.OperationCapabilities{Create: true}},
			Class:        storage.GatewayPrivateAccess}
	}
	must(db.PublishGatewayAuthority(ctx, lease, []storage.GatewayCalendarProof{proof(source), proof(target)}, earlier))
	must(db.EnsureCalendarSyncStates(ctx, earlier, eventReadModelWindow(cfg, now)))
	state, err := db.ClaimDueCalendarSync(ctx, "seed", earlier, earlier.Add(time.Minute))
	must(err)
	if state == nil || state.CalendarID != source {
		t.Fatalf("wanted source sync, got %#v", state)
	}
	must(db.ApplyEventSyncPage(ctx, *state, storage.EventSyncBatch{NextCursor: "seed", NextSyncAt: &now}, true, earlier))
	must(db.CreateRule(ctx, storage.Rule{ID: "rule", SourceCalendarID: source, TargetCalendarID: target, State: "paused",
		IntervalSeconds: 600, LookaheadDays: 1, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: earlier, UpdatedAt: earlier}))
	must(db.EnqueueJob(ctx, storage.Job{ID: "job", RuleID: "rule", Kind: "dry_run", State: "pending", AvailableAt: earlier, CreatedAt: earlier}))

	listing := &blockedPublicationListing{started: make(chan struct{}, 1)}
	factory := publicationProviderBuilder{listing}
	publisherCtx, cancel := context.WithCancel(ctx)
	publisherDone := make(chan struct{})
	go func() {
		defer close(publisherDone)
		runGatewayPublication(publisherCtx, db, factory, time.Minute)
	}()
	defer func() { cancel(); <-publisherDone }()
	select {
	case <-listing.started:
	case <-time.After(time.Second):
		t.Fatal("startup scheduler did not pick up the durable source generation")
	}

	cycle, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	must(runWorkerCycleWithConfig(cycle, db, factory, cfg, now))
	runs, err := db.ListRuns(ctx, 1)
	must(err)
	if len(runs) != 1 || runs[0].JobID != "job" || runs[0].Outcome != "succeeded" {
		t.Fatalf("rule job was not finished while publication was blocked: %#v", runs)
	}
}

type publicationQueueFixture struct {
	blockedPublicationListing
	starts chan string
}

func (*publicationQueueFixture) OwnsCalendar(string) bool { return true }

func (p *publicationQueueFixture) ListEventsV2(ctx context.Context, request calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	p.starts <- request.CalendarID
	if strings.HasSuffix(request.CalendarID, "-slow") {
		<-ctx.Done()
		return calendar.Page[calendar.EventV2]{}, ctx.Err()
	}
	if request.CalendarID == "03-fails" {
		return calendar.Page[calendar.EventV2]{}, errors.New("fixture provider unavailable")
	}
	return calendar.Page[calendar.EventV2]{Complete: true}, nil
}

func TestGatewayPublicationQueueIsolatesSlowAndFailedSources(t *testing.T) {
	ctx := t.Context()
	now := time.Now().UTC()
	earlier := now.Add(-time.Minute)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	store, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "concurrent.db"))
	must(err)
	defer store.Close()
	must(store.Migrate(ctx))
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	must(err)
	store.SetArtifactCipher(cipher)
	must(store.CreateConnection(ctx, storage.Connection{ID: "fixture", Provider: "apple", Status: "connected",
		EncryptedCredentials: []byte("fixture"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: earlier, UpdatedAt: earlier}))

	rawIDs := []string{"01-slow", "02-slow", "03-fails", "04-healthy", "05-healthy"}
	proofs := make([]storage.GatewayCalendarProof, 0, len(rawIDs))
	for _, raw := range rawIDs {
		id := calendar.AccountCalendarID("apple", "fixture", raw)
		proofs = append(proofs, storage.GatewayCalendarProof{Metadata: calendar.Calendar{ID: id, Provider: "apple"},
			Capabilities: calendar.CalendarCapabilities{CalendarID: id, Provider: "apple"}, Class: storage.GatewayEventAccess})
	}
	lease, err := store.ClaimGatewayAuthorityCheck(ctx, "fixture", earlier, time.Minute)
	must(err)
	must(store.PublishGatewayAuthority(ctx, lease, proofs, earlier))
	must(store.EnsureCalendarSyncStates(ctx, earlier, storage.SyncWindow{Start: earlier, End: now.Add(time.Hour)}))
	next := now.Add(time.Hour)
	for range rawIDs {
		state, err := store.ClaimDueCalendarSync(ctx, "seed", earlier, now.Add(time.Minute))
		must(err)
		if state == nil {
			t.Fatal("missing ready fixture source")
		}
		must(store.ApplyEventSyncPage(ctx, *state, storage.EventSyncBatch{NextCursor: "seed", NextSyncAt: &next}, true, earlier))
	}

	provider := &publicationQueueFixture{starts: make(chan string, 32)}
	run, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		runGatewayPublication(run, store, publicationProviderBuilder{provider}, 20*time.Millisecond)
	}()
	defer func() { cancel(); <-stopped }()

	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	started := make(map[string]bool, len(rawIDs))
	for len(started) < len(rawIDs) {
		select {
		case id := <-provider.starts:
			if started[id] {
				t.Fatalf("duplicate in-flight or backed-off source %s", id)
			}
			started[id] = true
		case <-timeout.C:
			t.Fatalf("slow or failed sources blocked the healthy ones: %v", started)
		}
	}
	// Wait for completed publication, not just the start of the healthy jobs.
	for {
		ids, err := store.GatewayPublicationCandidates(ctx, "", time.Now())
		must(err)
		if len(ids) == 3 {
			if !strings.HasSuffix(ids[0], ":01-slow") || !strings.HasSuffix(ids[1], ":02-slow") || !strings.HasSuffix(ids[2], ":03-fails") {
				t.Fatalf("unexpected sources still awaiting publication: %v", ids)
			}
			break
		}
		select {
		case id := <-provider.starts:
			t.Fatalf("source unexpectedly retried: %s", id)
		case <-timeout.C:
			t.Fatal("healthy snapshots were not published")
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case id := <-provider.starts:
		t.Fatalf("source unexpectedly retried on scheduler tick: %s", id)
	case <-time.After(80 * time.Millisecond):
	}
}
