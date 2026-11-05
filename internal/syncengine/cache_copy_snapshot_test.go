package syncengine

import (
	"encoding/json"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
)

func TestCacheCopyCandidatesUsesHalfOpenWindowAndRejectsDuplicates(t *testing.T) {
	windowStart := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	windowEnd := windowStart.Add(24 * time.Hour)
	event := calendar.EventV2{CalendarID: "source", SourceAttendeesComplete: true, ID: "one", Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}
	out := calendar.EventV2{CalendarID: "source", SourceAttendeesComplete: true, ID: "outside", Start: calendar.EventTime{Date: "2026-11-06"}, End: calendar.EventTime{Date: "2026-11-07"}}
	items, err := cacheCopyCandidates("source", []calendar.EventV2{event, out}, windowStart, windowEnd, time.UTC)
	if err != nil || len(items) != 1 || items[0].Event.ID != "one" {
		t.Fatalf("candidates: %d %v", len(items), err)
	}
	if _, err := cacheCopyCandidates("source", []calendar.EventV2{event, event}, windowStart, windowEnd, time.UTC); err == nil {
		t.Fatal("duplicate identity was accepted")
	}
}

func TestCacheCopyCandidatesDoesNotInferCancellation(t *testing.T) {
	start := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	event := calendar.EventV2{CalendarID: "source", SourceAttendeesComplete: true, ID: "cancelled", Status: "cancelled", Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}
	if _, err := cacheCopyCandidates("source", []calendar.EventV2{event}, start, start.Add(24*time.Hour), time.UTC); err == nil {
		t.Fatal("cancelled event accepted without provenance")
	}
}

func TestCacheCopyCandidatesAllDayUsesSourceTimeZone(t *testing.T) {
	for _, zone := range []string{"Europe/Belgrade", "America/Los_Angeles"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)
		end := start.AddDate(0, 0, 1)
		events := []calendar.EventV2{
			{CalendarID: "source", SourceAttendeesComplete: true, ID: "before", Start: calendar.EventTime{Date: "2026-10-31"}, End: calendar.EventTime{Date: "2026-11-01"}},
			{CalendarID: "source", SourceAttendeesComplete: true, ID: "inside", Start: calendar.EventTime{Date: "2026-11-01"}, End: calendar.EventTime{Date: "2026-11-02"}},
			{CalendarID: "source", SourceAttendeesComplete: true, ID: "after", Start: calendar.EventTime{Date: "2026-11-02"}, End: calendar.EventTime{Date: "2026-11-03"}},
		}
		items, err := cacheCopyCandidates("source", events, start, end, loc)
		if err != nil || len(items) != 1 || items[0].Event.ID != "inside" {
			t.Fatalf("%s: got %d candidates, err %v", zone, len(items), err)
		}
		events[1].CalendarID = "other"
		if _, err := cacheCopyCandidates("source", events, start, end, loc); err == nil {
			t.Fatalf("%s: accepted wrong calendar", zone)
		}
	}
}

func TestCacheCopyCandidatesRejectsSkippedLocalDate(t *testing.T) {
	loc, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		t.Fatal(err)
	}
	event := calendar.EventV2{CalendarID: "source", SourceAttendeesComplete: true, ID: "skipped", Start: calendar.EventTime{Date: "2011-12-30"}, End: calendar.EventTime{Date: "2011-12-31"}}
	start := time.Date(2011, 12, 29, 0, 0, 0, 0, loc)
	if _, err := cacheCopyCandidates("source", []calendar.EventV2{event}, start, start.Add(72*time.Hour), loc); err == nil {
		t.Fatal("accepted skipped local day")
	}
}

func TestCacheCopyAcceptsUnprovenSourceCompletenessAfterJSON(t *testing.T) {
	start := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	original := calendar.EventV2{CalendarID: "source", ID: "event", SourceAttendeesComplete: true, Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var cached calendar.EventV2
	if err = json.Unmarshal(raw, &cached); err != nil {
		t.Fatal(err)
	}
	if cached.SourceAttendeesComplete {
		t.Fatal("ordinary JSON must not assert source completeness")
	}
	items, err := cacheCopyCandidates("source", []calendar.EventV2{cached}, start, start.Add(24*time.Hour), time.UTC)
	if err != nil || len(items) != 1 {
		t.Fatalf("cached event with unknown participants was blocked: %v", err)
	}
}
