package calendarprojection

import (
	"errors"
	"sort"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
)

// RemovalObservation describes source provenance only. It never authorizes a
// target deletion. Provider cancellations require explicit reconciliation.
type RemovalObservation struct {
	Removal          calendar.EventSyncRemoval
	PreviousIdentity string
	Unresolved       bool
}

// Materialize builds a source projection from verified staged pages.
// The caller must establish the lease, frozen window, cursor, base revision,
// page chain and publication transaction. Observations are not target deletions.
func Materialize(source, timezone string, windowStart, windowEnd time.Time, mode calendar.EventSyncMode, base []calendar.EventV2, baseAvailable bool, pages []calendar.EventSyncPage) ([]calendar.EventV2, []RemovalObservation, error) {
	if source == "" || strings.ContainsRune(source, '\x00') || timezone == "" || timezone == "Local" || windowStart.IsZero() || !windowEnd.After(windowStart) || len(pages) == 0 || (mode != calendar.EventSyncIncremental && mode != calendar.EventSyncReplacement) || (mode == calendar.EventSyncIncremental && !baseAvailable) {
		return nil, nil, errors.New("invalid source projection attempt")
	}
	zone, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, nil, errors.New("invalid source time zone")
	}
	if mode == calendar.EventSyncReplacement && len(base) != 0 {
		return nil, nil, errors.New("replacement must start empty")
	}
	byID := make(map[string]calendar.EventV2, len(base))
	for _, event := range base {
		if event.ID == "" || event.CalendarID != source || event.Status == "cancelled" || event.InstanceKind == "cancelled" {
			return nil, nil, errors.New("invalid base event")
		}
		if _, exists := byID[event.ID]; exists {
			return nil, nil, errors.New("duplicate base event")
		}
		byID[event.ID] = event
	}
	baseCandidates, err := Candidates(source, base, windowStart, windowEnd, zone)
	if err != nil {
		return nil, nil, err
	}
	if len(baseCandidates) != len(base) {
		return nil, nil, errors.New("base event outside frozen window")
	}
	seen := make(map[string]bool)
	var observations []RemovalObservation
	for i, page := range pages {
		if page.SourceTimeZone != timezone || page.ResetRequired || len(page.Warnings) != 0 || page.Complete != (i == len(pages)-1) || len(page.DeletedObjectIDs) != 0 || len(page.ReplacedObjectIDs) != 0 || len(page.Inventory) != 0 {
			return nil, nil, errors.New("unsupported or incomplete source page")
		}
		if len(page.DeletedEventIDs) != len(page.Removals) {
			return nil, nil, errors.New("incomplete removal provenance")
		}
		removals := make(map[string]calendar.EventSyncRemoval, len(page.Removals))
		for _, removal := range page.Removals {
			if removal.ObjectID == "" || (removal.Reason != calendar.EventSyncProviderCancellation && removal.Reason != calendar.EventSyncWindowEviction) {
				return nil, nil, errors.New("invalid removal provenance")
			}
			if _, exists := removals[removal.ObjectID]; exists {
				return nil, nil, errors.New("duplicate removal provenance")
			}
			removals[removal.ObjectID] = removal
		}
		for _, id := range page.DeletedEventIDs {
			removal, exists := removals[id]
			if !exists || seen[id] {
				return nil, nil, errors.New("conflicting source changes")
			}
			seen[id] = true
			observation := RemovalObservation{Removal: removal, Unresolved: true}
			if previous, ok := byID[id]; ok {
				identity, err := Identity(source, previous)
				if err != nil {
					return nil, nil, err
				}
				observation.PreviousIdentity = identity
				if removal.Reason == calendar.EventSyncProviderCancellation {
					observation.Unresolved = !matchesCancelledInstance(source, previous, removal, identity)
				} else {
					observation.Unresolved = false
				}
			}
			delete(byID, id)
			observations = append(observations, observation)
		}
		for _, upsert := range page.Upserts {
			event := upsert.Event
			if event.ID == "" || event.CalendarID != source || upsert.Object.ObjectID != event.ID || event.Status == "cancelled" || event.InstanceKind == "cancelled" || seen[event.ID] {
				return nil, nil, errors.New("conflicting source changes")
			}
			if _, err := Identity(source, event); err != nil {
				return nil, nil, err
			}
			if event.InstanceKind == "seriesMaster" || len(event.Recurrence) != 0 {
				return nil, nil, errors.New("source projection requires expanded events")
			}
			seen[event.ID] = true
			byID[event.ID] = event
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	events := make([]calendar.EventV2, 0, len(ids))
	identities := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		event := byID[id]
		identity, err := Identity(source, event)
		if err != nil {
			return nil, nil, err
		}
		if event.InstanceKind == "seriesMaster" || len(event.Recurrence) != 0 {
			return nil, nil, errors.New("source projection requires expanded events")
		}
		if err := calendar.ValidateEventTimeRangeV2(event.Start, event.End); err != nil {
			return nil, nil, errors.New("invalid source event range")
		}
		if _, duplicate := identities[identity]; duplicate {
			return nil, nil, errors.New("duplicate occurrence identity")
		}
		identities[identity] = struct{}{}
		events = append(events, event)
	}
	selected, err := Candidates(source, events, windowStart, windowEnd, zone)
	if err != nil {
		return nil, nil, err
	}
	if len(selected) != len(events) {
		return nil, nil, errors.New("source event outside frozen window")
	}

	return events, observations, nil
}

// A provider cancellation tied to an exact, previously materialized
// standalone/expanded instance identifies that occurrence. This is source
// evidence only: it does not authorize a target deletion.
func matchesCancelledInstance(source string, previous calendar.EventV2, removal calendar.EventSyncRemoval, identity string) bool {
	if previous.ID != removal.ObjectID || previous.InstanceKind == "seriesMaster" || len(previous.Recurrence) != 0 || removal.RecurrencePresent {
		return false
	}
	if removal.SeriesID != "" && removal.SeriesID != previous.RecurringEventID {
		return false
	}
	if removal.OriginalStart != nil {
		if previous.OriginalStart == nil {
			return false
		}
		observed := previous
		observed.OriginalStart = removal.OriginalStart
		observedID, err := Identity(source, observed)
		if err != nil || observedID != identity {
			return false
		}
	}
	return true
}
