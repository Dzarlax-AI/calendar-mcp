package syncengine

import (
	"testing"

	"calendar-mcp/internal/calendar"
)

func TestCacheCopyIdentityUsesOriginalInstantAndDistinctEvents(t *testing.T) {
	first := calendar.EventTime{DateTime: "2026-10-25T09:00:00+01:00", TimeZone: "Europe/Belgrade"}
	otherZone := calendar.EventTime{DateTime: "2026-10-25T08:00:00Z", TimeZone: "UTC"}
	event := calendar.EventV2{CalendarID: "source", ID: "instance", RecurringEventID: "series", OriginalStart: &first}
	a, err := cacheCopyIdentity("source", event)
	if err != nil {
		t.Fatal(err)
	}
	otherEvent := event
	otherEvent.CalendarID = "other-source"
	otherCalendar, err := cacheCopyIdentity("other-source", otherEvent)
	if err != nil || otherCalendar == a {
		t.Fatal("different source calendars share identity")
	}
	event.OriginalStart = &otherZone
	b, err := cacheCopyIdentity("source", event)
	if err != nil || a != b {
		t.Fatalf("same instant identities differ: %v", err)
	}
	event.ID = "other"
	c, err := cacheCopyIdentity("source", event)
	if err != nil || c != a {
		t.Fatalf("instance ID change altered identity: %v", err)
	}
	later := calendar.EventTime{DateTime: "2026-10-26T08:00:00Z", TimeZone: "UTC"}
	event.OriginalStart = &later
	c, err = cacheCopyIdentity("source", event)
	if err != nil || c == a {
		t.Fatalf("distinct occurrences share identity: %v", err)
	}
	event.OriginalStart = nil
	if _, err := cacheCopyIdentity("source", event); err == nil {
		t.Fatal("missing original start was accepted")
	}
}

func TestCacheCopyCreateProducesStableStandaloneCopies(t *testing.T) {
	day := calendar.EventTime{Date: "2026-11-05"}
	event := calendar.EventV2{CalendarID: "source", SourceAttendeesComplete: true, ID: "instance", ICalUID: "shared", RecurringEventID: "series", OriginalStart: &day, Start: day, End: calendar.EventTime{Date: "2026-11-06"}, Recurrence: []string{"RRULE:FREQ=DAILY"}, Attendees: []calendar.AttendeeV2{{PersonV2: calendar.PersonV2{Email: "synthetic@example.invalid"}}}}
	first, err := cacheCopyCreate("rule", "source", event, "google")
	if err != nil {
		t.Fatal(err)
	}
	if first.ICalUID == "shared" || len(first.Recurrence) != 0 || len(first.Attendees) != 0 || first.SyncMarker == nil || first.Google.PrivateProperties["calendar_source_event"] != first.SyncMarker.SourceEventID {
		t.Fatal("standalone identity and marker not preserved")
	}
	if cacheCopyScope() != calendar.ScopeSeries {
		t.Fatal("standalone target must use top-level scope")
	}
	repeated, err := cacheCopyCreate("rule", "source", event, "google")
	if err != nil || repeated.ICalUID != first.ICalUID {
		t.Fatal("identity is not stable")
	}
	event.ID = "another"
	next := calendar.EventTime{Date: "2026-11-06"}
	event.OriginalStart = &next
	second, err := cacheCopyCreate("rule", "source", event, "google")
	if err != nil || second.ICalUID == first.ICalUID || second.SyncMarker.SourceEventID == first.SyncMarker.SourceEventID {
		t.Fatal("instances share ownership identity")
	}
	event.Status = "cancelled"
	if _, err := cacheCopyCreate("rule", "source", event, "google"); err == nil {
		t.Fatal("cancelled event can be created")
	}
}

func TestCacheCopyCreatesEventWithIncompleteSourceAttendeesWithoutInvites(t *testing.T) {
	day := calendar.EventTime{Date: "2026-11-05"}
	event := calendar.EventV2{
		CalendarID: "source", ID: "incomplete", Title: "Planning",
		Start: day, End: calendar.EventTime{Date: "2026-11-06"},
		SourceAttendeesComplete: false,
		Attendees:               []calendar.AttendeeV2{{PersonV2: calendar.PersonV2{Email: "synthetic@example.invalid"}}},
	}
	created, err := cacheCopyCreate("rule", "source", event, "google")
	if err != nil {
		t.Fatalf("incomplete attendees must not block creation: %v", err)
	}
	if created.Title != "Planning" || len(created.Attendees) != 0 || len(created.Recurrence) != 0 {
		t.Fatal("copy lost event information or created invitees")
	}
}
