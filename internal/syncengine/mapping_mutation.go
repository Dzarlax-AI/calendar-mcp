package syncengine

import (
	"context"
	"errors"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

var ErrPendingTargetMutation = errors.New("pending target mutation requires reconciliation")

func (e *Engine) beginMappingMutation(ctx context.Context, m storage.Mapping, kind, targetID string) (storage.Mapping, error) {
	if err := CheckDispatchFence(ctx, e.dispatchFence); err != nil {
		return m, err
	}
	if targetID == "" || (kind != "pending_update" && kind != "pending_delete") || (m.ReconciliationState != "current" && m.ReconciliationState != "legacy" && m.ReconciliationState != "" && m.ReconciliationState != "pending_create") {
		return m, ErrPendingTargetMutation
	}
	if m.Version > 0 && m.ReconciliationState != "pending_create" && m.TargetEventID != targetID {
		return m, ErrPendingTargetMutation
	}
	if m.Version == 0 {
		m.TargetEventID = targetID
		m.ReconciliationState = kind
		incarnation, err := e.mappings.InsertPendingMutationMapping(ctx, m)
		if err != nil {
			return m, err
		}
		if incarnation == "" {
			return m, ErrPendingTargetMutation
		}
		m.Version = 1
		m.Incarnation = incarnation
		return m, nil
	}
	oldVersion := m.Version
	m.TargetEventID = targetID
	m.ReconciliationState = kind
	if err := e.mappings.UpdateMappingCAS(ctx, m, oldVersion); err != nil {
		return m, err
	}
	m.Version++
	return m, nil
}
func (e *Engine) completeMappingMutation(ctx context.Context, pending, replacement storage.Mapping, result *calendar.OperationResult) error {
	if result == nil || result.Status != "completed" {
		return ErrPendingTargetMutation
	}
	if pending.ObjectKind == "cache_instance" {
		if !verifiedCacheTarget(result.Event, pending.TargetEventID, pending.RuleID, pending.SourceEventID) {
			return ErrCacheTargetUnverified
		}
		replacement.TargetETag = result.Event.ETag
	}
	replacement.Version = pending.Version
	replacement.Incarnation = pending.Incarnation
	replacement.ReconciliationState = "current"
	return e.mappings.UpdateMappingCAS(ctx, replacement, pending.Version)
}
func (e *Engine) completeMappingDelete(ctx context.Context, pending storage.Mapping, result *calendar.OperationResult) error {
	if result == nil || result.Status != "completed" {
		return ErrPendingTargetMutation
	}
	return e.mappings.DeleteMappingCAS(ctx, pending.ID, pending.Incarnation, pending.Version)
}
