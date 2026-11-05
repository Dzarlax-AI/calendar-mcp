package syncengine

import (
	"context"
	"errors"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/calendarprojection"
	"calendar-mcp/internal/storage"
)

// cacheExecutionBatch is private input, not source authorization. The caller
// must establish verified freshness and cumulative cancellation history.
type cacheExecutionBatch struct {
	Events                 []calendar.EventV2
	Removals               []calendarprojection.RemovalObservation
	Timezone               string
	WindowStart, WindowEnd time.Time
	HistoryComplete        bool
}

// runCachedBatch feeds stable standalone instances into the existing engine.
func (e *Engine) runCachedBatch(ctx context.Context, rule storage.Rule, batch cacheExecutionBatch, dryRun bool) (Result, error) {
	if rule.SyncMode != "cached_instances" || !batch.HistoryComplete || batch.Timezone == "" || batch.Timezone == "Local" {
		return Result{}, storage.ErrProjectionSnapshotUnavailable
	}
	zone, err := time.LoadLocation(batch.Timezone)
	if err != nil {
		return Result{}, storage.ErrProjectionSnapshotUnavailable
	}
	candidates, err := cacheCopyCandidates(rule.SourceCalendarID, batch.Events, batch.WindowStart, batch.WindowEnd, zone)
	if err != nil {
		return Result{}, err
	}
	events := make([]calendar.EventV2, 0, len(candidates)+len(batch.Removals))
	seen := make(map[string]bool, len(candidates)+len(batch.Removals))
	for _, candidate := range candidates {
		id := cacheExecutionEventID(candidate.Identity)
		if seen[id] {
			return Result{}, storage.ErrProjectionSnapshotUnavailable
		}
		seen[id] = true
		event := candidate.Event
		event.ID = id
		event.ICalUID = id + "@calendar-mcp"
		event.RecurringEventID = ""
		event.OriginalStart = nil
		event.InstanceKind = "cache_instance"
		event.Recurrence = nil
		events = append(events, event)
	}
	cancelled := make(map[string]bool)
	unresolved := 0
	for _, observation := range batch.Removals {
		if observation.Removal.Reason == calendar.EventSyncWindowEviction {
			continue
		}
		if observation.Removal.Reason != calendar.EventSyncProviderCancellation {
			return Result{}, storage.ErrProjectionSnapshotUnavailable
		}
		if observation.Unresolved {
			// A sparse cancellation does not authorize any target deletion. Keep
			// it in the source journal for later reconciliation, but let other
			// independent events continue to synchronize.
			unresolved++
			continue
		}
		identity := observation.PreviousIdentity
		if !strings.HasPrefix(identity, rule.SourceCalendarID+"\x00") || len(identity) <= len(rule.SourceCalendarID)+1 {
			return Result{}, errors.New("invalid cached cancellation identity")
		}
		id := cacheExecutionEventID(identity)
		if cancelled[id] {
			continue
		}
		if seen[id] {
			// A later active instance supersedes an old cancellation for this
			// run. The durable tombstone remains; e.run checks all pending
			// target mutations before any write, and the global reservation
			// prevents another in-flight sync Run from racing this decision.
			continue
		}
		seen[id] = true
		cancelled[id] = true
		events = append(events, calendar.EventV2{CalendarID: rule.SourceCalendarID, ID: id, Status: "cancelled", InstanceKind: "cache_instance"})
	}
	result, err := e.run(ctx, rule, dryRun, &events)
	result.Warnings += unresolved
	return result, err
}

func cacheExecutionEventID(identity string) string {
	return newMappingID("cache-instance", identity, "")
}

type CacheCopyExecutionSource interface {
	GetSourceProjectionForCopy(context.Context, string, time.Time, time.Time, time.Time, time.Duration) (storage.SourceProjectionForCopy, error)
	CheckSourceProjectionCurrent(context.Context, string, int64, time.Time, time.Time, time.Time, time.Duration) error
	CheckSyncTargetEligibility(context.Context, string) error
}

type cacheCopyDispatchFence struct {
	parent           DispatchFence
	source           CacheCopyExecutionSource
	ruleID, sourceID string
	revision         int64
	start, end       time.Time
	maxAge           time.Duration
}

func (f cacheCopyDispatchFence) CheckProviderDispatch(ctx context.Context) error {
	if err := CheckDispatchFence(ctx, f.parent); err != nil {
		return err
	}
	if err := f.source.CheckSyncTargetEligibility(ctx, f.ruleID); err != nil {
		return err
	}
	return f.source.CheckSourceProjectionCurrent(ctx, f.sourceID, f.revision, f.start, f.end, time.Now().UTC(), f.maxAge)
}

// RunCacheCopy reads the verified local source exactly once and rechecks its
// revision and target eligibility before every provider mutation. It does not
// read the source calendar from a provider. The caller must hold the shared
// global sync-run reservation and install a valid job-attempt fence.
func (e *Engine) RunCacheCopy(ctx context.Context, source CacheCopyExecutionSource, rule storage.Rule, now time.Time, maxAge time.Duration) (Result, error) {
	if source == nil || rule.SyncMode != "cached_instances" || now.IsZero() || maxAge <= 0 {
		return Result{}, storage.ErrProjectionSnapshotUnavailable
	}
	if err := source.CheckSyncTargetEligibility(ctx, rule.ID); err != nil {
		return Result{}, err
	}
	start := now.AddDate(0, 0, -rule.LookbackDays)
	end := now.AddDate(0, 0, rule.LookaheadDays)
	snapshot, err := source.GetSourceProjectionForCopy(ctx, rule.SourceCalendarID, start, end, now, maxAge)
	if err != nil {
		return Result{}, err
	}
	if !snapshot.Snapshot.Verified || snapshot.Snapshot.Revision < 1 || snapshot.Snapshot.CalendarID != rule.SourceCalendarID {
		return Result{}, storage.ErrProjectionSnapshotUnavailable
	}
	parent := e.dispatchFence
	e.dispatchFence = cacheCopyDispatchFence{parent: parent, source: source, ruleID: rule.ID, sourceID: rule.SourceCalendarID, revision: snapshot.Snapshot.Revision, start: start, end: end, maxAge: maxAge}
	defer func() { e.dispatchFence = parent }()
	batch := cacheExecutionBatch{Events: snapshot.Events, Removals: snapshot.Observations, Timezone: snapshot.Snapshot.Timezone, WindowStart: start, WindowEnd: end, HistoryComplete: true}
	return e.runCachedBatch(ctx, rule, batch, false)
}
