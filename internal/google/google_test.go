package google

import (
	"testing"
	"time"
)

func TestToGoogleEventTime_AllDayUsesDate(t *testing.T) {
	tm := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)
	got := toGoogleEventTime(tm, true)
	if got.Date != "2026-05-30" {
		t.Fatalf("Date = %q, want 2026-05-30", got.Date)
	}
	if got.DateTime != "" {
		t.Fatalf("DateTime = %q, want empty", got.DateTime)
	}
}

func TestToGoogleEventTime_TimedUsesDateTime(t *testing.T) {
	tm := time.Date(2026, 5, 30, 13, 0, 0, 0, time.FixedZone("CEST", 2*60*60))
	got := toGoogleEventTime(tm, false)
	if got.Date != "" {
		t.Fatalf("Date = %q, want empty", got.Date)
	}
	if got.DateTime != "2026-05-30T13:00:00+02:00" {
		t.Fatalf("DateTime = %q, want 2026-05-30T13:00:00+02:00", got.DateTime)
	}
}
