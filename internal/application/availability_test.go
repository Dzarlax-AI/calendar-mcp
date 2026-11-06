package application

import (
	"context"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

type availabilityStub struct {
	*stubV2Provider
	route string
}

func (p *availabilityStub) RouteName() string        { return p.route }
func (p *availabilityStub) OwnsCalendar(string) bool { return true }

func TestAvailabilityDrainsPagesAndIncludesNearbyAllDay(t *testing.T) {
	start := time.Date(2026, 11, 5, 20, 0, 0, 0, time.UTC)
	end := start.Add(8 * time.Hour)
	for _, provider := range []string{"google", "microsoft", "apple"} {
		t.Run(provider, func(t *testing.T) {
			raw := &stubV2Provider{name: provider}
			p := &availabilityStub{raw, provider + "@acct"}
			calls := 0
			raw.pageFunc = func(req calendar.ListEventsRequestV2) calendar.Page[calendar.EventV2] {
				calls++
				if !req.Start.Equal(start.Add(-24*time.Hour)) || !req.End.Equal(end.Add(24*time.Hour)) || req.View != calendar.RecurrenceExpanded || !req.AvailabilityStrict {
					t.Fatal("incorrect strict provider window")
				}
				if req.PageToken == "" {
					return calendar.Page[calendar.EventV2]{Complete: true, NextPageToken: "next", Items: []calendar.EventV2{{ID: "all-day", Start: calendar.EventTime{Date: "2026-11-06"}, End: calendar.EventTime{Date: "2026-11-07"}}}}
				}
				return calendar.Page[calendar.EventV2]{Complete: true, Items: []calendar.EventV2{{ID: "timed", Start: calendar.EventTime{DateTime: "2026-11-05T21:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-11-05T22:00:00Z", TimeZone: "UTC"}}}}
			}
			s := New(calendar.NewRegistry([]calendar.Provider{p}))
			page, err := s.ListAvailabilityEvents(context.Background(), provider+"@acct:physical", start, end)
			if err != nil || !page.Complete || len(page.Items) != 2 || page.NextPageToken != "" || len(page.Sources) != 1 || calls != 2 {
				t.Fatalf("page=%+v calls=%d err=%v", page, calls, err)
			}
			if page.Items[0].Start.Date != "2026-11-06" {
				t.Fatal("all-day source date changed")
			}
		})
	}
}
func TestAvailabilityFailsClosed(t *testing.T) {
	start := time.Date(2026, 11, 5, 20, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	for _, tc := range []struct {
		name string
		page calendar.Page[calendar.EventV2]
	}{
		{"incomplete", calendar.Page[calendar.EventV2]{Complete: false}},
		{"invalid event", calendar.Page[calendar.EventV2]{Complete: true, Items: []calendar.EventV2{{ID: "bad"}}}},
		{"out-of-window master", calendar.Page[calendar.EventV2]{Complete: true, Items: []calendar.EventV2{{ID: "master", InstanceKind: "seriesMaster", Start: calendar.EventTime{DateTime: "2026-10-01T00:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-10-01T01:00:00Z", TimeZone: "UTC"}}}}},
		{"repeated cursor", calendar.Page[calendar.EventV2]{Complete: true, NextPageToken: "again"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &availabilityStub{&stubV2Provider{name: "google", page: tc.page}, "google@acct"}
			s := New(calendar.NewRegistry([]calendar.Provider{p}))
			if _, err := s.ListAvailabilityEvents(context.Background(), "google@acct:physical", start, end); err == nil {
				t.Fatal("unsafe page accepted")
			}
		})
	}
	p := &availabilityStub{&stubV2Provider{name: "google", page: calendar.Page[calendar.EventV2]{Complete: true}}, "google@acct"}
	s0 := New(calendar.NewRegistry([]calendar.Provider{p}))
	if _, err := s0.ListAvailabilityEvents(context.Background(), "google:acct:physical", start, end); err != nil {
		t.Fatalf("canonical account id rejected: %v", err)
	}
	s := New(calendar.NewRegistry([]calendar.Provider{p}))
	for _, id := range []string{"", "google:primary", "google@acct:primary", "google@acct:"} {
		if _, err := s.ListAvailabilityEvents(context.Background(), id, start, end); err == nil {
			t.Fatalf("ambiguous id accepted: %q", id)
		}
	}
}

func TestAvailabilityExcludesTimedPointEventsAndRejectsInvalidRanges(t *testing.T) {
	start := time.Date(2026, 11, 5, 20, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	point := calendar.EventTime{DateTime: "2026-11-05T20:30:00Z", TimeZone: "UTC"}
	for _, tc := range []struct {
		name    string
		event   calendar.EventV2
		wantErr bool
	}{
		{"timed point", calendar.EventV2{ID: "point", Start: point, End: point}, false},
		{"all-day zero", calendar.EventV2{ID: "all-day", Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-05"}}, true},
		{"reversed", calendar.EventV2{ID: "bad", Start: point, End: calendar.EventTime{DateTime: "2026-11-05T20:00:00Z", TimeZone: "UTC"}}, true},
		{"missing end", calendar.EventV2{ID: "bad", Start: point}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &availabilityStub{&stubV2Provider{name: "google", page: calendar.Page[calendar.EventV2]{Complete: true, Items: []calendar.EventV2{tc.event}}}, "google@acct"}
			s := New(calendar.NewRegistry([]calendar.Provider{p}))
			page, err := s.ListAvailabilityEvents(context.Background(), "google:acct:physical", start, end)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			if !tc.wantErr && (!page.Complete || len(page.Items) != 0) {
				t.Fatalf("point affected busy: %+v", page)
			}
		})
	}
}
