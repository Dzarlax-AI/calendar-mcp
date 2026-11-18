package eventsync

import (
	"context"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type GatewayWriteFinder interface {
	FindGatewayWriteReceipt(context.Context, storage.GatewayWrite) (*calendar.EventV2, error)
}

// ReconcileGatewayWrite never invokes a provider mutation or notification.
// An absent or ambiguous marker keeps the fence until the outcome deadline.
// After that, a new snapshot is required and the write stays unknown.
func ReconcileGatewayWrite(ctx context.Context, store *storage.Store, lookup GatewayWriteFinder, now time.Time) error {
	write, err := store.ClaimGatewayWriteCheck(ctx, now)
	if err != nil || write == nil {
		return err
	}
	switch write.Phase {
	case "confirmed", "not_dispatched":
		return store.FinishGatewayWrite(ctx, write.ID, write.Calendars, now)
	case "dispatched":
		if !now.Before(write.OutcomeDue) {
			return store.ExpireGatewayWrite(ctx, *write, now)
		}
		switch write.Scope.Kind {
		case "create", "import", "update":
		default:
			return nil
		}
		if write.Scope.Scope == calendar.ScopeFollowing {
			return nil
		}
		check, cancel := context.WithDeadline(ctx, write.OutcomeDue)
		defer cancel()
		event, err := lookup.FindGatewayWriteReceipt(check, *write)
		if err == nil {
			err = check.Err()
		}
		if err != nil || event == nil || event.ID == "" {
			return err
		}
		var receipt any = event
		if write.Scope.Kind == "update" {
			provider, _, _ := strings.Cut(write.Scope.CalendarID, ":")
			if strings.TrimPrefix(event.ID, provider+":") != strings.TrimPrefix(write.Scope.EventID, provider+":") {
				return storage.ErrAuthorityChanged
			}
			receipt = &calendar.OperationResult{Status: "completed", Event: event}
		}
		if err := store.RecordGatewayWriteConfirmation(ctx, write.ID, receipt, time.Now()); err != nil {
			return err
		}
		return store.FinishGatewayWrite(ctx, write.ID, write.Calendars, time.Now())
	default:
		return storage.ErrAuthorityChanged
	}
}
