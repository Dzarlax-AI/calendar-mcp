package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"calendar-mcp/internal/calendar"
)

type projectionMutationBackend interface {
	BeginProjectionMutationMany(context.Context, []string, string, time.Time) error
	ResolveProjectionMutationManyAndSchedule(context.Context, []string, string, time.Time) error
}

// ProjectionMutationGuard is an opt-in adapter; the runtime does not install it.
// It is NOT copy-safe until all dispatch paths are fenced and a verified clean
// replacement after release has actually completed.
type ProjectionMutationGuard struct {
	store   projectionMutationBackend
	entropy io.Reader
	clock   func() time.Time
}

// NewProjectionMutationGuard builds an adapter around the durable store. It
// does not install the adapter or enable mutation execution.
func NewProjectionMutationGuard(s *Store) *ProjectionMutationGuard {
	g := &ProjectionMutationGuard{entropy: rand.Reader, clock: time.Now}
	if s != nil {
		g.store = s
	}
	return g
}

var _ calendar.ProviderMutationGuard = (*ProjectionMutationGuard)(nil)

// ProjectionMutationBeginError retains the random operation reference after a
// failed or ambiguous storage result. It does not authorize provider dispatch.
type ProjectionMutationBeginError struct {
	OperationID string
	Cause       error
}

func (e *ProjectionMutationBeginError) Error() string {
	return "provider mutation fence could not be established"
}
func (e *ProjectionMutationBeginError) Unwrap() error { return e.Cause }

func (g *ProjectionMutationGuard) BeginProviderMutation(ctx context.Context, calendarIDs []string) (calendar.ProviderMutationIntent, error) {
	if g == nil || g.store == nil || g.entropy == nil || g.clock == nil {
		return nil, errors.New("provider mutation fence is not configured")
	}
	ids, _, err := normalizeMutationCalendars(calendarIDs)
	if err != nil {
		return nil, err
	}
	var random [32]byte
	if _, err = io.ReadFull(g.entropy, random[:]); err != nil {
		return nil, fmt.Errorf("generate provider mutation reference: %w", err)
	}
	operationID := hex.EncodeToString(random[:])
	now := g.clock().UTC()
	if err = g.store.BeginProjectionMutationMany(ctx, ids, operationID, now); err != nil {
		return nil, &ProjectionMutationBeginError{OperationID: operationID, Cause: err}
	}
	return &projectionMutationTicket{store: g.store, operationID: operationID, calendarIDs: ids, clock: g.clock}, nil
}

type projectionMutationTicket struct {
	store       projectionMutationBackend
	operationID string
	calendarIDs []string
	clock       func() time.Time
}

// ResolveProviderMutation is only for a confirmed provider success. On a
// schedule/resolve failure the error must reach the caller, who must not retry
// the provider write. An ambiguous provider result must never call this method.
func (t *projectionMutationTicket) ResolveProviderMutation(ctx context.Context) error {
	if t == nil || t.store == nil || t.clock == nil {
		return errors.New("provider mutation ticket is unavailable")
	}
	if err := t.store.ResolveProjectionMutationManyAndSchedule(ctx, t.calendarIDs, t.operationID, t.clock().UTC()); err != nil {
		return fmt.Errorf("resolve provider mutation fence: %w", err)
	}
	return nil
}
