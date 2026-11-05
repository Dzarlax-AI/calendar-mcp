package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
)

type previewProvider struct {
	name   string
	writes int
}

func (p *previewProvider) Name() string { return p.name }
func (p *previewProvider) ListCalendars(context.Context) ([]calendar.Calendar, error) {
	return nil, nil
}
func (p *previewProvider) GetEvents(context.Context, string, time.Time, time.Time) ([]calendar.Event, error) {
	return nil, nil
}
func (p *previewProvider) CreateEvent(context.Context, string, calendar.EventCreate) (*calendar.Event, error) {
	p.writes++
	return nil, errors.New("unexpected write")
}
func (p *previewProvider) UpdateEvent(context.Context, string, string, calendar.EventUpdate) (*calendar.Event, error) {
	p.writes++
	return nil, errors.New("unexpected write")
}
func (p *previewProvider) DeleteEvent(context.Context, string, string) error {
	p.writes++
	return errors.New("unexpected write")
}
func (p *previewProvider) Capabilities(context.Context, string) (calendar.CalendarCapabilities, error) {
	return calendar.CalendarCapabilities{}, nil
}
func (p *previewProvider) ListEventsV2(context.Context, calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	return calendar.Page[calendar.EventV2]{Items: []calendar.EventV2{{ID: "event", Title: "Synthetic private event", Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}}, Complete: true}, nil
}
func (p *previewProvider) GetEventV2(context.Context, calendar.EventRef) (*calendar.EventV2, error) {
	return nil, nil
}
func (p *previewProvider) CreateEventV2(context.Context, calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	p.writes++
	return nil, errors.New("unexpected write")
}
func (p *previewProvider) UpdateEventV2(context.Context, calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	p.writes++
	return nil, errors.New("unexpected write")
}
func (p *previewProvider) DeleteEventV2(context.Context, calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	p.writes++
	return nil, errors.New("unexpected write")
}

type previewBuilder struct{ providers []calendar.Provider }

func (b previewBuilder) Build(context.Context) ([]calendar.Provider, error) { return b.providers, nil }

func TestDryRunWorkerPersistsPreviewWithoutProviderWrites(t *testing.T) {
	ctx := t.Context()
	s, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "calendar.db"))
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
	now := time.Now().UTC()
	if err = s.CreateConnection(ctx, storage.Connection{ID: "account", Provider: "google", AccountFingerprint: "account", DisplayName: "synthetic", Status: "connected", EncryptedCredentials: []byte("fake"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"google:source", "google:target"} {
		if err = s.UpsertCalendar(ctx, storage.Calendar{ID: id, ConnectionID: "account", ProviderCalendarID: id, Name: id, CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "google:target", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
	if err = s.CreateRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	if err = s.EnqueueJob(ctx, storage.Job{ID: "job", RuleID: "rule", Kind: "dry_run", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimJob(ctx, "worker", now)
	if err != nil || job == nil {
		t.Fatalf("claim %v", err)
	}
	p := &previewProvider{name: "google"}
	if err = executeJob(ctx, s, previewBuilder{providers: []calendar.Provider{p}}, *job, now); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListRuns(ctx, 10)
	if err != nil || len(runs) != 1 || runs[0].Outcome != "succeeded" || runs[0].CreatedCount != 1 {
		t.Fatalf("runs %#v %v", runs, err)
	}
	preview, state, err := s.GetSyncRunPreview(ctx, runs[0].ID, time.Now().UTC())
	if err != nil || state != "available" || preview.Total != 1 || preview.Entries[0].Title != "Synthetic private event" || p.writes != 0 {
		t.Fatalf("preview %s %v writes %d", state, err, p.writes)
	}
}
