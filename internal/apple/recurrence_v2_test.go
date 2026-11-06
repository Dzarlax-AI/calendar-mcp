package apple

import (
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"

	"calendar-mcp/internal/calendar"
)

func TestAppleEventsFromObjectExpandsSeriesExceptionsAndExdates(t *testing.T) {
	master := ical.NewEvent()
	master.Props.SetText(ical.PropUID, "series")
	master.Props.SetText(ical.PropSummary, "Daily")
	master.Props.SetDateTime(ical.PropDateTimeStart, time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC))
	master.Props.SetDateTime(ical.PropDateTimeEnd, time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC))
	rule := ical.NewProp(ical.PropRecurrenceRule)
	rule.Value = "FREQ=DAILY;COUNT=3"
	master.Props.Add(rule)
	exdate := ical.NewProp(ical.PropExceptionDates)
	exdate.SetDateTime(time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC))
	master.Props.Add(exdate)
	exception := cloneAppleEvent(master)
	for _, name := range []string{ical.PropRecurrenceRule, ical.PropExceptionDates} {
		exception.Props.Del(name)
	}
	exception.Props.SetDateTime(ical.PropRecurrenceID, time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC))
	exception.Props.SetDateTime(ical.PropDateTimeStart, time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC))
	exception.Props.SetDateTime(ical.PropDateTimeEnd, time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC))
	exception.Props.SetText(ical.PropSummary, "Moved")
	container := ical.NewCalendar()
	container.Children = append(container.Children, master.Component, exception.Component)
	items, err := appleEventsFromObject(caldav.CalendarObject{Path: "/cal/series.ics", ETag: "one", Data: container}, calendar.ListEventsRequestV2{CalendarID: "/cal", Start: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC), View: calendar.RecurrenceBoth, ShowDeleted: true})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, item := range items {
		kinds[item.InstanceKind]++
	}
	if kinds["seriesMaster"] != 1 || kinds["occurrence"] != 1 || kinds["exception"] != 1 || kinds["cancelled"] != 1 {
		t.Fatalf("instance kinds = %#v, items = %#v", kinds, items)
	}
}

func TestAppleExpandedMalformedObjectsAreIncomplete(t *testing.T) {
	start := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	master := ical.NewEvent()
	master.Props.SetText(ical.PropUID, "synthetic")
	master.Props.SetDateTime(ical.PropDateTimeStart, start)
	master.Props.SetDateTime(ical.PropDateTimeEnd, start.Add(time.Hour))
	rule := ical.NewProp(ical.PropRecurrenceRule)
	rule.Value = "FREQ=DAILY;COUNT=3"
	master.Props.Add(rule)
	exception := ical.NewEvent()
	exception.Props.SetText(ical.PropUID, "synthetic")
	exception.Props.SetDateTime(ical.PropRecurrenceID, start.Add(24*time.Hour))
	exception.Props.SetDateTime(ical.PropDateTimeStart, start.Add(26*time.Hour))
	invalidEnd := ical.NewProp(ical.PropDateTimeEnd)
	invalidEnd.Value = "not-a-date"
	exception.Props.Add(invalidEnd)
	// A malformed explicit end must not make a moved exception disappear silently.
	for _, tc := range []struct {
		name     string
		children []*ical.Component
	}{
		{"empty", nil},
		{"orphan", []*ical.Component{exception.Component}},
		{"malformed exception", []*ical.Component{master.Component, exception.Component}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := ical.NewCalendar()
			object.Children = append(object.Children, tc.children...)
			req := calendar.ListEventsRequestV2{CalendarID: "/cal", Start: start, End: start.Add(72 * time.Hour), View: calendar.RecurrenceExpanded}
			_, skipped := appleEventsFromObjects([]caldav.CalendarObject{{Path: "/cal/synthetic.ics", Data: object}}, req)
			if skipped != 1 {
				t.Fatalf("malformed object accepted as complete; skipped=%d", skipped)
			}
		})
	}
}

func TestAppleExpandedRejectsUnsupportedRecurrence(t *testing.T) {
	start := time.Date(2030, 1, 1, 9, 0, 0, 0, time.UTC)
	for _, scenario := range []string{"this-and-future", "unknown-range", "foreign-uid", "multiple-rules"} {
		t.Run(scenario, func(t *testing.T) {
			master := ical.NewEvent()
			master.Props.SetText(ical.PropUID, "synthetic")
			master.Props.SetDateTime(ical.PropDateTimeStart, start)
			master.Props.SetDateTime(ical.PropDateTimeEnd, start.Add(time.Hour))
			rule := ical.NewProp(ical.PropRecurrenceRule)
			rule.Value = "FREQ=DAILY;COUNT=5"
			master.Props.Add(rule)
			if scenario == "multiple-rules" {
				other := ical.NewProp(ical.PropRecurrenceRule)
				other.Value = "FREQ=WEEKLY;COUNT=2"
				master.Props.Add(other)
			}
			container := ical.NewCalendar()
			container.Children = append(container.Children, master.Component)
			if scenario != "multiple-rules" {
				exception := ical.NewEvent()
				exception.Props.SetText(ical.PropUID, "synthetic")
				if scenario == "foreign-uid" {
					exception.Props.SetText(ical.PropUID, "other")
				}
				rid := ical.NewProp(ical.PropRecurrenceID)
				rid.SetDateTime(start.Add(24 * time.Hour))
				if scenario == "this-and-future" {
					rid.Params.Set("RANGE", "THISANDFUTURE")
				}
				if scenario == "unknown-range" {
					rid.Params.Set("RANGE", "FUTURE")
				}
				exception.Props.Add(rid)
				exception.Props.SetDateTime(ical.PropDateTimeStart, start.Add(26*time.Hour))
				exception.Props.SetDateTime(ical.PropDateTimeEnd, start.Add(27*time.Hour))
				container.Children = append(container.Children, exception.Component)
			}
			request := calendar.ListEventsRequestV2{CalendarID: "/cal", Start: start.Add(48 * time.Hour), End: start.Add(72 * time.Hour), View: calendar.RecurrenceExpanded}
			_, skipped := appleEventsFromObjects([]caldav.CalendarObject{{Path: "/cal/test.ics", Data: container}}, request)
			if skipped != 1 {
				t.Fatalf("unsupported recurrence accepted: skipped=%d", skipped)
			}
		})
	}
}

func TestAppleAllDayTimezoneBoundaryIsIncomplete(t *testing.T) {
	day := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, recurring := range []bool{false, true} {
		event := ical.NewEvent()
		event.Props.SetText(ical.PropUID, "synthetic")
		event.Props.SetDate(ical.PropDateTimeStart, day)
		event.Props.SetDate(ical.PropDateTimeEnd, day.AddDate(0, 0, 1))
		if recurring {
			rule := ical.NewProp(ical.PropRecurrenceRule)
			rule.Value = "FREQ=DAILY;COUNT=3"
			event.Props.Add(rule)
		}
		container := ical.NewCalendar()
		container.Children = append(container.Children, event.Component)
		for _, tc := range []struct {
			name     string
			from, to time.Duration
		}{
			{"no UTC overlap", -5 * time.Hour, -4 * time.Hour},
			{"partial UTC overlap", -4 * time.Hour, 4 * time.Hour},
		} {
			req := calendar.ListEventsRequestV2{CalendarID: "/cal", Start: day.Add(tc.from), End: day.Add(tc.to), View: calendar.RecurrenceExpanded}
			_, skipped := appleEventsFromObjectsWithBoundary([]caldav.CalendarObject{{Path: "/cal/test.ics", Data: container}}, req, true)
			if skipped != 1 {
				t.Fatalf("%s ambiguous all-day event accepted, recurring=%v", tc.name, recurring)
			}
		}
	}
}

func TestAppleRecurrenceIncludesStartAndDeduplicates(t *testing.T) {
	start := time.Date(2030, 1, 1, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name          string
		rule, exclude bool
		want          int
	}{
		{"rdate only", false, false, 2}, {"rrule duplicate", true, false, 2}, {"exdate start", false, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := ical.NewEvent()
			event.Props.SetText(ical.PropUID, "synthetic")
			event.Props.SetDateTime(ical.PropDateTimeStart, start)
			event.Props.SetDateTime(ical.PropDateTimeEnd, start.Add(time.Hour))
			rdate := ical.NewProp(ical.PropRecurrenceDates)
			rdate.SetDateTime(start.Add(24 * time.Hour))
			event.Props.Add(rdate)
			if tc.rule {
				r := ical.NewProp(ical.PropRecurrenceRule)
				r.Value = "FREQ=DAILY;COUNT=2"
				event.Props.Add(r)
			}
			if tc.exclude {
				ex := ical.NewProp(ical.PropExceptionDates)
				ex.SetDateTime(start)
				event.Props.Add(ex)
			}
			object := ical.NewCalendar()
			object.Children = append(object.Children, event.Component)
			items, err := appleEventsFromObject(caldav.CalendarObject{Path: "/cal/test.ics", Data: object}, calendar.ListEventsRequestV2{CalendarID: "/cal", Start: start.Add(-time.Hour), End: start.Add(48 * time.Hour), View: calendar.RecurrenceExpanded})
			active := 0
			for _, item := range items {
				if item.Status != "cancelled" {
					active++
				}
			}
			if err != nil || active != tc.want {
				t.Fatalf("active=%d err=%v want=%d", active, err, tc.want)
			}
		})
	}
}

func TestAppleEventsFromObjectIncludesOverlappingOccurrence(t *testing.T) {
	master := ical.NewEvent()
	master.Props.SetText(ical.PropUID, "series")
	master.Props.SetDateTime(ical.PropDateTimeStart, time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC))
	master.Props.SetDateTime(ical.PropDateTimeEnd, time.Date(2026, 8, 20, 11, 0, 0, 0, time.UTC))
	rule := ical.NewProp(ical.PropRecurrenceRule)
	rule.Value = "FREQ=DAILY;COUNT=2"
	master.Props.Add(rule)
	container := ical.NewCalendar()
	container.Children = append(container.Children, master.Component)
	items, err := appleEventsFromObject(caldav.CalendarObject{Path: "/cal/series.ics", Data: container}, calendar.ListEventsRequestV2{CalendarID: "/cal", Start: time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC), End: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC), View: calendar.RecurrenceExpanded})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Start.DateTime == "" {
		t.Fatalf("overlapping occurrences = %#v", items)
	}
}

func TestAppleEventsFromObjectFiltersMovedExceptionsByActualInterval(t *testing.T) {
	master := ical.NewEvent()
	master.Props.SetText(ical.PropUID, "series")
	master.Props.SetDateTime(ical.PropDateTimeStart, time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC))
	master.Props.SetDateTime(ical.PropDateTimeEnd, time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC))
	rule := ical.NewProp(ical.PropRecurrenceRule)
	rule.Value = "FREQ=DAILY;COUNT=2"
	master.Props.Add(rule)
	movedIntoWindow := cloneAppleEvent(master)
	movedIntoWindow.Props.Del(ical.PropRecurrenceRule)
	movedIntoWindow.Props.SetDateTime(ical.PropRecurrenceID, time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC))
	movedIntoWindow.Props.SetDateTime(ical.PropDateTimeStart, time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC))
	movedIntoWindow.Props.SetDateTime(ical.PropDateTimeEnd, time.Date(2026, 8, 21, 11, 0, 0, 0, time.UTC))
	movedIntoWindow.Props.SetText(ical.PropSummary, "Moved into window")
	movedOutOfWindow := cloneAppleEvent(master)
	movedOutOfWindow.Props.Del(ical.PropRecurrenceRule)
	movedOutOfWindow.Props.SetDateTime(ical.PropRecurrenceID, time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC))
	movedOutOfWindow.Props.SetDateTime(ical.PropDateTimeStart, time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC))
	movedOutOfWindow.Props.SetDateTime(ical.PropDateTimeEnd, time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC))
	movedOutOfWindow.Props.SetText(ical.PropSummary, "Moved out of window")
	container := ical.NewCalendar()
	container.Children = append(container.Children, master.Component, movedIntoWindow.Component, movedOutOfWindow.Component)

	items, err := appleEventsFromObject(caldav.CalendarObject{Path: "/cal/series.ics", Data: container}, calendar.ListEventsRequestV2{
		CalendarID: "/cal", Start: time.Date(2026, 8, 21, 9, 30, 0, 0, time.UTC), End: time.Date(2026, 8, 21, 11, 30, 0, 0, time.UTC), View: calendar.RecurrenceExpanded,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Title != "Moved into window" {
		t.Fatalf("moved exceptions = %#v", items)
	}
}

func TestAppleEventCreateCarriesSyncMarker(t *testing.T) {
	event, err := appleEventFromCreate("uid", calendar.EventCreateV2{Start: calendar.EventTime{DateTime: "2026-08-20T09:00:00Z", TimeZone: "UTC"}, End: calendar.EventTime{DateTime: "2026-08-20T10:00:00Z", TimeZone: "UTC"}, SyncMarker: &calendar.SyncMarker{RuleID: "rule", SourceEventID: "source"}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := event.Props.Text(appleSyncMarkerProperty)
	if got != calendar.SyncMarkerValue("rule", "source") {
		t.Fatalf("sync marker = %q", got)
	}
}

func TestAppleEventsFromObjectReportsMalformedStandaloneTime(t *testing.T) {
	event := ical.NewEvent()
	event.Props.SetText(ical.PropUID, "broken")
	container := ical.NewCalendar()
	container.Children = append(container.Children, event.Component)
	_, err := appleEventsFromObject(caldav.CalendarObject{Path: "/cal/broken.ics", Data: container}, calendar.ListEventsRequestV2{CalendarID: "/cal", Start: time.Now(), End: time.Now().Add(time.Hour), View: calendar.RecurrenceExpanded})
	if err == nil {
		t.Fatal("malformed event was silently dropped")
	}
}

func TestAppleEventsFromObjectsKeepsValidEventsWhenOneObjectIsMalformed(t *testing.T) {
	valid := ical.NewEvent()
	valid.Props.SetText(ical.PropUID, "valid")
	valid.Props.SetDateTime(ical.PropDateTimeStart, time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC))
	valid.Props.SetDateTime(ical.PropDateTimeEnd, time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC))
	validCalendar := ical.NewCalendar()
	validCalendar.Children = append(validCalendar.Children, valid.Component)

	malformed := ical.NewEvent()
	malformed.Props.SetText(ical.PropUID, "malformed")
	malformedCalendar := ical.NewCalendar()
	malformedCalendar.Children = append(malformedCalendar.Children, malformed.Component)

	items, skipped := appleEventsFromObjects([]caldav.CalendarObject{
		{Path: "/cal/valid.ics", Data: validCalendar},
		{Path: "/cal/malformed.ics", Data: malformedCalendar},
	}, calendar.ListEventsRequestV2{
		CalendarID: "/cal", Start: time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC), View: calendar.RecurrenceExpanded,
	})

	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}
	if len(items) != 1 || items[0].ICalUID != "valid" {
		t.Fatalf("items = %#v, want the valid event", items)
	}
}
