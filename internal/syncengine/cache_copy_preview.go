package syncengine

import (
	"context"
	"errors"
	"time"

	"calendar-mcp/internal/storage"
)

// cachePreviewReader is deliberately read-only: it has no provider or mutation
// access. Its result is only a point-in-time source observation.
type cachePreviewReader interface {
	ReadProjectionPreviewSnapshot(context.Context, string, time.Time, time.Time, time.Time, time.Duration) (storage.ProjectionPreviewSnapshot, error)
}

type cachePreview struct {
	Revision int64
	Result   Result
}

// PreviewCacheCopy reads only a local snapshot. Destination state is not
// checked; the result cannot authorize rule enablement or any provider write.
func PreviewCacheCopy(ctx context.Context, reader cachePreviewReader, rule storage.Rule, now time.Time, maxAge time.Duration) (Result, error) {
	start := now.AddDate(0, 0, -rule.LookbackDays)
	end := now.AddDate(0, 0, rule.LookaheadDays)
	preview, err := previewCacheCopy(ctx, reader, rule, start, end, now, maxAge)
	return preview.Result, err
}

// previewCacheCopy is an unconnected diagnostic preview. It does not inspect
// target inventory or infer creates, updates, deletions, ownership, or safety
// to dispatch. Neither the API nor the current worker calls it.
func previewCacheCopy(ctx context.Context, reader cachePreviewReader, rule storage.Rule, start, end, now time.Time, maxAge time.Duration) (cachePreview, error) {
	var out cachePreview
	if reader == nil || rule.ID == "" || rule.SourceCalendarID == "" || rule.TargetCalendarID == "" || rule.SourceCalendarID == rule.TargetCalendarID || start.IsZero() || !end.After(start) || now.IsZero() || maxAge <= 0 {
		return out, errors.New("invalid cache preview request")
	}
	snapshot, err := reader.ReadProjectionPreviewSnapshot(ctx, rule.SourceCalendarID, start, end, now, maxAge)
	if err != nil {
		return out, err
	}
	if snapshot.Revision < 1 || snapshot.Timezone == "" || snapshot.Timezone == "Local" {
		return out, storage.ErrProjectionSnapshotUnavailable
	}
	zone, err := time.LoadLocation(snapshot.Timezone)
	if err != nil {
		return out, storage.ErrProjectionSnapshotUnavailable
	}
	candidates, err := cacheCopyCandidates(rule.SourceCalendarID, snapshot.Events, start, end, zone)
	if err != nil {
		return out, err
	}
	result := Result{Preview: &storage.SyncRunPreview{Entries: []storage.SyncRunPreviewEntry{}, SourceRevision: snapshot.Revision, TargetStatus: "not_checked"}}
	for _, candidate := range candidates {
		if _, err := cacheCopyCreate(rule.ID, rule.SourceCalendarID, candidate.Event, "google"); err != nil {
			return out, err
		}
		if !candidate.Event.SourceAttendeesComplete {
			result.ParticipantsIncomplete++
			result.Warnings++
		}
		previewAction(result.Preview, candidate.Event, "candidate", "target_not_checked", "not_checked")
	}
	return cachePreview{Revision: snapshot.Revision, Result: result}, nil
}
