package syncengine

import (
	"errors"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

var ErrCacheTargetUnverified = errors.New("cache target ownership or version is unverified")

// cacheTargetConditions requires the last confirmed target version before a
// cache copy can be modified. The provider must validate the marker and ETag
// together against its fresh target read before sending the mutation.
func cacheTargetConditions(m storage.Mapping) (string, *calendar.SyncMarker, error) {
	if m.ObjectKind != "cache_instance" {
		return "", nil, nil
	}
	if m.RuleID == "" || m.SourceEventID == "" || m.TargetETag == "" {
		return "", nil, ErrCacheTargetUnverified
	}
	return m.TargetETag, &calendar.SyncMarker{RuleID: m.RuleID, SourceEventID: m.SourceEventID}, nil
}

func verifiedCacheTarget(event *calendar.EventV2, targetID, ruleID, sourceID string) bool {
	if event == nil || event.ID == "" || targetID != "" && event.ID != targetID || event.ETag == "" || event.Google == nil || event.Google.PrivateProperties == nil {
		return false
	}
	properties := event.Google.PrivateProperties
	return properties["calendar_sync_rule"] == ruleID && properties["calendar_source_event"] == sourceID
}
