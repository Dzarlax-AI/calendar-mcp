package apple

import (
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

func TestSetAppleEventTime_AllDayUsesDate(t *testing.T) {
	ev := ical.NewEvent()
	tm := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)

	setAppleEventTime(*ev, ical.PropDateTimeStart, tm, true)

	prop := ev.Props.Get(ical.PropDateTimeStart)
	if prop == nil {
		t.Fatalf("DTSTART property was not set")
	}
	if got := prop.ValueType(); got != ical.ValueDate {
		t.Fatalf("ValueType = %q, want %q", got, ical.ValueDate)
	}
}

func TestConvertEventMarksAllDay(t *testing.T) {
	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, "event-1")
	ev.Props.SetText(ical.PropSummary, "All day")
	ev.Props.SetDate(ical.PropDateTimeStart, time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC))
	ev.Props.SetDate(ical.PropDateTimeEnd, time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC))

	got := convertEvent(*ev, "cal", "cal/event-1.ics")
	if !got.AllDay {
		t.Fatalf("AllDay = false, want true")
	}
}
