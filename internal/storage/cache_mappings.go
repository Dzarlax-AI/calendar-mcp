package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrCacheMappingConflict covers unavailable source/rule/snapshot, stale CAS,
// and conflicting target ownership without exposing identifiers.
var ErrCacheMappingConflict = errors.New("cache mapping unavailable or changed")

// CacheMapping is an isolated assertion, not proof of a provider result,
// ownership marker, target ETag, or permission to mutate a target. Callers
// must never use this unconnected storage helper as a substitute for the
// durable target operation outcome and lease protocol.
type CacheMapping struct {
	RuleID, IdentityHash, SourceCalendarID, TargetCalendarID string
	TargetEventID, OwnershipMarker, TargetETag, ContentHash  string
	MappingVersion, SourceRevision, RuleVersion              int64
	UpdatedAt                                                time.Time
}

// validGoogleStandaloneEventID accepts only Google's documented base32hex
// IDs supplied on insertion. This temporary fail-closed restriction rejects
// prefixed aliases and may reject server-generated IDs: their complete format
// has not been confirmed. Callers must park unsupported values.
func validGoogleStandaloneEventID(id string) bool {
	if len(id) < 5 || len(id) > 1024 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'v' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func validCacheMapping(m CacheMapping, expectedVersion int64) bool {
	validText := func(v string, max int) bool { return v != "" && len(v) <= max && !strings.ContainsRune(v, '\x00') }
	return validText(m.RuleID, 512) && targetDigest(m.IdentityHash) && validText(m.SourceCalendarID, 1024) && validText(m.TargetCalendarID, 1024) && m.SourceCalendarID != m.TargetCalendarID && validGoogleStandaloneEventID(m.TargetEventID) && validText(m.OwnershipMarker, 2048) && validText(m.TargetETag, 2048) && targetDigest(m.ContentHash) && m.SourceRevision > 0 && m.RuleVersion > 0 && m.MappingVersion == expectedVersion && !m.UpdatedAt.IsZero() && expectedVersion >= 0 && expectedVersion < int64(^uint64(0)>>1)
}

// PutCacheMappingAssertion inserts when expectedVersion=0 or updates the
// exact current version. It locks the calendars and current rule, and requires
// a retained source revision. Target ownership and marker are immutable; a
// conflicting target or stale version is rejected without overwriting either
// mapping. There is deliberately no delete, migration from legacy mappings,
// provider call, rule checkpoint advancement, or GC. A successful commit is
// not evidence a provider operation was performed or is safe to repeat.
// On an ambiguous commit GetCacheMapping is diagnostic only: a later update
// can supersede the record, so absence/current contents cannot prove outcome.
func (s *Store) PutCacheMappingAssertion(ctx context.Context, m CacheMapping, expectedVersion int64) error {
	if !validCacheMapping(m, expectedVersion) {
		return ErrCacheMappingConflict
	}
	ids, _, err := normalizeMutationCalendars([]string{m.SourceCalendarID, m.TargetCalendarID})
	if err != nil {
		return ErrCacheMappingConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cache mapping assertion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err = lockMutationCalendars(ctx, s, tx, ids); err != nil {
		return err
	}
	var targetProvider string
	providerQuery := `SELECT conn.provider FROM calendars c JOIN connections conn ON conn.id=c.connection_id WHERE c.id=?`
	if s.dialect == DialectPostgres {
		providerQuery += ` FOR NO KEY UPDATE OF conn`
	}
	err = tx.QueryRowContext(ctx, s.query(providerQuery), m.TargetCalendarID).Scan(&targetProvider)
	if errors.Is(err, sql.ErrNoRows) || err == nil && targetProvider != "google" {
		return ErrCacheMappingConflict
	}
	if err != nil {
		return fmt.Errorf("check cache mapping target provider: %w", err)
	}
	query := `SELECT source_calendar_id,target_calendar_id,config_version FROM sync_rules WHERE id=?`
	if s.dialect == DialectPostgres {
		query += ` FOR NO KEY UPDATE`
	}
	var source, target string
	var ruleVersion int64
	err = tx.QueryRowContext(ctx, s.query(query), m.RuleID).Scan(&source, &target, &ruleVersion)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (source != m.SourceCalendarID || target != m.TargetCalendarID || ruleVersion != m.RuleVersion) {
		return ErrCacheMappingConflict
	}
	if err != nil {
		return fmt.Errorf("check cache mapping rule: %w", err)
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, s.query(`SELECT EXISTS(SELECT 1 FROM projection_snapshots WHERE calendar_id=? AND revision=? AND verified=?)`), m.SourceCalendarID, m.SourceRevision, true).Scan(&exists); err != nil {
		return fmt.Errorf("check cache mapping revision: %w", err)
	}
	if !exists {
		return ErrCacheMappingConflict
	}
	var res sql.Result
	if expectedVersion == 0 {
		res, err = tx.ExecContext(ctx, s.query(`INSERT INTO cache_event_mappings(rule_id,identity_hash,mapping_version,source_calendar_id,source_revision,target_calendar_id,target_event_id,ownership_marker,target_etag,content_hash,rule_version,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`), m.RuleID, m.IdentityHash, 1, m.SourceCalendarID, m.SourceRevision, m.TargetCalendarID, m.TargetEventID, m.OwnershipMarker, m.TargetETag, m.ContentHash, m.RuleVersion, m.UpdatedAt.UTC())
	} else {
		res, err = tx.ExecContext(ctx, s.query(`UPDATE cache_event_mappings SET mapping_version=?,source_revision=?,target_etag=?,content_hash=?,updated_at=? WHERE rule_id=? AND identity_hash=? AND mapping_version=? AND source_calendar_id=? AND target_calendar_id=? AND target_event_id=? AND ownership_marker=? AND rule_version=? AND source_revision<=?`), expectedVersion+1, m.SourceRevision, m.TargetETag, m.ContentHash, m.UpdatedAt.UTC(), m.RuleID, m.IdentityHash, expectedVersion, m.SourceCalendarID, m.TargetCalendarID, m.TargetEventID, m.OwnershipMarker, m.RuleVersion, m.SourceRevision)
	}
	if err != nil {
		return fmt.Errorf("save cache mapping assertion: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrCacheMappingConflict
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit cache mapping assertion: %w", err)
	}
	return nil
}

// GetCacheMapping returns private diagnostic state, not authorization.
func (s *Store) GetCacheMapping(ctx context.Context, ruleID, identityHash string) (CacheMapping, error) {
	var m CacheMapping
	if ruleID == "" || len(ruleID) > 512 || strings.ContainsRune(ruleID, '\x00') || !targetDigest(identityHash) {
		return m, ErrCacheMappingConflict
	}
	m.RuleID, m.IdentityHash = ruleID, identityHash
	err := s.db.QueryRowContext(ctx, s.query(`SELECT mapping_version,source_calendar_id,source_revision,target_calendar_id,target_event_id,ownership_marker,target_etag,content_hash,rule_version,updated_at FROM cache_event_mappings WHERE rule_id=? AND identity_hash=?`), ruleID, identityHash).Scan(&m.MappingVersion, &m.SourceCalendarID, &m.SourceRevision, &m.TargetCalendarID, &m.TargetEventID, &m.OwnershipMarker, &m.TargetETag, &m.ContentHash, &m.RuleVersion, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CacheMapping{}, ErrNotFound
	}
	if err != nil {
		return CacheMapping{}, fmt.Errorf("read cache mapping: %w", err)
	}
	return m, nil
}
