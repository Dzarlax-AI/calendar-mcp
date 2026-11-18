package storage

import (
	"testing"
	"time"
)

func TestCivilRangeCovered(t *testing.T) {
	start := time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 25, 23, 0, 0, 0, time.UTC)
	if !civilRangeCovered("2026-10-25", "2026-10-26", "Europe/Belgrade", start, end) {
		t.Fatal("25-hour civil day should fit")
	}
	if civilRangeCovered("2026-10-25", "2026-10-26", "Europe/Belgrade", start.Add(time.Second), end) {
		t.Fatal("missing first second of civil day")
	}
	if civilRangeCovered("2026-10-25", "2026-10-26", "Europe/Belgrade", start, end.Add(-time.Second)) {
		t.Fatal("missing last second of civil day")
	}
	for _, zone := range []string{"", "Local", "invalid/zone"} {
		if civilRangeCovered("2026-10-25", "2026-10-26", zone, start, end) {
			t.Fatalf("unknown zone %q", zone)
		}
	}
}
