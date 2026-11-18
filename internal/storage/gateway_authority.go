package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"calendar-mcp/internal/calendar"
)

const (
	GatewayAuthorityInterval = 5 * time.Minute
	GatewayAuthorityLifetime = 15 * time.Minute
)

type GatewayAccessClass string

const (
	GatewayMetadataOnly  GatewayAccessClass = "metadata"
	GatewayEventAccess   GatewayAccessClass = "events"
	GatewayPrivateAccess GatewayAccessClass = "private"
)

// GatewayCalendarProof is one result in a complete account inventory. The
// detail receipt carries the current local source binding from before I/O.
type GatewayCalendarProof struct {
	Metadata      calendar.Calendar
	Capabilities  calendar.CalendarCapabilities
	DetailReceipt string
	Class         GatewayAccessClass
	Unknown       bool
	GoogleRole    string
}

type GatewayAuthorityLease struct {
	ConnectionID, Provider, AuthorizationRevision, ID string
	StartedAt                                         time.Time
}

type gatewayProofRow struct {
	revision, receipt, class, metadata, capabilities string
	until                                            int64
}

// DueGatewayAuthorityChecks orders unverified and expired connections first.
func (s *Store) DueGatewayAuthorityChecks(ctx context.Context, now time.Time, limit int) ([]string, error) {
	if limit < 1 || limit > 256 {
		return nil, fmt.Errorf("invalid gateway authority batch size")
	}
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT c.id FROM connections c
	LEFT JOIN gateway_account_checks g ON g.connection_id=c.id AND g.authorization_revision=c.authorization_revision
	WHERE c.status='connected' AND c.authorization_revision<>'' AND
	COALESCE(g.next_check_ms,0)<=? AND COALESCE(g.lease_until_ms,0)<=?
	ORDER BY COALESCE(g.next_check_ms,0),c.id LIMIT ?`), now.UnixMilli(), now.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) ClaimGatewayAuthorityCheck(ctx context.Context, connectionID string, now time.Time, timeout time.Duration) (GatewayAuthorityLease, error) {
	if timeout <= 0 || timeout > GatewayAuthorityInterval {
		return GatewayAuthorityLease{}, fmt.Errorf("invalid gateway authority lease duration")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GatewayAuthorityLease{}, err
	}
	defer tx.Rollback()
	lease := GatewayAuthorityLease{ConnectionID: connectionID, ID: uuid.NewString(), StartedAt: now}
	if err := s.lockGatewayConnection(ctx, tx, &lease); err != nil {
		return GatewayAuthorityLease{}, err
	}
	// Old source proofs must never cross a connection authorization epoch.
	if _, err := tx.ExecContext(ctx, s.query(`DELETE FROM gateway_calendar_grants WHERE connection_id=? AND EXISTS (SELECT 1 FROM gateway_account_checks WHERE connection_id=? AND authorization_revision<>?)`), connectionID, connectionID, lease.AuthorizationRevision); err != nil {
		return GatewayAuthorityLease{}, err
	}
	res, err := tx.ExecContext(ctx, s.query(`INSERT INTO gateway_account_checks
	(connection_id, authorization_revision, lease_id, lease_started_ms, lease_until_ms) VALUES (?,?,?,?,?)
	ON CONFLICT(connection_id) DO UPDATE SET
	lease_id=excluded.lease_id,lease_started_ms=excluded.lease_started_ms,lease_until_ms=excluded.lease_until_ms,
	next_check_ms=CASE WHEN gateway_account_checks.authorization_revision=excluded.authorization_revision THEN gateway_account_checks.next_check_ms ELSE 0 END,
	verified_until_ms=CASE WHEN gateway_account_checks.authorization_revision=excluded.authorization_revision THEN gateway_account_checks.verified_until_ms ELSE 0 END,
	inventory_revision=CASE WHEN gateway_account_checks.authorization_revision=excluded.authorization_revision THEN gateway_account_checks.inventory_revision ELSE '' END,
	authorization_revision=excluded.authorization_revision
	WHERE gateway_account_checks.authorization_revision<>excluded.authorization_revision
	OR (gateway_account_checks.lease_until_ms<=? AND gateway_account_checks.next_check_ms<=?)`),
		connectionID, lease.AuthorizationRevision, lease.ID, now.UnixMilli(), now.Add(timeout).UnixMilli(), now.UnixMilli(), now.UnixMilli())
	if err := oneGatewayAuthorityRow(res, err); err != nil {
		return GatewayAuthorityLease{}, err
	}
	if err := tx.Commit(); err != nil {
		return GatewayAuthorityLease{}, err
	}
	return lease, nil
}

// PublishGatewayAuthority commits only a complete inventory under the claim
// and connection epoch which preceded provider I/O. An omitted calendar loses
// its grant. Unchanged timely renewal preserves each publication revision.
func (s *Store) PublishGatewayAuthority(ctx context.Context, lease GatewayAuthorityLease, proofs []GatewayCalendarProof, now time.Time) error {
	if len(proofs) > 256 || now.Before(lease.StartedAt) || lease.ID == "" || lease.AuthorizationRevision == "" {
		return ErrAuthorityChanged
	}
	publicationStarted := time.Now()
	rows, err := gatewayProofRows(lease.Provider, proofs)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.lockGatewayConnection(ctx, tx, &lease); err != nil {
		return err
	}

	var priorUntil, leaseUntil int64
	var inventory string
	err = tx.QueryRowContext(ctx, s.query(`SELECT verified_until_ms,inventory_revision,lease_until_ms FROM gateway_account_checks
	WHERE connection_id=? AND authorization_revision=? AND lease_id=? AND lease_started_ms=? AND lease_until_ms>?`),
		lease.ConnectionID, lease.AuthorizationRevision, lease.ID, lease.StartedAt.UnixMilli(), now.Add(time.Since(publicationStarted)).UnixMilli()).Scan(&priorUntil, &inventory, &leaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAuthorityChanged
	}
	if err != nil {
		return err
	}
	old, err := s.gatewayProofsForAccount(ctx, tx, lease.ConnectionID)
	if err != nil {
		return err
	}
	if priorUntil > 0 && lease.StartedAt.UnixMilli() < priorUntil {
		priorUntil = max(priorUntil, min(leaseUntil, priorUntil+60000))
	}
	changed := inventory == "" || priorUntil <= now.UnixMilli()
	ordered := append([]GatewayCalendarProof(nil), proofs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Metadata.ID < ordered[j].Metadata.ID })
	for _, evidence := range ordered {
		id := evidence.Metadata.ID
		proof := rows[id]
		if evidence.Unknown {
			if priorUntil > 0 {
				delete(old, id)
			}
			changed = true
			continue
		}
		proof.receipt, err = s.publishGatewayCalendarBinding(ctx, tx, lease, evidence, old[id], now)
		if err != nil {
			return err
		}
		previous := old[id]
		delete(old, id)
		proof.revision = previous.revision
		proof.until = previous.until
		previousDeadline := previous.until
		if previous.until > 0 && lease.StartedAt.UnixMilli() < previous.until {
			previousDeadline = max(previous.until, min(leaseUntil, previous.until+60000))
		}
		if previousDeadline <= now.UnixMilli() || proof.revision == "" || proof != previous {
			proof.revision = uuid.NewString()
			changed = true
		}
		_, err = tx.ExecContext(ctx, s.query(`INSERT INTO gateway_calendar_grants
		(calendar_id,connection_id,grant_revision,verified_until_ms,detail_receipt,access_class,metadata_json,capabilities_json)
		VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(calendar_id) DO UPDATE SET
		grant_revision=excluded.grant_revision, verified_until_ms=excluded.verified_until_ms, detail_receipt=excluded.detail_receipt,access_class=excluded.access_class,
		metadata_json=excluded.metadata_json,capabilities_json=excluded.capabilities_json`),
			id, lease.ConnectionID, proof.revision, lease.StartedAt.Add(GatewayAuthorityLifetime).UnixMilli(), proof.receipt, proof.class, proof.metadata, proof.capabilities)
		if err != nil {
			return err
		}
		if evidence.Class != GatewayMetadataOnly {
			if _, err := tx.ExecContext(ctx, s.query(s.eventSyncTimeComparisons(`UPDATE calendar_sync_state
			SET next_sync_at=? WHERE calendar_id=? AND next_sync_at>? AND (lease_until IS NULL OR lease_until<=?)
			AND EXISTS(SELECT 1 FROM calendars c WHERE c.id=calendar_sync_state.calendar_id AND c.enabled=TRUE AND c.can_read=TRUE)`)),
				now, id, now, now); err != nil {
				return err
			}
		}
	}

	for id := range old {
		if _, err := tx.ExecContext(ctx, s.query(`DELETE FROM gateway_calendar_grants WHERE connection_id=? AND calendar_id=?`), lease.ConnectionID, id); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		inventory = uuid.NewString()
	}
	for _, proof := range proofs {
		if proof.Unknown {
			inventory = ""
			break
		}
	}
	res, err := tx.ExecContext(ctx, s.query(`UPDATE gateway_account_checks SET lease_id='',lease_started_ms=0,lease_until_ms=0,
		next_check_ms=?,verified_until_ms=?,inventory_revision=? WHERE connection_id=? AND lease_id=? AND lease_until_ms>?`),
		lease.StartedAt.Add(GatewayAuthorityInterval).UnixMilli(), lease.StartedAt.Add(GatewayAuthorityLifetime).UnixMilli(), inventory,
		lease.ConnectionID, lease.ID, now.Add(time.Since(publicationStarted)).UnixMilli())
	if err := oneGatewayAuthorityRow(res, err); err != nil {
		return err
	}
	return tx.Commit()
}

func gatewayProofRows(provider string, proofs []GatewayCalendarProof) (map[string]gatewayProofRow, error) {
	rows := make(map[string]gatewayProofRow, len(proofs))
	for _, proof := range proofs {
		id := proof.Metadata.ID
		if id == "" || proof.Metadata.Provider != provider || proof.Capabilities.CalendarID != id || proof.Capabilities.Provider != provider {
			return nil, fmt.Errorf("invalid gateway calendar proof identity")
		}
		if proof.Unknown {
			if _, found := rows[id]; found {
				return nil, fmt.Errorf("duplicate gateway calendar proof")
			}
			rows[id] = gatewayProofRow{}
			continue
		}
		if provider == "google" && !validGatewayGoogleRole(proof.GoogleRole, proof.Class) {
			return nil, fmt.Errorf("invalid gateway Google authorization role")
		}
		if _, found := rows[id]; found {
			return nil, fmt.Errorf("duplicate gateway calendar proof")
		}
		switch proof.Class {
		case GatewayMetadataOnly, GatewayEventAccess, GatewayPrivateAccess:
		default:
			return nil, fmt.Errorf("invalid gateway visibility evidence")
		}
		metadata, err := json.Marshal(proof.Metadata)
		if err != nil {
			return nil, err
		}
		caps, err := json.Marshal(proof.Capabilities)
		if err != nil {
			return nil, err
		}
		if len(metadata)+len(caps) > 16<<10 {
			return nil, fmt.Errorf("gateway calendar proof exceeds size limit")
		}
		rows[id] = gatewayProofRow{receipt: proof.DetailReceipt, class: string(proof.Class), metadata: string(metadata), capabilities: string(caps)}
	}
	return rows, nil
}

func (s *Store) gatewayProofsForAccount(ctx context.Context, tx *sql.Tx, connectionID string) (map[string]gatewayProofRow, error) {
	rows, err := tx.QueryContext(ctx, s.query(`SELECT calendar_id,grant_revision,detail_receipt,access_class,metadata_json,capabilities_json,verified_until_ms
	FROM gateway_calendar_grants WHERE connection_id=?`), connectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	proofs := map[string]gatewayProofRow{}
	for rows.Next() {
		var id string
		var proof gatewayProofRow
		if err := rows.Scan(&id, &proof.revision, &proof.receipt, &proof.class, &proof.metadata, &proof.capabilities, &proof.until); err != nil {
			return nil, err
		}
		proofs[id] = proof
	}
	return proofs, rows.Err()
}

// A write statement locks the connection before any account or calendar proof
// on both dialects. OAuth refresh can change ciphertext without changing epoch.
func (s *Store) lockGatewayConnection(ctx context.Context, tx *sql.Tx, lease *GatewayAuthorityLease) error {
	res, err := tx.ExecContext(ctx, s.query(`UPDATE connections SET updated_at=updated_at WHERE id=? AND status='connected'`), lease.ConnectionID)
	if err := oneGatewayAuthorityRow(res, err); err != nil {
		return err
	}
	var provider, revision string
	err = tx.QueryRowContext(ctx, s.query(`SELECT provider,authorization_revision FROM connections WHERE id=?`), lease.ConnectionID).Scan(&provider, &revision)
	if err != nil {
		return err
	}
	if revision == "" || lease.AuthorizationRevision != "" && (lease.AuthorizationRevision != revision || lease.Provider != provider) {
		return ErrAuthorityChanged
	}
	lease.Provider, lease.AuthorizationRevision = provider, revision
	return nil
}

func oneGatewayAuthorityRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrAuthorityChanged
	}
	return nil
}

type GatewayAuthorityDenial struct {
	CredentialDigest [32]byte
}

func (*GatewayAuthorityDenial) Error() string { return "gateway account access denied" }

// FailGatewayAuthorityCheck releases a failed claim without extending its
// proof. A definitive account denial invalidates the inventory immediately.
func (s *Store) FailGatewayAuthorityCheck(ctx context.Context, lease GatewayAuthorityLease, denial *GatewayAuthorityDenial, now time.Time) error {
	if lease.ID == "" || lease.AuthorizationRevision == "" || now.Before(lease.StartedAt) {
		return ErrAuthorityChanged
	}
	releaseStarted := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.lockGatewayConnection(ctx, tx, &lease); err != nil {
		return err
	}
	if denial != nil {
		var credential []byte
		err := tx.QueryRowContext(ctx, s.query(`SELECT encrypted_credentials FROM connections WHERE id=? AND credential_is_bounded`), lease.ConnectionID).Scan(&credential)
		if errors.Is(err, sql.ErrNoRows) || err == nil && sha256.Sum256(credential) != denial.CredentialDigest {
			return ErrAuthorityChanged
		}
		if err != nil {
			return err
		}
	}

	res, err := tx.ExecContext(ctx, s.query(`UPDATE gateway_account_checks SET lease_id='',lease_started_ms=0,lease_until_ms=0,next_check_ms=?,
	verified_until_ms=CASE WHEN ? THEN 0 ELSE verified_until_ms END
	WHERE connection_id=? AND authorization_revision=? AND lease_id=? AND lease_started_ms=? AND lease_until_ms>?`),
		now.Add(time.Minute).UnixMilli(), denial != nil, lease.ConnectionID, lease.AuthorizationRevision, lease.ID, lease.StartedAt.UnixMilli(), now.Add(time.Since(releaseStarted)).UnixMilli())
	if err := oneGatewayAuthorityRow(res, err); err != nil {
		return err
	}
	if denial != nil {
		if _, err := tx.ExecContext(ctx, s.query(`UPDATE gateway_calendar_grants SET verified_until_ms=0 WHERE connection_id=?`), lease.ConnectionID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Only a check already running at proof expiry can bridge the expiry. A
// released or superseded lease, and a confirmed denial, cannot grant grace.
const gatewayProofDeadline = `CASE WHEN a.verified_until_ms>0 AND a.lease_id<>'' AND a.lease_started_ms<a.verified_until_ms
	AND a.lease_until_ms>a.verified_until_ms
	THEN CASE WHEN a.lease_until_ms<a.verified_until_ms+60000 THEN a.lease_until_ms ELSE a.verified_until_ms+60000 END
	ELSE a.verified_until_ms END`

const gatewaySourceDeadline = `CASE WHEN g.verified_until_ms>0 AND a.lease_id<>'' AND a.lease_started_ms<g.verified_until_ms
 AND a.lease_until_ms>g.verified_until_ms THEN CASE WHEN a.lease_until_ms<g.verified_until_ms+60000 THEN a.lease_until_ms ELSE g.verified_until_ms+60000 END ELSE g.verified_until_ms END`

type GatewayCalendarGrant struct {
	Metadata           calendar.Calendar
	Capabilities       calendar.CalendarCapabilities
	Class              GatewayAccessClass
	Revision           string
	ConnectionRevision string
	DetailReceipt      string
	VerifiedUntil      time.Time
	HardVerifiedUntil  time.Time
}

// GatewayCalendarInventory returns an all-or-nothing public calendar snapshot.
// Existing account selection and receipt fences take effect immediately.
func (s *Store) GatewayCalendarInventory(ctx context.Context, now time.Time) ([]GatewayCalendarGrant, error) {
	tx, err := s.db.BeginTx(ctx, cachedPageReadTxOptions(s.dialect))
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var missing bool
	err = tx.QueryRowContext(ctx, s.query(`SELECT EXISTS (SELECT 1 FROM connections c
	LEFT JOIN gateway_account_checks a ON a.connection_id=c.id AND a.authorization_revision=c.authorization_revision
	WHERE c.status='connected' AND (a.connection_id IS NULL OR a.inventory_revision='' OR (`+gatewayProofDeadline+`)<=?))
	OR EXISTS (SELECT 1 FROM gateway_calendar_grants g JOIN calendars cal ON cal.id=g.calendar_id
	JOIN connections c ON c.id=cal.connection_id WHERE c.status='connected' AND cal.enabled=TRUE AND g.detail_receipt<>cal.detail_receipt)`),
		now.UnixMilli()).Scan(&missing)
	if err != nil {
		return nil, err
	}
	if missing {
		return nil, ErrAuthorityChanged
	}
	grants, err := s.gatewayCalendarGrants(ctx, tx, now, "", false)
	if err != nil {
		return nil, err
	}
	return grants, tx.Commit()
}

// GatewayCalendarGrant loads only an authorized local proof. Unknown,
// disabled, expired, and revoked IDs have the same outcome.
func (s *Store) GatewayCalendarGrant(ctx context.Context, id string, now time.Time) (GatewayCalendarGrant, error) {
	if id == "" {
		return GatewayCalendarGrant{}, ErrAuthorityChanged
	}
	grants, err := s.gatewayCalendarGrants(ctx, s.db, now, id, false)
	if err != nil {
		return GatewayCalendarGrant{}, err
	}
	if len(grants) != 1 {
		return GatewayCalendarGrant{}, ErrAuthorityChanged
	}
	return grants[0], nil
}

// GatewayCalendarGrantByReference resolves a local legacy alias only if it
// selects one currently authorized calendar. No provider is consulted.
func (s *Store) GatewayCalendarGrantByReference(ctx context.Context, reference string, now time.Time) (GatewayCalendarGrant, error) {
	if reference == "" {
		return GatewayCalendarGrant{}, ErrAuthorityChanged
	}
	exact, err := s.GatewayCalendarGrant(ctx, reference, now)
	if err == nil || !errors.Is(err, ErrAuthorityChanged) {
		return exact, err
	}
	grants, err := s.gatewayCalendarGrants(ctx, s.db, now, reference, true)
	if err != nil {
		return GatewayCalendarGrant{}, err
	}
	if len(grants) != 1 {
		return GatewayCalendarGrant{}, ErrAuthorityChanged
	}
	return grants[0], nil
}

func (s *Store) gatewayCalendarGrants(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, now time.Time, id string, aliases bool) ([]GatewayCalendarGrant, error) {
	query := `SELECT g.metadata_json,g.capabilities_json,g.access_class,g.grant_revision,
	c.authorization_revision,g.detail_receipt,(` + gatewaySourceDeadline + `),g.verified_until_ms
	FROM gateway_calendar_grants g
	JOIN calendars cal ON cal.id=g.calendar_id AND cal.connection_id=g.connection_id
	JOIN connections c ON c.id=g.connection_id
	JOIN gateway_account_checks a ON a.connection_id=c.id AND a.authorization_revision=c.authorization_revision
	WHERE c.status='connected' AND cal.enabled=TRUE AND cal.detail_receipt=g.detail_receipt
	AND (` + gatewaySourceDeadline + `)>? AND a.verified_until_ms>0`
	args := []any{now.UnixMilli()}
	if id != "" {
		if aliases {
			query += ` AND (cal.id=? OR (c.provider || ':' || cal.provider_calendar_id)=?
			OR (c.provider='google' AND cal.google_primary=TRUE AND (?='google:primary' OR ?=('google:' || c.id || ':primary'))))`
			args = append(args, id, id, id, id)
		} else {
			query += ` AND cal.id=?`
			args = append(args, id)
		}
	}
	query += ` ORDER BY cal.id`
	if id != "" {
		query += ` LIMIT 2`
	}
	rows, err := db.QueryContext(ctx, s.query(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := []GatewayCalendarGrant{}
	for rows.Next() {
		var grant GatewayCalendarGrant
		var metadata, capabilities string
		var until, hardUntil int64
		err := rows.Scan(&metadata, &capabilities, &grant.Class, &grant.Revision, &grant.ConnectionRevision, &grant.DetailReceipt, &until, &hardUntil)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(metadata), &grant.Metadata); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(capabilities), &grant.Capabilities); err != nil {
			return nil, err
		}
		grant.VerifiedUntil = time.UnixMilli(until).UTC()
		grant.HardVerifiedUntil = time.UnixMilli(hardUntil).UTC()
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

func (s *Store) publishGatewayCalendarBinding(ctx context.Context, tx *sql.Tx, lease GatewayAuthorityLease, proof GatewayCalendarProof, previous gatewayProofRow, now time.Time) (string, error) {
	id := proof.Metadata.ID
	prefix := lease.Provider + ":" + lease.ConnectionID + ":"
	raw, ok := strings.CutPrefix(id, prefix)
	if !ok || raw == "" || len(id) > cachedPageMaxIDBytes {
		return "", ErrAuthorityChanged
	}
	var receipt string
	var readable bool
	err := tx.QueryRowContext(ctx, s.query(`SELECT detail_receipt,can_read FROM calendars WHERE id=? AND connection_id=? AND provider_calendar_id=?`), id, lease.ConnectionID, raw).Scan(&receipt, &readable)
	missing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !missing {
		return "", err
	}
	if receipt != proof.DetailReceipt {
		return "", ErrAuthorityChanged
	}
	read := proof.Class != GatewayMetadataOnly
	write := proof.Capabilities.Operations.Create || proof.Capabilities.Operations.Update || proof.Capabilities.Operations.Delete
	roleChanged := lease.Provider == "google" && read && DetailReceiptRole(receipt) != proof.GoogleRole
	accessChanged := readable != read || previous.class != "" && previous.class != string(proof.Class) || roleChanged
	finalReceipt := receipt
	if lease.Provider == "google" {
		if !read {
			finalReceipt = ""
		} else if roleChanged || accessChanged {
			finalReceipt = proof.GoogleRole + ":" + uuid.NewString()
		}
	} else if receipt == "" || accessChanged {
		finalReceipt = uuid.NewString()
	}
	if lease.Provider == "microsoft" && read && !missing && !accessChanged {
		result, resetErr := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET generation=generation+1,cursor='',status='pending',lease_owner=NULL,lease_until=NULL,last_success_at=NULL,last_error_code=NULL,next_sync_at=?,updated_at=? WHERE calendar_id=? AND status='parked' AND last_error_code IN ('auth','permission')`), now, now, id)
		if resetErr != nil {
			return "", resetErr
		}
		n, resetErr := result.RowsAffected()
		if resetErr != nil {
			return "", resetErr
		}
		if n > 0 {
			if err := s.purgeAuthorizedProjection(ctx, tx, projectionCalendarScope, id); err != nil {
				return "", err
			}
		}
	}
	if missing {
		_, err = tx.ExecContext(ctx, s.query(`INSERT INTO calendars
			(id,connection_id,provider_calendar_id,name,timezone,can_read,can_write,supports_recurrence,discovered_at,google_primary,detail_receipt)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`), id, lease.ConnectionID, raw, proof.Metadata.Name, safeDiscoveryTimezone(lease.Provider, proof.Metadata.Timezone),
			read, write, proof.Capabilities.Fields.Recurrence, now, lease.Provider == "google" && proof.Metadata.Primary, finalReceipt)
		return finalReceipt, err
	}
	if finalReceipt != receipt || accessChanged {
		_, err = tx.ExecContext(ctx, s.query(`UPDATE calendars SET can_read=?,can_write=?,detail_receipt=? WHERE id=?`), read, write, finalReceipt, id)
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, s.query(`UPDATE calendar_sync_state SET generation=generation+1,cursor='',status='pending',
			lease_owner=NULL,lease_until=NULL,last_success_at=NULL,last_error_code=NULL,next_sync_at=?,updated_at=? WHERE calendar_id=?`), now, now, id); err != nil {
			return "", err
		}
		if err := s.purgeAuthorizedProjection(ctx, tx, projectionCalendarScope, id); err != nil {
			return "", err
		}
	}
	return finalReceipt, nil
}

func validGatewayGoogleRole(role string, class GatewayAccessClass) bool {
	switch role {
	case "owner", "writer":
		return class == GatewayPrivateAccess
	case "reader", "writerWithoutPrivateAccess":
		return class == GatewayEventAccess
	case "freeBusyReader":
		return class == GatewayMetadataOnly
	default:
		return false
	}
}

// RestrictGatewayCalendarRead closes the affected event grant even if a later
// probe prevents complete publication. Calendar discovery metadata stays
// available while its independently verified inventory remains current.
func (s *Store) RestrictGatewayCalendarRead(ctx context.Context, lease GatewayAuthorityLease, rawID, receipt string, eventsDenied bool, now time.Time) error {
	if rawID == "" || lease.ID == "" || lease.AuthorizationRevision == "" {
		return ErrAuthorityChanged
	}
	started := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.lockGatewayConnection(ctx, tx, &lease); err != nil {
		return err
	}
	var active bool
	err = tx.QueryRowContext(ctx, s.query(`SELECT EXISTS(SELECT 1 FROM gateway_account_checks
	WHERE connection_id=? AND authorization_revision=? AND lease_id=? AND lease_started_ms=? AND lease_until_ms>?)`),
		lease.ConnectionID, lease.AuthorizationRevision, lease.ID, lease.StartedAt.UnixMilli(), now.Add(time.Since(started)).UnixMilli()).Scan(&active)
	if err != nil {
		return err
	}
	if !active {
		return ErrAuthorityChanged
	}
	id := calendar.AccountCalendarID(lease.Provider, lease.ConnectionID, rawID)
	var class, metadata, caps string
	err = tx.QueryRowContext(ctx, s.query(`SELECT g.access_class,g.metadata_json,g.capabilities_json FROM gateway_calendar_grants g
	JOIN calendars c ON c.id=g.calendar_id
	WHERE g.calendar_id=? AND g.connection_id=? AND c.detail_receipt=? AND g.detail_receipt=c.detail_receipt`), id, lease.ConnectionID, receipt).Scan(&class, &metadata, &caps)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if class == string(GatewayMetadataOnly) || !eventsDenied && class != string(GatewayPrivateAccess) {
		return nil
	}
	var item calendar.Calendar
	var capabilities calendar.CalendarCapabilities
	if err := json.Unmarshal([]byte(metadata), &item); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(caps), &capabilities); err != nil {
		return err
	}
	item.ReadOnly = true
	capabilities.ReadOnly = true
	capabilities.Operations = calendar.OperationCapabilities{}
	metadataJSON, err := json.Marshal(item)
	if err != nil {
		return err
	}
	capabilitiesJSON, err := json.Marshal(capabilities)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, s.query(`UPDATE gateway_calendar_grants SET access_class='metadata',grant_revision=?,metadata_json=?,capabilities_json=?
	WHERE calendar_id=?`), uuid.NewString(), string(metadataJSON), string(capabilitiesJSON), id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
