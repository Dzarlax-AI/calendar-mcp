package storage

import (
	"bytes"
	"strings"
	"testing"

	"calendar-mcp/internal/calendar"
)

func TestProjectionSnapshotSizeLimit(t *testing.T) {
	event := calendar.EventV2{ID: "event", CalendarID: "source", Title: strings.Repeat("x", 5<<20)}
	payload, err := encodeProjectionSnapshotPayload([]calendar.EventV2{event}, "source")
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) <= 4<<20 || len(payload) >= maxProjectionSnapshotBytes {
		t.Fatal("fixture not above previous limit")
	}
	if events, err := decodeProjectionSnapshotPayload(payload, "source"); err != nil || len(events) != 1 {
		t.Fatalf("valid snapshot rejected: %v", err)
	}
	oversized := append([]byte(`{"version":1,"events":[],"attendee_evidence":[],"extra":"`), bytes.Repeat([]byte("x"), maxProjectionSnapshotBytes)...)
	oversized = append(oversized, []byte(`"}`)...)
	if _, err = decodeProjectionSnapshotPayload(oversized, "source"); err == nil {
		t.Fatal("oversized snapshot accepted")
	}
}
