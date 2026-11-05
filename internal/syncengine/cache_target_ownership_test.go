package syncengine

import (
	"context"
	"errors"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/calendarprojection"
	"calendar-mcp/internal/storage"
)

type sparseCacheResponseProvider struct{ *fakeV2Provider }

func (p *sparseCacheResponseProvider) UpdateEventV2(context.Context, calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	p.updated++
	return &calendar.OperationResult{Status: "completed"}, nil
}

func TestCacheTargetRequiresConfirmedETagAndCompleteUpdateResponse(t *testing.T) {
	for _, tc := range []struct {
		name                string
		cancelled, haveETag bool
	}{
		{"update without etag", false, false}, {"delete without etag", true, false}, {"sparse update response", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			event := calendar.EventV2{CalendarID: "google:source", ID: "event", Title: "changed", Start: calendar.EventTime{DateTime: now.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"}, End: calendar.EventTime{DateTime: now.Add(2 * time.Hour).Format(time.RFC3339), TimeZone: "UTC"}}
			identity, err := calendarprojection.Identity(event.CalendarID, event)
			if err != nil {
				t.Fatal(err)
			}
			sourceID := cacheExecutionEventID(identity)
			mappingID := newMappingID("rule", sourceID, "")
			m := storage.Mapping{ID: mappingID, RuleID: "rule", ObjectKind: "cache_instance", SourceEventID: sourceID, TargetEventID: "target", ContentHash: "old", ReconciliationState: "current", Version: 1, Incarnation: "synthetic"}
			if tc.haveETag {
				m.TargetETag = "confirmed"
			}
			mappings := &fakeMappings{values: map[string]storage.Mapping{mappingID: m}}
			target := &sparseCacheResponseProvider{&fakeV2Provider{name: "apple"}}
			engine := testNew(calendar.NewRegistry([]calendar.Provider{target}), mappings)
			rule := storage.Rule{ID: "rule", SourceCalendarID: event.CalendarID, TargetCalendarID: "apple:target", State: "paused", SyncMode: "cached_instances", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none"}
			batch := cacheExecutionBatch{Events: []calendar.EventV2{event}, Timezone: "UTC", WindowStart: now, WindowEnd: now.Add(24 * time.Hour), HistoryComplete: true}
			if tc.cancelled {
				batch.Events = nil
				batch.Removals = []calendarprojection.RemovalObservation{{Removal: calendar.EventSyncRemoval{ObjectID: event.ID, Reason: calendar.EventSyncProviderCancellation}, PreviousIdentity: identity}}
			}
			if _, err = engine.runCachedBatch(t.Context(), rule, batch, false); !errors.Is(err, ErrCacheTargetUnverified) {
				t.Fatalf("error=%v", err)
			}
			got := mappings.values[mappingID]
			if !tc.haveETag && (got.ReconciliationState != "current" || target.updated != 0 || target.deleted != 0) {
				t.Fatalf("unverified target mutated: %+v", got)
			}
			if tc.haveETag && (got.ReconciliationState != "pending_update" || got.TargetETag != "confirmed" || target.updated != 1) {
				t.Fatalf("sparse result not parked: %+v", got)
			}
		})
	}
}
