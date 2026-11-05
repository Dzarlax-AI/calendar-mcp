package syncengine

import (
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

func TestClassifyCachePreviewSyntheticInventory(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	rule := storage.Rule{ID: "rule", SourceCalendarID: "source", TargetCalendarID: "target"}
	event := calendar.EventV2{ID: "event", CalendarID: "source", Title: "title", SourceAttendeesComplete: true, Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}
	candidates, err := cacheCopyCandidates("source", []calendar.EventV2{event}, now.Add(-time.Hour), now.Add(time.Hour), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	identity := candidates[0].Identity
	hash := previewIdentityHash(identity)
	marker := newMappingID(rule.ID, identity, "")
	target := calendar.EventV2{ID: "target-event", CalendarID: "target", Provider: "google", ETag: "etag", AttendeesKnown: true, Organizer: &calendar.PersonV2{Self: true}, Google: &calendar.GoogleEventExtension{EventType: "default", PrivateProperties: map[string]string{"calendar_sync_rule": rule.ID, "calendar_source_event": marker}}}
	mapping := storage.CacheMapping{RuleID: rule.ID, SourceCalendarID: "source", TargetCalendarID: "target", IdentityHash: hash, TargetEventID: target.ID, OwnershipMarker: marker, TargetETag: target.ETag, ContentHash: hashEvent(event)}
	cases := []struct {
		name       string
		candidates []cacheCopyCandidate
		inv        cachePreviewInventory
		action     string
	}{
		{"create", candidates, cachePreviewInventory{Complete: true}, "create"},
		{"unchanged", candidates, cachePreviewInventory{Complete: true, Events: []calendar.EventV2{target}, Mappings: []storage.CacheMapping{mapping}}, "skip"},
		{"update", candidates, cachePreviewInventory{Complete: true, Events: []calendar.EventV2{target}, Mappings: []storage.CacheMapping{func() storage.CacheMapping { m := mapping; m.ContentHash = "changed"; return m }()}}, "update"},
		{"delete only confirmed cancellation", nil, cachePreviewInventory{Complete: true, Events: []calendar.EventV2{target}, Mappings: []storage.CacheMapping{mapping}, ConfirmedCancellations: []string{identity}}, "delete"},
		{"missing source does not delete", nil, cachePreviewInventory{Complete: true, Events: []calendar.EventV2{target}, Mappings: []storage.CacheMapping{mapping}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := classifyCachePreview(rule, tc.candidates, tc.inv)
			if err != nil {
				t.Fatal(err)
			}
			if tc.action == "" {
				if len(out) != 0 {
					t.Fatal(out)
				}
				return
			}
			if len(out) != 1 || out[0].Action != tc.action {
				t.Fatalf("actions: %+v", out)
			}
		})
	}

	bad := []struct {
		name       string
		candidates []cacheCopyCandidate
		inv        cachePreviewInventory
	}{
		{"incomplete inventory", candidates, cachePreviewInventory{}},
		{"ambiguous operation", candidates, cachePreviewInventory{Complete: true, HasAmbiguousOperations: true}},
		{"legacy conflict", candidates, cachePreviewInventory{Complete: true, Legacy: []storage.Mapping{{ID: "old"}}}},
		{"duplicate target", candidates, cachePreviewInventory{Complete: true, Events: []calendar.EventV2{target, target}}},
		{"duplicate mapping", candidates, cachePreviewInventory{Complete: true, Events: []calendar.EventV2{target}, Mappings: []storage.CacheMapping{mapping, mapping}}},
		{"unmapped marker", candidates, cachePreviewInventory{Complete: true, Events: []calendar.EventV2{target}}},
		{"missing target", candidates, cachePreviewInventory{Complete: true, Mappings: []storage.CacheMapping{mapping}}},
		{"bad etag", candidates, cachePreviewInventory{Complete: true, Events: []calendar.EventV2{func() calendar.EventV2 { v := target; v.ETag = "changed"; return v }()}, Mappings: []storage.CacheMapping{mapping}}},
		{"unknown attendees", candidates, cachePreviewInventory{Complete: true, Events: []calendar.EventV2{func() calendar.EventV2 { v := target; v.AttendeesKnown = false; return v }()}, Mappings: []storage.CacheMapping{mapping}}},
		{"has attendee", candidates, cachePreviewInventory{Complete: true, Events: []calendar.EventV2{func() calendar.EventV2 { v := target; v.Attendees = []calendar.AttendeeV2{{}}; return v }()}, Mappings: []storage.CacheMapping{mapping}}},
		{"foreign owner", candidates, cachePreviewInventory{Complete: true, Events: []calendar.EventV2{func() calendar.EventV2 { v := target; v.Organizer = &calendar.PersonV2{}; return v }()}, Mappings: []storage.CacheMapping{mapping}}},
		{"active and cancelled", candidates, cachePreviewInventory{Complete: true, Events: []calendar.EventV2{target}, Mappings: []storage.CacheMapping{mapping}, ConfirmedCancellations: []string{identity}}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			out, err := classifyCachePreview(rule, tc.candidates, tc.inv)
			if err == nil || out != nil {
				t.Fatalf("accepted conflict: %+v %v", out, err)
			}
		})
	}
}
