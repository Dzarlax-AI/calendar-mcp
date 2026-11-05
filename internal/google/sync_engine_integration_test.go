package google

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
	"calendar-mcp/internal/syncengine"
)

type syntheticSyncMappings struct{ values map[string]storage.Mapping }

type syntheticDispatchFence struct{}

func (syntheticDispatchFence) CheckProviderDispatch(context.Context) error { return nil }

func (m *syntheticSyncMappings) InsertPendingCreateMapping(_ context.Context, value storage.Mapping) (string, error) {
	for _, existing := range m.values {
		if existing.ID == value.ID || existing.RuleID == value.RuleID && existing.SourceEventID == value.SourceEventID && existing.OriginalStart == value.OriginalStart || existing.RuleID == value.RuleID && existing.TargetEventID == value.TargetEventID {
			return "", nil
		}
	}
	value.Incarnation = "synthetic-" + value.ID
	value.Version = 1
	m.values[value.ID] = value
	return value.Incarnation, nil
}

func (m *syntheticSyncMappings) InsertPendingMutationMapping(ctx context.Context, value storage.Mapping) (string, error) {
	return m.InsertPendingCreateMapping(ctx, value)
}

func (m *syntheticSyncMappings) UpdateMappingCAS(_ context.Context, value storage.Mapping, expected int64) error {
	old, ok := m.values[value.ID]
	if !ok || old.Version != expected || old.Incarnation != value.Incarnation {
		return storage.ErrMappingConflict
	}
	value.Version = expected + 1
	m.values[value.ID] = value
	return nil
}

func (m *syntheticSyncMappings) DeleteMappingCAS(_ context.Context, id, incarnation string, expected int64) error {
	old, ok := m.values[id]
	if !ok || old.Version != expected || old.Incarnation != incarnation {
		return storage.ErrMappingConflict
	}
	delete(m.values, id)
	return nil
}

func (m *syntheticSyncMappings) ListMappings(_ context.Context, id string) ([]storage.Mapping, error) {
	var out []storage.Mapping
	for _, v := range m.values {
		if v.RuleID == id {
			out = append(out, v)
		}
	}
	return out, nil
}
func (m *syntheticSyncMappings) UpsertMapping(_ context.Context, v storage.Mapping) error {
	m.values[v.ID] = v
	return nil
}
func (m *syntheticSyncMappings) DeleteMapping(_ context.Context, id string) error {
	delete(m.values, id)
	return nil
}

func TestGoogleSourceThroughEngineCreatesSafeParticipantDescription(t *testing.T) {
	var postCount int
	start := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	end := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	p, closeServer := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/calendars/source/events"):
			_, _ = fmt.Fprintf(w, `{"timeZone":"UTC","items":[{"id":"event","summary":"Meeting","description":"Agenda","start":{"dateTime":%q},"end":{"dateTime":%q},"organizer":{"self":true,"email":"owner@example.invalid","displayName":"Owner & Host"},"attendees":[{"email":"alice@example.invalid","displayName":"Alice <Admin>"},{"email":"bob@example.invalid","displayName":"Bob & Co"}]}]}`, start, end)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/calendars/target/events"):
			_, _ = io.WriteString(w, `{"items":[]}`)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/calendars/target/events"):
			postCount++
			if r.URL.Query().Get("sendUpdates") != "none" {
				t.Error("notifications were not suppressed")
			}
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			for _, key := range []string{"attendees", "organizer", "conferenceData"} {
				if _, ok := body[key]; ok {
					t.Errorf("unexpected invite field %s", key)
				}
			}
			var description string
			if err := json.Unmarshal(body["description"], &description); err != nil {
				t.Error("missing description")
				return
			}
			for _, value := range []string{"Alice &lt;Admin&gt;", "alice@example.invalid", "Bob &amp; Co", "bob@example.invalid", "Owner &amp; Host", "owner@example.invalid"} {
				if !strings.Contains(description, value) {
					t.Errorf("description missing escaped participant %q", value)
				}
			}
			_, _ = fmt.Fprintf(w, `{"id":"target-event","start":{"dateTime":%q},"end":{"dateTime":%q}}`, start, end)
		default:
			t.Errorf("unexpected synthetic request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	defer closeServer()
	engine := syncengine.New(calendar.NewRegistry([]calendar.Provider{p}), &syntheticSyncMappings{values: map[string]storage.Mapping{}}).WithDispatchFence(syntheticDispatchFence{})
	rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "google:target", State: "paused", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
	if _, err := engine.Run(t.Context(), rule, false); err != nil {
		t.Fatal(err)
	}
	if postCount != 1 {
		t.Fatalf("target POST count = %d", postCount)
	}
}
