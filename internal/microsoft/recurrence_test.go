package microsoft

import (
	"encoding/json"
	"strings"
	"testing"

	"calendar-mcp/internal/calendar"
)

func TestGraphRecurrenceLinesMapsWindowsZoneAndWeeklyPattern(t *testing.T) {
	lines, zone, err := graphRecurrenceLines(&graphRecurrence{
		Pattern: graphRecurrencePattern{
			Type:           "weekly",
			Interval:       2,
			DaysOfWeek:     []string{"monday", "wednesday"},
			FirstDayOfWeek: "monday",
		},
		Range: graphRecurrenceRange{
			Type:                "numbered",
			NumberOfOccurrences: 8,
			RecurrenceTimeZone:  "Central Europe Standard Time",
		},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if zone != "Europe/Budapest" {
		t.Fatalf("zone = %q, want Europe/Budapest", zone)
	}
	if got, want := strings.Join(lines, "\n"), "RRULE:FREQ=WEEKLY;BYDAY=MO,WE;WKST=MO;INTERVAL=2;COUNT=8"; got != want {
		t.Fatalf("recurrence = %q, want %q", got, want)
	}
}

func TestGraphRecurrenceJSONOmitsUnsetOptionalFields(t *testing.T) {
	data, err := json.Marshal(graphRecurrence{Pattern: graphRecurrencePattern{Type: "daily", Interval: 1}, Range: graphRecurrenceRange{Type: "noEnd", StartDate: "2026-08-20"}})
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, forbidden := range []string{"firstDayOfWeek", "index", "endDate", "numberOfOccurrences"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("recurrence JSON %s contains unset %s", text, forbidden)
		}
	}
}

func TestPortableRecurrenceToGraphSupportsCommonPatterns(t *testing.T) {
	start := calendar.EventTime{DateTime: "2026-08-24T09:00:00+02:00", TimeZone: "Europe/Belgrade"}
	value, err := portableToGraphRecurrence([]string{"RRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE;COUNT=8"}, start)
	if err != nil {
		t.Fatal(err)
	}
	if value.Pattern.Type != "weekly" || value.Pattern.Interval != 2 || strings.Join(value.Pattern.DaysOfWeek, ",") != "monday,wednesday" || value.Range.Type != "numbered" || value.Range.NumberOfOccurrences != 8 || value.Range.RecurrenceTimeZone == "" {
		t.Fatalf("graph recurrence = %#v", value)
	}
}

func TestPortableRecurrenceToGraphRejectsLossyRDate(t *testing.T) {
	start := calendar.EventTime{DateTime: "2026-08-24T09:00:00Z", TimeZone: "UTC"}
	_, err := portableToGraphRecurrence([]string{"RRULE:FREQ=DAILY", "RDATE:20260830T090000Z"}, start)
	if err == nil || !strings.Contains(err.Error(), "RDATE") {
		t.Fatalf("error = %v", err)
	}
}

func TestGraphRecurrenceLinesEndsTimedSeriesAtLocalEndOfDay(t *testing.T) {
	lines, zone, err := graphRecurrenceLines(&graphRecurrence{
		Pattern: graphRecurrencePattern{Type: "daily", Interval: 1},
		Range: graphRecurrenceRange{
			Type:               "endDate",
			EndDate:            "2026-10-25",
			RecurrenceTimeZone: "Central Europe Standard Time",
		},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if zone != "Europe/Budapest" {
		t.Fatalf("zone = %q", zone)
	}
	if got := strings.Join(lines, "\n"); got != "RRULE:FREQ=DAILY;UNTIL=20261025T225959Z" {
		t.Fatalf("recurrence = %q", got)
	}
}

func TestGraphRecurrenceLinesRejectsAmbiguousRelativePattern(t *testing.T) {
	_, _, err := graphRecurrenceLines(&graphRecurrence{
		Pattern: graphRecurrencePattern{
			Type:       "relativeMonthly",
			DaysOfWeek: []string{"monday", "tuesday"},
			Index:      "first",
		},
		Range: graphRecurrenceRange{Type: "noEnd", RecurrenceTimeZone: "UTC"},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error = %v, want ambiguous relative recurrence", err)
	}
}
