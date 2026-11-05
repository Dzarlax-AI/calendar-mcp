package syncengine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

// This synthetic inventory is a point-in-time assertion, not a provider read,
// an ownership proof, or permission to dispatch. Complete means that the caller
// has explicitly supplied the entire relevant target inventory.
type cachePreviewInventory struct {
	Complete bool
	Events   []calendar.EventV2
	Mappings []storage.CacheMapping
	Legacy   []storage.Mapping
	// Unresolved operations preclude even an estimated write for this snapshot.
	HasAmbiguousOperations bool
	// Cancellation identities are separately reconciled source observations.
	// Mere absence from a window is never a cancellation.
	ConfirmedCancellations []string
}

type cacheInventoryAction struct{ IdentityHash, Action string }

var errCachePreviewConflict = errors.New("cache preview inventory is incomplete or conflicting")

func previewIdentityHash(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

// classifyCachePreview consumes only supplied data and never calls a provider
// or mutates anything. Its result is an estimate, not an authorization. In
// particular, the mapping is only an assertion: a matching current target event
// with exact marker, ETag and no-attendee evidence is also required.
func classifyCachePreview(rule storage.Rule, candidates []cacheCopyCandidate, in cachePreviewInventory) ([]cacheInventoryAction, error) {
	if rule.ID == "" || rule.SourceCalendarID == "" || rule.TargetCalendarID == "" || rule.SourceCalendarID == rule.TargetCalendarID || !in.Complete || in.HasAmbiguousOperations {
		return nil, errCachePreviewConflict
	}
	targets := make(map[string]calendar.EventV2, len(in.Events))
	markers := make(map[string]string, len(in.Events))
	for _, e := range in.Events {
		if e.ID == "" || e.CalendarID != rule.TargetCalendarID {
			return nil, errCachePreviewConflict
		}
		if _, ok := targets[e.ID]; ok {
			return nil, errCachePreviewConflict
		}
		targets[e.ID] = e
		if e.Google != nil {
			p := e.Google.PrivateProperties
			if p["calendar_sync_rule"] == rule.ID && p["calendar_source_event"] != "" {
				marker := p["calendar_source_event"]
				if _, ok := markers[marker]; ok {
					return nil, errCachePreviewConflict
				}
				markers[marker] = e.ID
			}
		}
	}
	mappings := make(map[string]storage.CacheMapping, len(in.Mappings))
	mappedTargets := map[string]bool{}
	for _, m := range in.Mappings {
		if m.RuleID != rule.ID || m.SourceCalendarID != rule.SourceCalendarID || m.TargetCalendarID != rule.TargetCalendarID || m.IdentityHash == "" || m.TargetEventID == "" || m.OwnershipMarker == "" || m.TargetETag == "" || m.ContentHash == "" {
			return nil, errCachePreviewConflict
		}
		if _, ok := mappings[m.IdentityHash]; ok || mappedTargets[m.TargetEventID] {
			return nil, errCachePreviewConflict
		}
		mappings[m.IdentityHash] = m
		mappedTargets[m.TargetEventID] = true
	}
	// Existing legacy ownership must be resolved before a new mapping or target
	// change, including when the old source identity cannot be translated.
	if len(in.Legacy) > 0 {
		return nil, errCachePreviewConflict
	}
	for hash, m := range mappings {
		target, ok := targets[m.TargetEventID]
		if !ok || !safeSyntheticPreviewTarget(target, rule.ID, m.OwnershipMarker, m.TargetETag) || markers[m.OwnershipMarker] != m.TargetEventID || hash == "" {
			return nil, errCachePreviewConflict
		}
	}
	actions := make([]cacheInventoryAction, 0, len(candidates)+len(in.ConfirmedCancellations))
	active := map[string]bool{}
	for _, candidate := range candidates {
		identity, err := cacheCopyIdentity(rule.SourceCalendarID, candidate.Event)
		if err != nil || identity != candidate.Identity {
			return nil, errCachePreviewConflict
		}
		hash := previewIdentityHash(identity)
		if active[hash] {
			return nil, errCachePreviewConflict
		}
		active[hash] = true
		created, err := cacheCopyCreate(rule.ID, rule.SourceCalendarID, candidate.Event, "google")
		if err != nil || len(created.Attendees) != 0 || created.SyncMarker == nil {
			return nil, errCachePreviewConflict
		}
		marker := created.SyncMarker.SourceEventID
		if m, ok := mappings[hash]; ok {
			if m.OwnershipMarker != marker {
				return nil, errCachePreviewConflict
			}
			action := "skip"
			if m.ContentHash != hashEvent(candidate.Event) {
				action = "update"
			}
			actions = append(actions, cacheInventoryAction{hash, action})
		} else {
			if _, exists := markers[marker]; exists {
				return nil, errCachePreviewConflict
			}
			actions = append(actions, cacheInventoryAction{hash, "create"})
		}
	}
	seenCancel := map[string]bool{}
	for _, identity := range in.ConfirmedCancellations {
		if identity == "" {
			return nil, errCachePreviewConflict
		}
		hash := previewIdentityHash(identity)
		m, ok := mappings[hash]
		if !ok || active[hash] || seenCancel[hash] || m.OwnershipMarker != newMappingID(rule.ID, identity, "") {
			return nil, errCachePreviewConflict
		}
		seenCancel[hash] = true
		actions = append(actions, cacheInventoryAction{hash, "delete"})
	}
	return actions, nil
}

func safeSyntheticPreviewTarget(e calendar.EventV2, ruleID, marker, etag string) bool {
	return e.Provider == "google" && e.ETag != "" && e.ETag == etag && e.AttendeesKnown && len(e.Attendees) == 0 && e.Organizer != nil && e.Organizer.Self && e.RecurringEventID == "" && e.OriginalStart == nil && len(e.Recurrence) == 0 && e.InstanceKind != "seriesMaster" && e.Conference == nil && e.Google != nil && e.Google.EventType == "default" && len(e.Google.OutOfOffice) == 0 && len(e.Google.FocusTime) == 0 && len(e.Google.WorkingLocation) == 0 && len(e.Google.Birthday) == 0 && e.Google.PrivateProperties["calendar_sync_rule"] == ruleID && e.Google.PrivateProperties["calendar_source_event"] == marker
}
