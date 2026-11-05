package storage

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"calendar-mcp/internal/calendar"
)

func TestProjectionPayloadEvidence(t *testing.T) {
	source := "synthetic-calendar"
	events := []calendar.EventV2{{ID: "a", CalendarID: source, SourceAttendeesComplete: true}, {ID: "b", CalendarID: source}}
	public, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(public, []byte("AttendeesComplete")) {
		t.Fatal("private evidence leaked")
	}
	page := calendar.EventSyncPage{Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: "a"}, Event: events[0]}, {Object: calendar.SyncObject{ObjectID: "b"}, Event: events[1]}}}
	for _, kind := range []string{"page", "snapshot"} {
		t.Run(kind, func(t *testing.T) {
			var raw []byte
			if kind == "page" {
				raw, err = encodeProjectionPagePayload(page, source)
			} else {
				raw, err = encodeProjectionSnapshotPayload(events, source)
			}
			if err != nil {
				t.Fatal(err)
			}
			decode := func(raw []byte) ([]calendar.EventV2, error) {
				if kind == "snapshot" {
					return decodeProjectionSnapshotPayload(raw, source)
				}
				p, e := decodeProjectionPagePayload(raw, source)
				if e != nil {
					return nil, e
				}
				return []calendar.EventV2{p.Upserts[0].Event, p.Upserts[1].Event}, nil
			}
			got, err := decode(raw)
			if err != nil || !got[0].SourceAttendeesComplete || got[1].SourceAttendeesComplete {
				t.Fatalf("round trip failed: %v", err)
			}
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name   string
				mutate func(map[string]any)
			}{
				{"missing evidence", func(m map[string]any) { delete(m, "attendee_evidence") }},
				{"unknown version", func(m map[string]any) { m["version"] = 2 }},
				{"unknown field", func(m map[string]any) { m["unexpected"] = true }},
				{"missing item", func(m map[string]any) { m["attendee_evidence"] = m["attendee_evidence"].([]any)[:1] }},
				{"duplicate id", func(m map[string]any) { a := m["attendee_evidence"].([]any); a[1].(map[string]any)["event_id"] = "a" }},
				{"unknown id", func(m map[string]any) { m["attendee_evidence"].([]any)[0].(map[string]any)["event_id"] = "other" }},
				{"missing bool", func(m map[string]any) { delete(m["attendee_evidence"].([]any)[0].(map[string]any), "complete") }},
				{"null bool", func(m map[string]any) { m["attendee_evidence"].([]any)[0].(map[string]any)["complete"] = nil }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var m map[string]any
					_ = json.Unmarshal(raw, &m)
					tc.mutate(m)
					bad, _ := json.Marshal(m)
					if _, err := decode(bad); err == nil {
						t.Fatal("accepted corrupt evidence")
					}
				})
			}
			if _, err := decode(append(raw, []byte(` {}`)...)); err == nil {
				t.Fatal("accepted trailing JSON")
			}
			bad := bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1)
			if _, err := decode(bad); err == nil {
				t.Fatal("accepted duplicate JSON key")
			}
			if kind == "page" {
				if _, err := decodeProjectionPagePayload(raw, "wrong"); err == nil {
					t.Fatal("accepted wrong calendar")
				}
			} else if _, err := decodeProjectionSnapshotPayload(raw, "wrong"); err == nil {
				t.Fatal("accepted wrong calendar")
			}
		})
	}
}

func TestProjectionPayloadRejectsNestedDuplicatesNullsAndPrivateFieldInjection(t *testing.T) {
	const source = "synthetic-calendar"
	event := calendar.EventV2{ID: "synthetic-event", CalendarID: source, SourceAttendeesComplete: true}
	snapshot, err := encodeProjectionSnapshotPayload([]calendar.EventV2{event}, source)
	if err != nil {
		t.Fatal(err)
	}
	page, err := encodeProjectionPagePayload(calendar.EventSyncPage{Upserts: []calendar.EventSyncUpsert{{Object: calendar.SyncObject{ObjectID: event.ID}, Event: event}}}, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		raw    []byte
		decode func([]byte) error
	}{
		{"snapshot", snapshot, func(raw []byte) error { _, err := decodeProjectionSnapshotPayload(raw, source); return err }},
		{"page", page, func(raw []byte) error { _, err := decodeProjectionPagePayload(raw, source); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mutation := range []struct{ name, old, replacement string }{
				{"nested duplicate event ID", `"id":"synthetic-event"`, `"id":"synthetic-event","id":"synthetic-event"`},
				{"nested case-fold duplicate", `"event_id":"synthetic-event"`, `"event_id":"synthetic-event","EVENT_ID":"synthetic-event"`},
				{"private source evidence injection", `"id":"synthetic-event"`, `"id":"synthetic-event","SourceAttendeesComplete":true`},
				{"private target evidence injection", `"id":"synthetic-event"`, `"id":"synthetic-event","AttendeesKnown":true`},
			} {
				t.Run(mutation.name, func(t *testing.T) {
					bad := bytes.Replace(tc.raw, []byte(mutation.old), []byte(mutation.replacement), 1)
					if bytes.Equal(bad, tc.raw) {
						t.Fatal("test mutation did not match")
					}
					if err := tc.decode(bad); err == nil {
						t.Fatal("corrupted nested JSON accepted")
					}
				})
			}
		})
	}
	// The page envelope also rejects a null event, not just a null snapshot.
	var pageObject map[string]any
	if err := json.Unmarshal(page, &pageObject); err != nil {
		t.Fatal(err)
	}
	upserts := pageObject["page"].(map[string]any)["Upserts"].([]any)
	upserts[0].(map[string]any)["Event"] = nil
	pageWithNull, err := json.Marshal(pageObject)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = decodeProjectionPagePayload(pageWithNull, source); err == nil {
		t.Fatal("null page event accepted")
	}
	for _, raw := range [][]byte{
		[]byte(`{"version":1,"events":null,"attendee_evidence":[]}`),
		[]byte(`{"version":1,"events":[null],"attendee_evidence":[{"event_id":"synthetic-event","complete":true}]}`),
	} {
		if _, err := decodeProjectionSnapshotPayload(raw, source); err == nil {
			t.Fatal("null snapshot events accepted")
		}
	}
}

func TestProjectionSnapshotPublicJSONOmitsCursorAndPayload(t *testing.T) {
	const privateCursor = "synthetic-private-cursor"
	privatePayload := []byte("synthetic-private-payload")
	for _, value := range []any{
		ProjectionSnapshot{Cursor: privateCursor, Payload: privatePayload},
		ProjectionSnapshotInput{Cursor: privateCursor, Payload: privatePayload},
		ProjectionSnapshotInspection{Snapshot: ProjectionSnapshot{Cursor: privateCursor, Payload: privatePayload}},
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		lower := bytes.ToLower(raw)
		if bytes.Contains(lower, []byte("cursor")) || bytes.Contains(lower, []byte("payload")) || bytes.Contains(raw, []byte(privateCursor)) || bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString(privatePayload))) {
			t.Fatal("private cursor or payload leaked into JSON")
		}
	}
}
