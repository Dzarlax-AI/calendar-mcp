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
