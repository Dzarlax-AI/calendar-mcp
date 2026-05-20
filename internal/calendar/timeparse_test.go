package calendar

import "testing"

func TestParseEventTimeRange_DateOnly(t *testing.T) {
	start, end, allDay, err := ParseEventTimeRange("2026-05-30", "2026-05-31")
	if err != nil {
		t.Fatalf("ParseEventTimeRange returned error: %v", err)
	}
	if !allDay {
		t.Fatalf("allDay = false, want true")
	}
	if got := start.Format(DateLayout); got != "2026-05-30" {
		t.Fatalf("start = %q, want 2026-05-30", got)
	}
	if got := end.Format(DateLayout); got != "2026-05-31" {
		t.Fatalf("end = %q, want 2026-05-31", got)
	}
}

func TestParseEventTimeRange_DateTime(t *testing.T) {
	_, _, allDay, err := ParseEventTimeRange("2026-05-30T13:00:00+02:00", "2026-05-30T14:00:00+02:00")
	if err != nil {
		t.Fatalf("ParseEventTimeRange returned error: %v", err)
	}
	if allDay {
		t.Fatalf("allDay = true, want false")
	}
}

func TestParseEventTimeRange_RejectsMixedFormats(t *testing.T) {
	_, _, _, err := ParseEventTimeRange("2026-05-30", "2026-05-30T14:00:00+02:00")
	if err == nil {
		t.Fatalf("ParseEventTimeRange returned nil error for mixed formats")
	}
}
