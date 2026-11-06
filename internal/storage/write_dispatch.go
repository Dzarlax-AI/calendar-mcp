package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
)

// BeginWriteDispatch reserves the same global fence as synchronization. A
// failed or ambiguous begin does not authorize dispatch. Callers must retain
// the reservation unless they know all provider calls have finished with a
// confirmed result (or that none were sent).
func (s *Store) BeginWriteDispatch(ctx context.Context, calendarIDs []string) (func(context.Context) error, error) {
	id := uuid.NewString()
	hash := sha256.Sum256([]byte(id))
	ids := append([]string(nil), calendarIDs...)
	err := s.BeginExclusiveDispatch(ctx, ExclusiveDispatchRequest{OperationID: id, TargetKey: GlobalSyncDispatchKey, RequestHash: hex.EncodeToString(hash[:]), Kind: "run", CalendarIDs: ids, Now: time.Now().UTC()})
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		return s.ReleaseExclusiveAfterConfirmedRun(ctx, id, ids, time.Now().UTC())
	}, nil
}
