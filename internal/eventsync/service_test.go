package eventsync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type fakeResolver struct {
	provider calendar.Provider
	rawID    string
	err      error
}

func (r fakeResolver) Resolve(string) (calendar.Provider, string, error) {
	return r.provider, r.rawID, r.err
}

type fakeProvider struct {
	calendar.Provider
	pages          []calendar.EventSyncPage
	errs           []error
	requests       []calendar.EventSyncRequest
	repairResults  []calendar.EventSyncObjectRepairResult
	repairErrs     []error
	repairRequests []calendar.EventSyncObjectRepairRequest
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) SyncEvents(_ context.Context, request calendar.EventSyncRequest) (calendar.EventSyncPage, error) {
	p.requests = append(p.requests, request)
	i := len(p.requests) - 1
	if i < len(p.errs) && p.errs[i] != nil {
		return calendar.EventSyncPage{}, p.errs[i]
	}
	if i >= len(p.pages) {
		return calendar.EventSyncPage{}, &calendar.EventSyncError{Class: calendar.EventSyncProtocol}
	}
	return p.pages[i], nil
}

func (p *fakeProvider) RepairEventSyncObject(_ context.Context, request calendar.EventSyncObjectRepairRequest) (calendar.EventSyncObjectRepairResult, error) {
	p.repairRequests = append(p.repairRequests, request)
	i := len(p.repairRequests) - 1
	if i < len(p.repairErrs) && p.repairErrs[i] != nil {
		return calendar.EventSyncObjectRepairResult{}, p.repairErrs[i]
	}
	if i >= len(p.repairResults) {
		return calendar.EventSyncObjectRepairResult{}, &calendar.EventSyncError{Class: calendar.EventSyncProtocol}
	}
	return p.repairResults[i], nil
}

type legacyProvider struct{ calendar.Provider }

func (legacyProvider) Name() string { return "legacy" }

type policyProvider struct {
	*fakeProvider
	policy      calendar.EventSyncPolicy
	policyCalls int
}

func (p *policyProvider) EventSyncPolicy() calendar.EventSyncPolicy {
	p.policyCalls++
	return p.policy
}

type appliedPage struct {
	state storage.CalendarSyncState
	batch storage.EventSyncBatch
	final bool
}

type fakeStore struct {
	applied    []appliedPage
	fails      []string
	failAt     []time.Time
	parks      []string
	resets     int
	applyErrAt int
	applyErr   error
	resetErr   error
	quarantine []storage.CalendarSyncQuarantine
	repairs    []storage.EventSyncRepairBatch
}

func (s *fakeStore) ApplyEventSyncPage(_ context.Context, state storage.CalendarSyncState, batch storage.EventSyncBatch, final bool, _ time.Time) error {
	s.applied = append(s.applied, appliedPage{state: state, batch: batch, final: final})
	if s.applyErr != nil && len(s.applied) == s.applyErrAt {
		return s.applyErr
	}
	return nil
}

func (s *fakeStore) FailCalendarSync(_ context.Context, _ storage.CalendarSyncState, code string, _, next time.Time) error {
	s.fails = append(s.fails, code)
	s.failAt = append(s.failAt, next)
	return nil
}

func (s *fakeStore) ParkCalendarSync(_ context.Context, _ storage.CalendarSyncState, code string, _ time.Time) error {
	s.parks = append(s.parks, code)
	return nil
}

func (s *fakeStore) ResetCalendarSync(_ context.Context, state storage.CalendarSyncState, _ time.Time) (*storage.CalendarSyncState, error) {
	s.resets++
	if s.resetErr != nil {
		return nil, s.resetErr
	}
	state.Cursor = ""
	state.Generation++
	return &state, nil
}

func (s *fakeStore) ListDueEventSyncQuarantine(_ context.Context, _ storage.CalendarSyncState, _ time.Time, limit int) ([]storage.CalendarSyncQuarantine, error) {
	if limit > len(s.quarantine) {
		limit = len(s.quarantine)
	}
	return s.quarantine[:limit], nil
}

func (s *fakeStore) ApplyEventSyncObjectRepair(_ context.Context, _ storage.CalendarSyncState, batch storage.EventSyncRepairBatch, _ time.Time) error {
	s.repairs = append(s.repairs, batch)
	return nil
}

func testState(cursor string) storage.CalendarSyncState {
	return storage.CalendarSyncState{CalendarID: "fake:calendar", Cursor: cursor, Generation: 4, LeaseOwner: "worker", WindowStart: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC), WindowEnd: time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)}
}

func newService(store *fakeStore, provider calendar.Provider) *Service {
	return &Service{
		Store:    store,
		Resolver: fakeResolver{provider: provider, rawID: "provider-calendar"},
		Now:      func() time.Time { return time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC) },
		PolicyFor: func(calendar.Provider) calendar.EventSyncPolicy {
			return calendar.EventSyncPolicy{PollInterval: 10 * time.Minute, RetryBase: time.Minute, RetryMax: 5 * time.Minute, MaxPages: 10, MaxResets: 1}
		},
	}
}

func repairItem(id string) storage.CalendarSyncQuarantine {
	return storage.CalendarSyncQuarantine{CalendarID: "fake:calendar", ObjectID: id, ETag: "etag-" + id, Active: true}
}

func repairResult(id string, outcome calendar.EventSyncObjectRepairOutcome) calendar.EventSyncObjectRepairResult {
	result := calendar.EventSyncObjectRepairResult{Object: calendar.SyncObject{ObjectID: id, ETag: "etag-" + id}, Outcome: outcome}
	if outcome == calendar.EventSyncObjectStillQuarantined {
		result.Warning = &calendar.EventSyncWarning{ObjectID: id, ETag: "etag-" + id, Code: calendar.EventSyncProtocol}
	}
	return result
}

func TestRepairOneProcessesAllDueObjectsAndPersistsOutcomes(t *testing.T) {
	provider := &fakeProvider{repairResults: []calendar.EventSyncObjectRepairResult{
		repairResult("replace", calendar.EventSyncObjectReplaceMembership),
		repairResult("absent", calendar.EventSyncObjectAbsentFromProjection),
		repairResult("deleted", calendar.EventSyncObjectProviderDeleted),
		repairResult("quarantined", calendar.EventSyncObjectStillQuarantined),
	}}
	store := &fakeStore{quarantine: []storage.CalendarSyncQuarantine{repairItem("replace"), repairItem("absent"), repairItem("deleted"), repairItem("quarantined")}}
	service := newService(store, provider)
	policy := service.policy(provider)
	policy.MaxObjectRepairsPerRun = 4
	if err := service.repairOne(t.Context(), testState("saved"), provider, "provider-calendar", policy); err != nil {
		t.Fatal(err)
	}
	if len(provider.repairRequests) != 4 || len(store.repairs) != 4 {
		t.Fatalf("repair requests=%d persisted=%d", len(provider.repairRequests), len(store.repairs))
	}
	for i, want := range []calendar.EventSyncObjectRepairOutcome{calendar.EventSyncObjectReplaceMembership, calendar.EventSyncObjectAbsentFromProjection, calendar.EventSyncObjectProviderDeleted, calendar.EventSyncObjectStillQuarantined} {
		if store.repairs[i].Outcome != want {
			t.Fatalf("repair[%d] outcome=%q, want %q", i, store.repairs[i].Outcome, want)
		}
	}
}

func TestRepairOneSeparatesAuthAndTransientFailures(t *testing.T) {
	t.Run("auth failure parks run", func(t *testing.T) {
		provider := &fakeProvider{repairErrs: []error{&calendar.EventSyncError{Class: calendar.EventSyncAuth}}}
		store := &fakeStore{quarantine: []storage.CalendarSyncQuarantine{repairItem("auth")}}
		if err := newService(store, provider).RunOne(t.Context(), testState("saved")); !errors.Is(err, ErrProviderFailure) || len(store.parks) != 1 || len(store.fails) != 0 {
			t.Fatalf("err=%v parks=%#v fails=%#v", err, store.parks, store.fails)
		}
	})
	t.Run("transient failure persists repair and continues feed", func(t *testing.T) {
		provider := &fakeProvider{repairErrs: []error{&calendar.EventSyncError{Class: calendar.EventSyncTransient}}, pages: []calendar.EventSyncPage{terminal("advanced", "event")}}
		store := &fakeStore{quarantine: []storage.CalendarSyncQuarantine{repairItem("transient")}}
		if err := newService(store, provider).RunOne(t.Context(), testState("saved")); err != nil {
			t.Fatal(err)
		}
		if len(store.repairs) != 1 || store.repairs[0].Outcome != calendar.EventSyncObjectStillQuarantined || len(store.applied) != 1 {
			t.Fatalf("repairs=%#v applied=%#v", store.repairs, store.applied)
		}
	})
}

func TestRepairBatchRejectsUnknownOutcome(t *testing.T) {
	_, err := repairBatch(calendar.EventSyncObjectRepairResult{Object: calendar.SyncObject{ObjectID: "object"}, Outcome: ""}, "fake:calendar", repairItem("object"))
	if !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("repairBatch() error = %v, want ErrInvalidPage", err)
	}
}

func terminal(cursor string, ids ...string) calendar.EventSyncPage {
	page := calendar.EventSyncPage{Complete: true, NextCursor: calendar.EventSyncCursor(cursor)}
	for _, id := range ids {
		page.Upserts = append(page.Upserts, calendar.EventSyncUpsert{Event: calendar.EventV2{ID: id}})
	}
	return page
}

func TestEventSyncCapabilityKeepsLegacyProvidersCompatible(t *testing.T) {
	if _, ok := calendar.EventSyncCapability(legacyProvider{}); ok {
		t.Fatal("legacy provider unexpectedly has event sync capability")
	}
	store := &fakeStore{}
	err := newService(store, legacyProvider{}).RunOne(context.Background(), testState(""))
	if !errors.Is(err, ErrCapabilityUnavailable) {
		t.Fatalf("RunOne() error = %v, want capability unavailable", err)
	}
	if got := store.parks; len(got) != 1 || got[0] != string(calendar.EventSyncUnsupported) || len(store.fails) != 0 {
		t.Fatalf("parked=%#v failures=%#v", got, store.fails)
	}
}

func TestRunOneDiscoversProviderEventSyncPolicy(t *testing.T) {
	provider := &policyProvider{
		fakeProvider: &fakeProvider{pages: []calendar.EventSyncPage{terminal("next")}},
		policy: calendar.EventSyncPolicy{
			PollInterval: 2 * time.Minute,
			RetryBase:    2 * time.Second,
			RetryMax:     30 * time.Second,
			MaxPages:     12,
			MaxResets:    3,
		},
	}
	store := &fakeStore{}
	service := &Service{
		Store:    store,
		Resolver: fakeResolver{provider: provider, rawID: "provider-calendar"},
		Now:      func() time.Time { return time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC) },
	}
	if err := service.RunOne(context.Background(), testState("saved")); err != nil {
		t.Fatal(err)
	}
	if provider.policyCalls != 1 {
		t.Fatalf("EventSyncPolicy() calls = %d, want 1", provider.policyCalls)
	}
	if got, want := *store.applied[0].batch.NextSyncAt, time.Date(2026, 8, 22, 12, 2, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("next sync = %s, want %s", got, want)
	}
}

func TestPolicyForOverridesProviderPolicyAndNormalizesValues(t *testing.T) {
	provider := &policyProvider{
		fakeProvider: &fakeProvider{},
		policy: calendar.EventSyncPolicy{
			PollInterval: time.Minute,
			RetryBase:    time.Second,
			RetryMax:     2 * time.Second,
			MaxPages:     1,
			MaxResets:    1,
		},
	}
	service := &Service{PolicyFor: func(calendar.Provider) calendar.EventSyncPolicy {
		return calendar.EventSyncPolicy{
			PollInterval: 7 * time.Minute,
			RetryBase:    10 * time.Minute,
			RetryMax:     5 * time.Minute,
			MaxPages:     0,
			MaxResets:    -1,
		}
	}}
	got := service.policy(provider)
	want := calendar.EventSyncPolicy{
		PollInterval:           7 * time.Minute,
		RetryBase:              10 * time.Minute,
		RetryMax:               10 * time.Minute,
		MaxPages:               defaultMaxPages,
		MaxResets:              defaultMaxResets,
		MaxObjectRepairsPerRun: defaultMaxObjectRepairsPerRun,
	}
	if got != want {
		t.Fatalf("policy = %#v, want %#v", got, want)
	}
	if provider.policyCalls != 0 {
		t.Fatalf("EventSyncPolicy() calls = %d, want 0 when PolicyFor is set", provider.policyCalls)
	}
}

func TestLegacyProviderPolicyUsesConservativeDefaults(t *testing.T) {
	got := (&Service{}).policy(legacyProvider{})
	want := calendar.EventSyncPolicy{
		PollInterval:           defaultPollInterval,
		RetryBase:              defaultRetryBase,
		RetryMax:               defaultRetryMax,
		MaxPages:               defaultMaxPages,
		MaxResets:              defaultMaxResets,
		MaxObjectRepairsPerRun: defaultMaxObjectRepairsPerRun,
	}
	if got != want {
		t.Fatalf("policy = %#v, want %#v", got, want)
	}
}

func TestRunOneInitialAndMultipageIncremental(t *testing.T) {
	t.Run("replacement", func(t *testing.T) {
		// Inventory-only adapters such as Apple can complete a replacement
		// snapshot without an incremental cursor. Persisting the empty cursor
		// deliberately makes the next claim another replacement sync.
		provider := &fakeProvider{pages: []calendar.EventSyncPage{terminal("", "first")}}
		store := &fakeStore{}
		if err := newService(store, provider).RunOne(context.Background(), testState("")); err != nil {
			t.Fatal(err)
		}
		if len(store.applied) != 1 || !store.applied[0].final || !store.applied[0].batch.FullSync || store.applied[0].batch.NextCursor != "" || store.applied[0].batch.Degraded || store.applied[0].batch.ErrorCode != "" {
			t.Fatalf("applied page = %#v", store.applied)
		}
		if got := provider.requests[0]; got.Mode != calendar.EventSyncReplacement || got.CalendarID != "provider-calendar" || got.Window.Start != testState("").WindowStart {
			t.Fatalf("request = %#v", got)
		}
	})
	t.Run("incremental pages", func(t *testing.T) {
		provider := &fakeProvider{pages: []calendar.EventSyncPage{
			{Upserts: []calendar.EventSyncUpsert{{Event: calendar.EventV2{ID: "one"}}}, NextPageToken: "next"},
			terminal("advanced", "two"),
		}}
		store := &fakeStore{}
		if err := newService(store, provider).RunOne(context.Background(), testState("saved")); err != nil {
			t.Fatal(err)
		}
		if len(store.applied) != 2 || store.applied[0].final || !store.applied[1].final || store.applied[0].batch.FullSync || store.applied[1].batch.NextCursor != "advanced" || store.applied[0].batch.Degraded || store.applied[1].batch.Degraded || store.applied[1].batch.ErrorCode != "" {
			t.Fatalf("applied pages = %#v", store.applied)
		}
		if provider.requests[1].PageToken != "next" || provider.requests[1].Cursor != "saved" || provider.requests[0].Mode != calendar.EventSyncIncremental {
			t.Fatalf("requests = %#v", provider.requests)
		}
	})
}

func TestRunOneWarningsDegradeAttemptAndAdvanceCursor(t *testing.T) {
	t.Run("intermediate warning followed by clean terminal page", func(t *testing.T) {
		provider := &fakeProvider{pages: []calendar.EventSyncPage{
			{NextPageToken: "next", Warnings: []calendar.EventSyncWarning{{Code: calendar.EventSyncProtocol, ObjectID: "bad-object"}}},
			terminal("advanced", "two"),
		}}
		store := &fakeStore{}
		if err := newService(store, provider).RunOne(t.Context(), testState("saved")); err != nil {
			t.Fatal(err)
		}
		if len(store.applied) != 2 || !store.applied[0].batch.Degraded || !store.applied[1].batch.Degraded || store.applied[0].batch.ErrorCode != string(calendar.EventSyncProtocol) || store.applied[1].batch.ErrorCode != string(calendar.EventSyncProtocol) || store.applied[1].batch.NextCursor != "advanced" {
			t.Fatalf("applied pages = %#v", store.applied)
		}
		if got, want := *store.applied[1].batch.NextSyncAt, time.Date(2026, 8, 22, 12, 10, 0, 0, time.UTC); !got.Equal(want) {
			t.Fatalf("degraded retry at = %s, want %s", got, want)
		}
	})

	t.Run("terminal warning", func(t *testing.T) {
		provider := &fakeProvider{pages: []calendar.EventSyncPage{{
			Complete:   true,
			NextCursor: "advanced",
			Warnings:   []calendar.EventSyncWarning{{Code: calendar.EventSyncProtocol, ObjectID: "bad-object"}},
		}}}
		store := &fakeStore{}
		if err := newService(store, provider).RunOne(t.Context(), testState("saved")); err != nil {
			t.Fatal(err)
		}
		if len(store.applied) != 1 || !store.applied[0].batch.Degraded || store.applied[0].batch.ErrorCode != string(calendar.EventSyncProtocol) || store.applied[0].batch.NextCursor != "advanced" {
			t.Fatalf("applied page = %#v", store.applied)
		}
		if got, want := *store.applied[0].batch.NextSyncAt, time.Date(2026, 8, 22, 12, 10, 0, 0, time.UTC); !got.Equal(want) {
			t.Fatalf("degraded retry at = %s, want %s", got, want)
		}
	})

	t.Run("repeated protocol degradation backs off to poll interval", func(t *testing.T) {
		provider := &fakeProvider{pages: []calendar.EventSyncPage{{
			Complete:   true,
			NextCursor: "advanced",
			Warnings:   []calendar.EventSyncWarning{{Code: calendar.EventSyncProtocol, ObjectID: "bad-object"}},
		}}}
		store := &fakeStore{}
		state := testState("saved")
		state.LastErrorCode = string(calendar.EventSyncProtocol)
		if err := newService(store, provider).RunOne(t.Context(), state); err != nil {
			t.Fatal(err)
		}
		if got, want := *store.applied[0].batch.NextSyncAt, time.Date(2026, 8, 22, 12, 10, 0, 0, time.UTC); !got.Equal(want) {
			t.Fatalf("degraded retry at = %s, want %s", got, want)
		}
	})
}

func TestRunOneFailureReplaysCursorAndSchedulesBoundedRateLimit(t *testing.T) {
	provider := &fakeProvider{pages: []calendar.EventSyncPage{{Upserts: []calendar.EventSyncUpsert{{Event: calendar.EventV2{ID: "first"}}}, NextPageToken: "page-two"}}, errs: []error{nil, &calendar.EventSyncError{Class: calendar.EventSyncRateLimited, RetryAfter: 20 * time.Minute}}}
	store := &fakeStore{}
	err := newService(store, provider).RunOne(context.Background(), testState("authoritative"))
	if !errors.Is(err, ErrProviderFailure) {
		t.Fatalf("RunOne() error = %v", err)
	}
	if len(store.applied) != 1 || store.applied[0].final || len(store.fails) != 1 || store.fails[0] != string(calendar.EventSyncRateLimited) {
		t.Fatalf("store state = %#v failures=%#v", store.applied, store.fails)
	}
	if got, want := store.failAt[0], time.Date(2026, 8, 22, 12, 20, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("retry at = %s, want %s", got, want)
	}
	// The authoritative cursor was never advanced by the intermediate page, so
	// a later claim safely asks the provider to replay from the original cursor.
	replay := &fakeProvider{pages: []calendar.EventSyncPage{terminal("advanced", "first")}}
	if err := newService(&fakeStore{}, replay).RunOne(context.Background(), testState("authoritative")); err != nil {
		t.Fatal(err)
	}
	if replay.requests[0].Cursor != "authoritative" {
		t.Fatalf("replay cursor = %q, want original cursor", replay.requests[0].Cursor)
	}
}

func TestRunOneBacksOffRepeatedTransientFailureToPollInterval(t *testing.T) {
	provider := &fakeProvider{errs: []error{&calendar.EventSyncError{Class: calendar.EventSyncTransient}}}
	store := &fakeStore{}
	state := testState("cursor")
	state.LastErrorCode = string(calendar.EventSyncTransient)
	service := newService(store, provider)
	service.PolicyFor = func(calendar.Provider) calendar.EventSyncPolicy {
		return calendar.EventSyncPolicy{PollInterval: time.Minute, RetryBase: 5 * time.Second, RetryMax: 5 * time.Minute, MaxPages: 10, MaxResets: 1}
	}
	if err := service.RunOne(t.Context(), state); !errors.Is(err, ErrProviderFailure) {
		t.Fatal(err)
	}
	if len(store.failAt) != 1 || !store.failAt[0].Equal(time.Date(2026, 8, 22, 12, 1, 0, 0, time.UTC)) {
		t.Fatalf("retry at=%v, want one-minute backoff", store.failAt)
	}
}

func TestRunOneResetThenReplacementAndObjectReplacementMapping(t *testing.T) {
	provider := &fakeProvider{pages: []calendar.EventSyncPage{
		{ResetRequired: true},
		{Complete: true, NextCursor: "replacement-cursor", Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: "resource.ics", ETag: "etag"}, Event: calendar.EventV2{ID: "member"}}}, ReplacedObjectIDs: []string{"resource.ics"}, Inventory: []calendar.SyncObject{{ObjectID: "resource.ics", ETag: "etag"}}},
	}}
	store := &fakeStore{}
	if err := newService(store, provider).RunOne(context.Background(), testState("expired")); err != nil {
		t.Fatal(err)
	}
	if store.resets != 1 || len(store.applied) != 1 || !store.applied[0].batch.FullSync || store.applied[0].batch.ReplacedObjectIDs[0] != "resource.ics" || store.applied[0].state.Generation != 5 {
		t.Fatalf("reset/applied state = resets=%d pages=%#v", store.resets, store.applied)
	}
	if provider.requests[1].Mode != calendar.EventSyncReplacement || provider.requests[1].Cursor != "" || provider.requests[1].Generation != 5 {
		t.Fatalf("replacement request = %#v", provider.requests[1])
	}
}

func TestInvalidPagesRepeatedTokensAndLeaseLossDoNotLeakOpaqueValues(t *testing.T) {
	cases := []struct {
		name  string
		pages []calendar.EventSyncPage
		want  error
	}{
		{"terminal continuation", []calendar.EventSyncPage{{Complete: true, NextCursor: "secret-cursor", NextPageToken: "secret-page"}}, ErrInvalidPage},
		{"incremental terminal without cursor", []calendar.EventSyncPage{{Complete: true}}, ErrInvalidPage},
		{"replacement without membership", []calendar.EventSyncPage{{Complete: true, NextCursor: "secret-cursor", ReplacedObjectIDs: []string{"object"}}}, ErrInvalidPage},
		{"repeated token", []calendar.EventSyncPage{{NextPageToken: "secret-page"}, {NextPageToken: "secret-page"}}, ErrRepeatedPage},
		{"warning with non-protocol code", []calendar.EventSyncPage{{Complete: true, NextCursor: "secret-cursor", Warnings: []calendar.EventSyncWarning{{Code: calendar.EventSyncTransient, ObjectID: "object"}}}}, ErrInvalidPage},
		{"warning without object ID", []calendar.EventSyncPage{{Complete: true, NextCursor: "secret-cursor", Warnings: []calendar.EventSyncWarning{{Code: calendar.EventSyncProtocol}}}}, ErrInvalidPage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			err := newService(store, &fakeProvider{pages: tc.pages}).RunOne(context.Background(), testState("existing-secret"))
			if !errors.Is(err, tc.want) {
				t.Fatalf("RunOne() error = %v, want %v", err, tc.want)
			}
			if len(store.parks) != 1 || len(store.fails) != 0 {
				t.Fatalf("invalid protocol page parked=%#v failures=%#v", store.parks, store.fails)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("opaque value leaked in error %q", err)
			}
		})
	}
	t.Run("lease loss", func(t *testing.T) {
		store := &fakeStore{applyErrAt: 1, applyErr: storage.ErrCalendarSyncLeaseLost}
		err := newService(store, &fakeProvider{pages: []calendar.EventSyncPage{terminal("safe")}}).RunOne(context.Background(), testState("existing"))
		if !errors.Is(err, storage.ErrCalendarSyncLeaseLost) || len(store.fails) != 0 {
			t.Fatalf("error=%v failures=%#v", err, store.fails)
		}
	})
}

func TestRunOneParksNonRetryableAndRetriesTransientFailures(t *testing.T) {
	parkCases := []struct {
		name  string
		err   error
		class calendar.EventSyncErrorClass
	}{
		{"auth", &calendar.EventSyncError{Class: calendar.EventSyncAuth}, calendar.EventSyncAuth},
		{"permission", &calendar.EventSyncError{Class: calendar.EventSyncPermission}, calendar.EventSyncPermission},
		{"unsupported", &calendar.EventSyncError{Class: calendar.EventSyncUnsupported}, calendar.EventSyncUnsupported},
		{"protocol", &calendar.EventSyncError{Class: calendar.EventSyncProtocol}, calendar.EventSyncProtocol},
	}
	for _, tc := range parkCases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			provider := &fakeProvider{errs: []error{tc.err}}
			err := newService(store, provider).RunOne(context.Background(), testState("cursor"))
			if !errors.Is(err, ErrProviderFailure) || len(store.parks) != 1 || store.parks[0] != string(tc.class) || len(store.fails) != 0 {
				t.Fatalf("error=%v parks=%#v fails=%#v", err, store.parks, store.fails)
			}
		})
	}
	for _, class := range []calendar.EventSyncErrorClass{calendar.EventSyncTransient, calendar.EventSyncRateLimited} {
		t.Run(string(class), func(t *testing.T) {
			store := &fakeStore{}
			provider := &fakeProvider{errs: []error{&calendar.EventSyncError{Class: class}}}
			err := newService(store, provider).RunOne(context.Background(), testState("cursor"))
			if !errors.Is(err, ErrProviderFailure) || len(store.fails) != 1 || store.fails[0] != string(class) || len(store.parks) != 0 {
				t.Fatalf("error=%v parks=%#v fails=%#v", err, store.parks, store.fails)
			}
		})
	}
}

func TestRunOnePreservesSafeProviderFailureDetails(t *testing.T) {
	store := &fakeStore{}
	provider := &fakeProvider{errs: []error{&calendar.EventSyncError{
		Class: calendar.EventSyncTransient, ProviderStatus: 500, ProviderReason: "backendError",
	}}}
	err := newService(store, provider).RunOne(t.Context(), testState("cursor"))
	var syncErr *calendar.EventSyncError
	if !errors.Is(err, ErrProviderFailure) || !errors.As(err, &syncErr) {
		t.Fatalf("error=%v, want provider failure with typed detail", err)
	}
	if syncErr.ProviderStatus != 500 || syncErr.ProviderReason != "backendError" || !strings.Contains(err.Error(), "provider_status=500") {
		t.Fatalf("error=%v typed=%#v", err, syncErr)
	}
}

func TestRunOneRetriesCapacityLimitsInsteadOfParking(t *testing.T) {
	t.Run("page limit", func(t *testing.T) {
		store := &fakeStore{}
		provider := &fakeProvider{pages: []calendar.EventSyncPage{{NextPageToken: "next"}}}
		service := newService(store, provider)
		service.PolicyFor = func(calendar.Provider) calendar.EventSyncPolicy {
			return calendar.EventSyncPolicy{PollInterval: time.Minute, RetryBase: time.Second, RetryMax: time.Minute, MaxPages: 1, MaxResets: 1}
		}
		err := service.RunOne(t.Context(), testState("cursor"))
		if !errors.Is(err, ErrPageLimit) || len(store.fails) != 1 || store.fails[0] != string(calendar.EventSyncTransient) || len(store.parks) != 0 {
			t.Fatalf("error=%v fails=%#v parks=%#v", err, store.fails, store.parks)
		}
	})
	t.Run("reset limit", func(t *testing.T) {
		store := &fakeStore{}
		provider := &fakeProvider{pages: []calendar.EventSyncPage{{ResetRequired: true}, {ResetRequired: true}}}
		service := newService(store, provider)
		service.PolicyFor = func(calendar.Provider) calendar.EventSyncPolicy {
			return calendar.EventSyncPolicy{PollInterval: time.Minute, RetryBase: time.Second, RetryMax: time.Minute, MaxPages: 3, MaxResets: 1}
		}
		err := service.RunOne(t.Context(), testState("cursor"))
		if !errors.Is(err, ErrResetLimit) || len(store.fails) != 1 || store.fails[0] != string(calendar.EventSyncTransient) || len(store.parks) != 0 {
			t.Fatalf("error=%v fails=%#v parks=%#v", err, store.fails, store.parks)
		}
	})
}

func TestRunOneCancellationDoesNotFail(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &fakeStore{}
	err := newService(store, &fakeProvider{}).RunOne(ctx, testState("cursor"))
	if !errors.Is(err, context.Canceled) || len(store.fails) != 0 {
		t.Fatalf("error=%v failures=%#v", err, store.fails)
	}
}

func TestClassifyProviderFailures(t *testing.T) {
	cases := []struct {
		err  error
		want calendar.EventSyncErrorClass
	}{
		{&calendar.EventSyncError{Class: calendar.EventSyncAuth}, calendar.EventSyncAuth},
		{&calendar.EventSyncError{Class: calendar.EventSyncPermission}, calendar.EventSyncPermission},
		{calendar.NewAPIError(calendar.ErrorUnsupportedCapability, "not available"), calendar.EventSyncUnsupported},
		{calendar.NewAPIError(calendar.ErrorInvalidArgument, "bad request"), calendar.EventSyncProtocol},
		{calendar.NewAPIError(calendar.ErrorProviderUnavailable, "offline"), calendar.EventSyncTransient},
	}
	for _, tc := range cases {
		got, _ := classify(tc.err)
		if got != tc.want {
			t.Fatalf("classify(%T) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
