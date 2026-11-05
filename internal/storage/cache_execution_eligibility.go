package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrCacheExecutionIneligible deliberately does not disclose calendar IDs.
var ErrCacheExecutionIneligible = errors.New("cache synchronization target is unavailable or conflicts with another rule")

// CheckSyncTargetEligibility is a point-in-time configuration check. Callers
// must serialize all sync runs before checking and recheck before every target
// mutation. It is not a provider ownership check or a proof that distinct
// opaque provider IDs cannot be aliases. Missing identities fail closed.
func (s *Store) CheckSyncTargetEligibility(ctx context.Context, ruleID string) error {
	return s.checkSyncTargetEligibility(ctx, s.db, ruleID)
}

type cacheEligibilityQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Store) checkSyncTargetEligibility(ctx context.Context, db cacheEligibilityQueryer, ruleID string) error {
	if ruleID == "" {
		return ErrCacheExecutionIneligible
	}
	var mode, sourceID, targetID, sourceProvider, targetProvider, sourceState, targetState, sourceConn, targetConn string
	var sourceEnabled, targetEnabled, readable, writable bool
	err := db.QueryRowContext(ctx, s.query(`SELECT r.sync_mode,sc.provider_calendar_id,tc.provider_calendar_id,sconn.provider,tconn.provider,sconn.status,tconn.status,sc.connection_id,tc.connection_id,sc.enabled,tc.enabled,sc.can_read,tc.can_write FROM sync_rules r JOIN calendars sc ON sc.id=r.source_calendar_id JOIN calendars tc ON tc.id=r.target_calendar_id JOIN connections sconn ON sconn.id=sc.connection_id JOIN connections tconn ON tconn.id=tc.connection_id WHERE r.id=?`), ruleID).Scan(&mode, &sourceID, &targetID, &sourceProvider, &targetProvider, &sourceState, &targetState, &sourceConn, &targetConn, &sourceEnabled, &targetEnabled, &readable, &writable)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCacheExecutionIneligible
	}
	if err != nil {
		return fmt.Errorf("check sync target eligibility: %w", err)
	}
	if mode != "direct" && mode != "cached_instances" {
		return ErrCacheExecutionIneligible
	}
	if mode == "cached_instances" {
		if sourceProvider != "google" || targetProvider != "google" || sourceState != "connected" || targetState != "connected" || !sourceEnabled || !targetEnabled || !readable || !writable || invalidPhysicalID(sourceID) || invalidPhysicalID(targetID) || sourceID == targetID {
			return ErrCacheExecutionIneligible
		}
		var badMapping bool
		if err = db.QueryRowContext(ctx, s.query(`SELECT EXISTS(SELECT 1 FROM event_mappings WHERE rule_id=? AND object_kind<>?)`), ruleID, "cache_instance").Scan(&badMapping); err != nil {
			return err
		}
		if badMapping {
			return ErrCacheExecutionIneligible
		}
	}
	// Direct rules only require this stricter check when a cache rule could own
	// the same physical target. Unresolved primary IDs remain ambiguous.
	rows, err := db.QueryContext(ctx, s.query(`SELECT r.sync_mode,r.state,c.provider_calendar_id,conn.provider,sc.provider_calendar_id,sconn.provider,EXISTS(SELECT 1 FROM event_mappings m WHERE m.rule_id=r.id),EXISTS(SELECT 1 FROM cache_event_mappings cm WHERE cm.rule_id=r.id) FROM sync_rules r JOIN calendars c ON c.id=r.target_calendar_id JOIN connections conn ON conn.id=c.connection_id JOIN calendars sc ON sc.id=r.source_calendar_id JOIN connections sconn ON sconn.id=sc.connection_id WHERE r.id<>?`), ruleID)
	if err != nil {
		return err
	}
	defer rows.Close()
	type route struct{ source, target, sourceProvider, targetProvider string }
	routes := []route{{sourceID, targetID, sourceProvider, targetProvider}}
	hasCacheRule := mode == "cached_instances"
	for rows.Next() {
		var otherMode, state, otherID, provider, otherSource, otherSourceProvider string
		var hasLegacy, hasCache bool
		if err = rows.Scan(&otherMode, &state, &otherID, &provider, &otherSource, &otherSourceProvider, &hasLegacy, &hasCache); err != nil {
			return err
		}
		relevant := state == "enabled" || hasLegacy || hasCache || (mode == "direct" && otherMode == "cached_instances")
		if !relevant {
			continue
		}
		if otherMode == "cached_instances" {
			hasCacheRule = true
		}
		routes = append(routes, route{otherSource, otherID, otherSourceProvider, provider})
		if targetProvider == "google" && provider == "google" && (mode == "cached_instances" || otherMode == "cached_instances") && (invalidPhysicalID(targetID) || invalidPhysicalID(otherID) || targetID == otherID) {
			return ErrCacheExecutionIneligible
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if !hasCacheRule {
		return nil
	}
	graph := make(map[string][]string)
	for _, r := range routes {
		if r.sourceProvider == "google" && invalidPhysicalID(r.source) || r.targetProvider == "google" && invalidPhysicalID(r.target) {
			return ErrCacheExecutionIneligible
		}
		from, to := r.sourceProvider+"\x00"+r.source, r.targetProvider+"\x00"+r.target
		graph[from] = append(graph[from], to)
	}
	visited, active := map[string]bool{}, map[string]bool{}
	var cycle func(string) bool
	cycle = func(node string) bool {
		if active[node] {
			return true
		}
		if visited[node] {
			return false
		}
		active[node] = true
		for _, next := range graph[node] {
			if cycle(next) {
				return true
			}
		}
		delete(active, node)
		visited[node] = true
		return false
	}
	for node := range graph {
		if cycle(node) {
			return ErrCacheExecutionIneligible
		}
	}
	return nil
}
func invalidPhysicalID(id string) bool {
	return id == "" || strings.TrimSpace(id) != id || strings.EqualFold(id, "primary") || strings.ContainsRune(id, '\x00')
}
