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
