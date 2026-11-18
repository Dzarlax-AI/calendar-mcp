package cachedpages

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
)

type serviceFixture struct {
	store *storage.Store
	now   time.Time
	ctx   context.Context
	page  Request
	svc   *Service
}

func newServiceFixture(t *testing.T, key, legacy string, eventCount int) serviceFixture {
	t.Helper()
	now := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	store, err := storage.Open(context.Background(), "sqlite://"+filepath.Join(t.TempDir(), "pages.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateConnection(context.Background(), storage.Connection{ID: "conn", Provider: "google", DisplayName: "Synthetic", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: `[]`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	calendarID := "google:conn:primary"
	if err := store.UpsertCalendar(context.Background(), storage.Calendar{ID: calendarID, ConnectionID: "conn", ProviderCalendarID: "primary", Name: "Synthetic", CanRead: true, DiscoveredAt: now}); err != nil {
		t.Fatal(err)
	}
	window := storage.SyncWindow{Start: now.AddDate(-1, 0, 0), End: now.AddDate(2, 0, 0)}
	if err := store.EnsureCalendarSyncStates(context.Background(), now, window); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.BeginAuthorization(context.Background(), "conn", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteGoogleAuthorization(context.Background(), snapshot, []storage.DiscoveredDetailCalendar{{Calendar: storage.Calendar{ID: calendarID, ConnectionID: "conn", ProviderCalendarID: "primary", Name: "Synthetic"}, AccessRole: "reader"}}, now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < eventCount; i++ {
		id := fmt.Sprintf("event-%02d", i)
		start := now.Add(time.Hour + time.Duration(i)*time.Minute)
		event := calendar.EventV2{ID: id, CalendarID: calendarID, Provider: "google", Title: id, Description: "sensitive description", Start: calendar.EventTime{DateTime: start.Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: start.Add(30 * time.Minute).Format(time.RFC3339), TimeZone: "UTC"}}
		if err := store.UpsertCachedEvent(context.Background(), event, now); err != nil {
			t.Fatal(err)
		}
	}
	keyBytes := bytes.Repeat([]byte{0x37}, 32)
	cipher, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(keyBytes))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(store, cipher, Options{Window: window, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := verifiedContext(t, key, legacy)
	return serviceFixture{
		store: store, now: now, ctx: ctx, svc: svc,
		page: Request{CalendarIDs: []string{calendarID}, Start: now, End: now.Add(24 * time.Hour), PageSize: 1},
	}
}

func verifiedContext(t *testing.T, key, legacy string) context.Context {
	t.Helper()
	var got context.Context
	handler := auth.Middleware(auth.Options{APIKey: key, LegacyAPIKey: legacy})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Context() }))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if got == nil {
		t.Fatal("auth middleware did not pass request context")
	}
	return got
}

func TestServiceReadPaginatesWithVerifiedCaller(t *testing.T) {
	f := newServiceFixture(t, "synthetic-key", "", 3)
	page, stats, err := f.svc.Read(f.ctx, f.page, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].EventID != "event-00" || page.NextPageToken == "" || page.Complete || page.Consistency != "cached_weak" {
		t.Fatalf("unexpected first page: %#v", page)
	}
	if stats.Read.RowsDecoded != 2 || stats.AuthorityNS <= 0 || stats.ReadNS <= 0 || stats.PackNS <= 0 || stats.FinalValidationNS <= 0 {
		t.Fatalf("missing page phase measurements: %+v", stats)
	}
	page, _, err = f.svc.Read(f.ctx, f.page, page.NextPageToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].EventID != "event-01" || page.NextPageToken == "" || page.Complete {
		t.Fatalf("unexpected second page: %#v", page)
	}
	page, _, err = f.svc.Read(f.ctx, f.page, page.NextPageToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].EventID != "event-02" || page.NextPageToken != "" || !page.Complete {
		t.Fatalf("unexpected terminal page: %#v", page)
	}
}

func TestServiceRequiresVerifiedCallerAndValidRequest(t *testing.T) {
	f := newServiceFixture(t, "synthetic-key", "", 1)
	if _, _, err := f.svc.Read(context.Background(), f.page, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing verified caller error=%v", err)
	}
	for _, request := range []Request{
		{CalendarIDs: []string{"google:conn:primary"}, Start: f.now, End: f.now, PageSize: 1},
		{CalendarIDs: []string{}, Start: f.now, End: f.now.Add(time.Hour), PageSize: 1},
		{CalendarIDs: []string{"google:conn:primary", "google:conn:primary"}, Start: f.now, End: f.now.Add(time.Hour), PageSize: 1},
		{CalendarIDs: []string{"google:conn:primary"}, Start: f.now, End: f.now.Add(time.Hour), PageSize: 51},
	} {
		if _, _, err := f.svc.Read(f.ctx, request, ""); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("invalid request accepted or misclassified: request=%#v error=%v", request, err)
		}
	}
	unsupported := Request{CalendarIDs: []string{"microsoft:conn:primary"}, Start: f.now, End: f.now.Add(time.Hour), PageSize: 1}
	if _, _, err := f.svc.Read(f.ctx, unsupported, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unsupported source should fail scope as unavailable: %v", err)
	}
	tooMany := f.page
	tooMany.CalendarIDs = make([]string, 17)
	for i := range tooMany.CalendarIDs {
		tooMany.CalendarIDs[i] = "google:conn:primary"
	}
	if _, _, err := f.svc.Read(f.ctx, tooMany, ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("17-input scope collapsing to one ID should be invalid: %v", err)
	}
	unknown := Request{CalendarIDs: []string{"google:unknown:calendar"}, Start: f.now, End: f.now.Add(time.Hour), PageSize: 1}
	if _, _, err := f.svc.Read(f.ctx, unknown, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unknown source should fail whole scope: %v", err)
	}
	request := f.page
	request.PageSize = 0
	page, _, err := f.svc.Read(f.ctx, request, "")
	if err != nil || len(page.Items) != 1 || !page.Complete {
		t.Fatalf("zero page size should default to 25: page=%#v error=%v", page, err)
	}
}

func TestServiceCursorIsBoundToCallerConfigAuthorityExpiryAndBoot(t *testing.T) {
	f := newServiceFixture(t, "synthetic-key", "legacy-one", 3)
	first, _, err := f.svc.Read(f.ctx, f.page, "")
	if err != nil {
		t.Fatal(err)
	}
	token := first.NextPageToken
	for name, ctx := range map[string]context.Context{
		"different caller":       verifiedContext(t, "other-key", "legacy-one"),
		"configuration rotation": verifiedContext(t, "synthetic-key", "legacy-two"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := f.svc.Read(ctx, f.page, token); !errors.Is(err, ErrCursorUnavailable) {
				t.Fatalf("cursor accepted under changed caller/config: %v", err)
			}
		})
	}
	if _, _, err := f.svc.Read(f.ctx, f.page, token[:len(token)-1]+"!"); !errors.Is(err, ErrCursorUnavailable) {
		t.Fatalf("tampered token error=%v", err)
	}
	changed := f.svc
	originalNonce := processBootNonce
	defer func() { processBootNonce = originalNonce }()
	if _, err := rand.Read(processBootNonce[:]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := changed.Read(f.ctx, f.page, token); !errors.Is(err, ErrCursorUnavailable) {
		t.Fatalf("cursor accepted after boot nonce change: %v", err)
	}
	if _, err := f.store.BeginAuthorization(context.Background(), "conn", nil, f.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Read(f.ctx, f.page, token); !errors.Is(err, ErrCursorUnavailable) {
		t.Fatalf("cursor accepted after authorization receipt/revision changed: %v", err)
	}
}

func TestNormalizeRequestAllowsYearOneMidnight(t *testing.T) {
	request := Request{CalendarIDs: []string{"google:conn:primary"}, Start: time.Time{}, End: time.Date(1, 1, 2, 0, 0, 0, 0, time.UTC)}
	if _, err := normalizeRequest(request); err != nil {
		t.Fatalf("year-one midnight rejected: %v", err)
	}
}

func TestTicketComparisonAllowsOrdinarySyncGenerationChanges(t *testing.T) {
	a := []storage.AuthorityTicket{{ConnectionID: "conn", CalendarID: "google:conn:primary", AuthorizationRevision: "revision", DetailReceipt: "reader:receipt", Generation: 4}}
	b := []storage.AuthorityTicket{{ConnectionID: "conn", CalendarID: "google:conn:primary", AuthorizationRevision: "revision", DetailReceipt: "reader:receipt", Generation: 5}}
	if !sameTickets(a, b) {
		t.Fatal("ordinary sync generation change invalidated an otherwise identical authority ticket")
	}
	b[0].DetailReceipt = "reader:new-receipt"
	if sameTickets(a, b) {
		t.Fatal("changed detail receipt was accepted")
	}
}

func TestServiceCarriesAbsoluteExpiryAcrossContinuations(t *testing.T) {
	f := newServiceFixture(t, "synthetic-key", "", 3)
	page, _, err := f.svc.Read(f.ctx, f.page, "")
	if err != nil {
		t.Fatal(err)
	}
	position, expiry, err := f.svc.codec.Decode(page.NextPageToken, pageBinding(mustNormalize(t, f.page), mustTickets(t, f.store, f.page), mustCaller(t, f.ctx), f.svc.window))
	if err != nil || !expiry.Equal(f.now.Add(cursorLifetime)) {
		t.Fatalf("initial expiry mismatch: expiry=%s error=%v", expiry, err)
	}
	_ = position
	page, _, err = f.svc.Read(f.ctx, f.page, page.NextPageToken)
	if err != nil {
		t.Fatal(err)
	}
	position, nextExpiry, err := f.svc.codec.Decode(page.NextPageToken, pageBinding(mustNormalize(t, f.page), mustTickets(t, f.store, f.page), mustCaller(t, f.ctx), f.svc.window))
	if err != nil || !nextExpiry.Equal(expiry) {
		t.Fatalf("continuation extended absolute expiry: expiry=%s next=%s error=%v", expiry, nextExpiry, err)
	}
	_ = position
	f.svc.now = func() time.Time { return expiry }
	f.svc.codec.now = f.svc.now
	if _, _, err := f.svc.Read(f.ctx, f.page, page.NextPageToken); !errors.Is(err, ErrCursorUnavailable) {
		t.Fatalf("expired cursor error=%v", err)
	}
}

func TestServiceFinalAuthorityValidationSuppressesPackedPage(t *testing.T) {
	f := newServiceFixture(t, "synthetic-key", "", 1)
	f.svc.testBeforeFinalValidation = func() {
		if _, err := f.store.BeginAuthorization(context.Background(), "conn", nil, f.now.Add(time.Second)); err != nil {
			t.Errorf("revoke authorization before final validation: %v", err)
		}
	}
	page, _, err := f.svc.Read(f.ctx, f.page, "")
	if !errors.Is(err, ErrUnavailable) || len(page.Items) != 0 {
		t.Fatalf("revoked source page escaped final validation: page=%#v error=%v", page, err)
	}
}

func mustNormalize(t *testing.T, request Request) Request {
	t.Helper()
	normalized, err := normalizeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return normalized
}

func mustTickets(t *testing.T, store *storage.Store, request Request) []storage.AuthorityTicket {
	t.Helper()
	normalized := mustNormalize(t, request)
	tickets, err := store.CaptureCachedPageAuthority(context.Background(), normalized)
	if err != nil {
		t.Fatal(err)
	}
	return tickets
}

func mustCaller(t *testing.T, ctx context.Context) auth.VerifiedCaller {
	t.Helper()
	caller, ok := auth.VerifiedCallerFromContext(ctx)
	if !ok {
		t.Fatal("missing verified caller")
	}
	return caller
}

func TestServiceRollingWindowAndCursorRollover(t *testing.T) {
	f := newServiceFixture(t, "synthetic-key", "", 3)
	now := time.Date(2026, 8, 22, 23, 59, 0, 0, time.UTC)
	initial := f.svc.window
	calls := 0
	windowAt := func(at time.Time) storage.SyncWindow {
		calls++
		d := time.Date(at.UTC().Year(), at.UTC().Month(), at.UTC().Day(), 0, 0, 0, 0, time.UTC)
		base := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
		shift := int(d.Sub(base).Hours() / 24)
		return storage.SyncWindow{Start: initial.Start.AddDate(0, 0, shift), End: initial.End.AddDate(0, 0, shift)}
	}
	svc, err := NewService(f.store, f.svc.cipher, Options{Now: func() time.Time { return now }, WindowAt: windowAt})
	if err != nil || !svc.Ready() {
		t.Fatalf("construct: %v", err)
	}
	calls = 0
	page, _, err := svc.Read(f.ctx, f.page, "")
	if err != nil || page.NextPageToken == "" || calls != 1 {
		t.Fatalf("first page: calls=%d err=%v", calls, err)
	}
	now = now.Add(30 * time.Second)
	if _, _, err := svc.Read(f.ctx, f.page, page.NextPageToken); err != nil {
		t.Fatalf("same day continuation: %v", err)
	}
	now = now.Add(31 * time.Second)
	if _, _, err := svc.Read(f.ctx, f.page, page.NextPageToken); !errors.Is(err, ErrCursorUnavailable) {
		t.Fatalf("rollover cursor: %v", err)
	}
	oldRange := f.page
	oldRange.Start, oldRange.End = initial.Start, initial.Start.Add(time.Hour)
	if _, _, err := svc.Read(f.ctx, oldRange, page.NextPageToken); !errors.Is(err, ErrCursorUnavailable) {
		t.Fatalf("exited range cursor: %v", err)
	}
	if _, _, err := svc.Read(f.ctx, oldRange, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("exited range: %v", err)
	}
	newRange := f.page
	newRange.Start, newRange.End = initial.End, initial.End.Add(time.Hour)
	if _, _, err := svc.Read(f.ctx, newRange, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("source not rolled: %v", err)
	}
	rolled := windowAt(now)
	if err := f.store.EnsureCalendarSyncStates(f.ctx, now, rolled); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Read(f.ctx, newRange, ""); err != nil {
		t.Fatalf("rolled source: %v", err)
	}
}
