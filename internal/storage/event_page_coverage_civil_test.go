package storage

import (
	"testing"
	"time"
)

func TestCivilRangeCovered(t *testing.T) {
	start := time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 25, 23, 0, 0, 0, time.UTC)
	if !civilRangeCovered("google", "2026-10-25", "2026-10-26", "Europe/Belgrade", start, end) {
		t.Fatal("25-hour civil day should fit")
	}
	if civilRangeCovered("google", "2026-10-25", "2026-10-26", "Europe/Belgrade", start.Add(time.Second), end) {
		t.Fatal("missing first second of civil day")
	}
	if civilRangeCovered("google", "2026-10-25", "2026-10-26", "Europe/Belgrade", start, end.Add(-time.Second)) {
		t.Fatal("missing last second of civil day")
	}
	for _, zone := range []string{"", "Local", "invalid/zone"} {
		if civilRangeCovered("google", "2026-10-25", "2026-10-26", zone, start, end) {
			t.Fatalf("unknown zone %q", zone)
		}
	}
}

func TestCivilRangeCoveredAtFixedAppleMidnight(t *testing.T) {
	for _, test := range []struct {
		zone     string
		midnight time.Time
	}{
		{"UTC+05:30:17", time.Date(2026, 10, 24, 18, 29, 43, 0, time.UTC)},
		{"UTC-00:19:32", time.Date(2026, 10, 25, 0, 19, 32, 0, time.UTC)},
	} {
		t.Run(test.zone, func(t *testing.T) {
			start, end := test.midnight, test.midnight.Add(24*time.Hour)
			const startDate, endDate = "2026-10-25", "2026-10-26"
			if !civilRangeCovered("apple", startDate, endDate, test.zone, start, end) {
				t.Fatal("exact window must cover the day")
			}
			if civilRangeCovered("apple", startDate, endDate, test.zone, start.Add(time.Second), end) ||
				civilRangeCovered("apple", startDate, endDate, test.zone, start, end.Add(-time.Second)) {
				t.Fatal("window is missing a second")
			}
			for _, provider := range []string{"google", "microsoft"} {
				if civilRangeCovered(provider, startDate, endDate, test.zone, start, end) {
					t.Fatalf("%s accepted Apple metadata", provider)
				}
			}
		})
	}
}
