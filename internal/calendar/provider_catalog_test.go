package calendar

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type catalogSelection struct {
	routes         map[string][]string
	enabled        map[string]bool
	err            error
	calls          int
	discoveryCalls int
	changeAt       int
}

func (s *catalogSelection) CalendarEnabled(_ context.Context, id string) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.enabled[id], nil
}
func (s *catalogSelection) ConfiguredProviderRoutes(_ context.Context, p string) ([]string, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if s.changeAt > 0 && s.calls >= s.changeAt {
		return []string{p + "@changed"}, nil
	}
	return append([]string(nil), s.routes[p]...), nil
}
func (s *catalogSelection) EnabledProviderCalendars(_ context.Context, provider string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	ids := []string{}
	for id, enabled := range s.enabled {
		if enabled && strings.HasPrefix(id, provider+":") {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
func (s *catalogSelection) AvailabilityCalendarSelection(_ context.Context, id string) (bool, bool, error) {
	if s.err != nil {
		return false, false, s.err
	}
	enabled, known := s.enabled[id]
	return known, enabled, nil
}
func (s *catalogSelection) RegisterDiscoveredCalendar(context.Context, string, Calendar) error {
	s.discoveryCalls++
	return nil
}

type catalogProvider struct {
	*routedFakeProvider
	calls int
}

func (p *catalogProvider) ListCalendars(ctx context.Context) ([]Calendar, error) {
	p.calls++
	return p.fakeProvider.ListCalendars(ctx)
}
func testCatalogProvider(name, account string, cals []Calendar, err error) *catalogProvider {
	return &catalogProvider{routedFakeProvider: &routedFakeProvider{fakeProvider: fakeProvider{name: name, calendars: cals, calendarsErr: err}, route: name + "@" + account}}
}

func TestAvailabilityCatalogIsolatesProviderAndKeepsEnabledCanonicalIDs(t *testing.T) {
	google := testCatalogProvider("google", "account", []Calendar{{ID: "visible", Name: "Visible"}, {ID: "hidden", Name: "Hidden"}}, nil)
	apple := testCatalogProvider("apple", "old", nil, errors.New("synthetic 401"))
	selection := &catalogSelection{routes: map[string][]string{"google": {"google@account"}, "apple": {"apple@old"}}, enabled: map[string]bool{"google:account:visible": true, "google:account:hidden": false}}
	r := NewRegistry([]Provider{google, apple}, RegistryOptions{Selection: selection})
	page, err := r.ListAvailabilityCalendars(context.Background(), "google")
	if err != nil || !page.Complete || page.Provider != "google" || len(page.Calendars) != 1 || page.Calendars[0].ID != "google:account:visible" || page.Calendars[0].Provider != "google" {
		t.Fatalf("catalog error=%v", err)
	}
	if apple.calls != 0 || google.calls != 1 || selection.discoveryCalls != 0 {
		t.Fatal("unrelated provider or discovery write called")
	}
	if _, err = r.ListAvailabilityCalendars(context.Background(), "apple"); err == nil {
		t.Fatal("selected failed provider was accepted")
	}
}
func TestAvailabilityCatalogNoPartialAndAuthoritativeAbsence(t *testing.T) {
	one := testCatalogProvider("google", "one", []Calendar{{ID: "a"}}, nil)
	two := testCatalogProvider("google", "two", nil, errors.New("synthetic 401"))
	selection := &catalogSelection{routes: map[string][]string{"google": {"google@one", "google@two"}}, enabled: map[string]bool{"google:one:a": true}}
	r := NewRegistry([]Provider{one, two}, RegistryOptions{Selection: selection})
	if page, err := r.ListAvailabilityCalendars(context.Background(), "google"); err == nil || len(page.Calendars) != 0 {
		t.Fatal("partial result leaked")
	}
	empty, err := r.ListAvailabilityCalendars(context.Background(), "microsoft")
	if err != nil || !empty.Complete || empty.Calendars == nil || len(empty.Calendars) != 0 {
		t.Fatal("authoritative absent provider not empty")
	}
	selection.routes["apple"] = []string{"apple@unbuilt"}
	if _, err = r.ListAvailabilityCalendars(context.Background(), "apple"); err == nil {
		t.Fatal("configured but missing provider accepted")
	}
}
func TestAvailabilityCatalogRejectsInventoryChangeAndUnknownSelection(t *testing.T) {
	p := testCatalogProvider("google", "one", nil, nil)
	s := &catalogSelection{routes: map[string][]string{"google": {"google@one"}}, changeAt: 2}
	r := NewRegistry([]Provider{p}, RegistryOptions{Selection: s})
	if _, err := r.ListAvailabilityCalendars(context.Background(), "google"); err == nil {
		t.Fatal("changed inventory accepted")
	}
	if _, err := NewRegistry(nil).ListAvailabilityCalendars(context.Background(), "google"); err == nil {
		t.Fatal("missing authoritative inventory accepted")
	}
}

func TestAvailabilityCatalogRejectsMissingEnabledCalendar(t *testing.T) {
	p := testCatalogProvider("google", "account", []Calendar{{ID: "visible"}}, nil)
	s := &catalogSelection{routes: map[string][]string{"google": {"google@account"}}, enabled: map[string]bool{"google:account:visible": true, "google:account:missing": true}}
	r := NewRegistry([]Provider{p}, RegistryOptions{Selection: s})
	page, err := r.ListAvailabilityCalendars(context.Background(), "google")
	if err == nil || len(page.Calendars) != 0 {
		t.Fatal("missing enabled calendar accepted as complete")
	}
}

func TestAvailabilityCatalogGoogleRequiresPositiveWriteEvidence(t *testing.T) {
	p := testCatalogProvider("google", "account", []Calendar{{ID: "proven", GoogleWriteAccessProven: true}, {ID: "unknown"}, {ID: "reader", ReadOnly: true}}, nil)
	s := &catalogSelection{routes: map[string][]string{"google": {"google@account"}}, enabled: map[string]bool{"google:account:proven": true, "google:account:unknown": true, "google:account:reader": true}}
	result, err := NewRegistry([]Provider{p}, RegistryOptions{Selection: s}).ListAvailabilityCalendars(context.Background(), "google")
	if err != nil || len(result.Calendars) != 3 {
		t.Fatal("catalog failed")
	}
	if result.Calendars[0].ReadOnly || !result.Calendars[1].ReadOnly || !result.Calendars[2].ReadOnly {
		t.Fatal("unproven calendar treated as writable")
	}
}

func TestAvailabilityCatalogDistinguishesDisabledFromUnregistered(t *testing.T) {
	p := testCatalogProvider("google", "account", []Calendar{{ID: "known-disabled"}, {ID: "new"}}, nil)
	s := &catalogSelection{routes: map[string][]string{"google": {"google@account"}}, enabled: map[string]bool{"google:account:known-disabled": false}}
	r := NewRegistry([]Provider{p}, RegistryOptions{Selection: s})
	if _, err := r.ListAvailabilityCalendars(context.Background(), "google"); err == nil {
		t.Fatal("unregistered calendar silently omitted")
	}
	if s.discoveryCalls != 0 {
		t.Fatal("discovery write attempted")
	}
	s.enabled["google:account:new"] = false
	page, err := r.ListAvailabilityCalendars(context.Background(), "google")
	if err != nil || !page.Complete || page.Calendars == nil || len(page.Calendars) != 0 {
		t.Fatal("registered disabled calendar not safely hidden")
	}
}
