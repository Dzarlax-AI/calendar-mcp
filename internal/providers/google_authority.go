package providers

import (
	"context"
	"errors"
	"strings"
	"time"

	"calendar-mcp/internal/google"
	"calendar-mcp/internal/storage"
)

func (f *Factory) attachGoogleRoleObserver(p *google.Provider, id string) {
	p.SetDenialObserver(func(ctx context.Context, rawID string) (func(context.Context, string, int, string, string, [32]byte) error, error) {
		var ticket storage.AuthorityTicket
		var err error
		if rawID == "@account" {
			ticket, err = f.store.CaptureAnyGoogleTicket(ctx, id)
		} else {
			ticket, err = f.store.CaptureGoogleSourceTicket(ctx, id, rawID)
		}
		if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrAuthorityChanged) {
			ticket = storage.AuthorityTicket{ConnectionID: id}
		} else if err != nil {
			return nil, err
		}
		return func(ctx context.Context, operation string, status int, reason, revision string, digest [32]byte) error {
			reasons := strings.Split(strings.ToLower(reason), ",")
			knownQuota := func(value string) bool {
				switch value {
				case "ratelimitexceeded", "userratelimitexceeded", "quotaexceeded", "dailylimitexceeded", "calendarusagelimitsexceeded":
					return true
				}
				return false
			}
			allQuota := reason != ""
			for _, value := range reasons {
				if !knownQuota(value) {
					allQuota = false
				}
			}
			if allQuota {
				return nil
			}
			if status == 401 {
				closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				err := f.store.CloseGoogleAccountIfCurrent(closeCtx, ticket, revision, digest, time.Now().UTC())
				return err
			}
			if operation == "discovery" && status != 401 {
				return nil
			}
			if status == 404 && operation != "event_list" && operation != "calendar" {
				return nil
			}
			if status == 403 && operation == "mutation" && reason != "" {
				allWriteOnly := true
				for _, value := range reasons {
					if value != "forbiddenfornonorganizer" && !knownQuota(value) {
						allWriteOnly = false
					}
				}
				if allWriteOnly {
					return nil
				}
			}
			if ticket.CalendarID == "" {
				return nil
			}
			if status == 403 || status == 404 {
				closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				err := f.store.ObserveGoogleRole(closeCtx, ticket, revision, digest, "", time.Now().UTC())
				if errors.Is(err, storage.ErrAuthorityClosed) {
					return nil
				}
				return err
			}
			return nil
		}, nil
	})

	beginRole := func(skipUnchanged bool) func(context.Context, string) (func(context.Context, string, string, [32]byte) error, error) {
		return func(ctx context.Context, rawID string) (func(context.Context, string, string, [32]byte) error, error) {
			ticket, err := f.store.CaptureGoogleSourceTicket(ctx, id, rawID)
			if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrAuthorityChanged) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			return func(ctx context.Context, role, revision string, digest [32]byte) error {
				unchanged := role != "" && role == storage.DetailReceiptRole(ticket.DetailReceipt)
				if unchanged && skipUnchanged {
					return nil
				}
				if unchanged {
					return f.store.ObserveGoogleRole(ctx, ticket, revision, digest, role, time.Now().UTC())
				}
				closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				return f.store.ObserveGoogleRole(closeCtx, ticket, revision, digest, role, time.Now().UTC())
			}, nil
		}
	}
	p.SetRoleObserver(beginRole(false))
	p.SetCalendarRoleObserver(beginRole(true))
	p.SetAccountRoleObserver(func(ctx context.Context) (func(context.Context, string, string, string, [32]byte) error, error) {
		tickets, err := f.store.CaptureGoogleAccountTickets(ctx, id)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, raw, role, revision string, digest [32]byte) error {
			t, ok := tickets[raw]
			if !ok || (role != "" && role == storage.DetailReceiptRole(t.DetailReceipt)) {
				return nil
			}
			err := f.store.ObserveGoogleRole(ctx, t, revision, digest, role, time.Now().UTC())
			if errors.Is(err, storage.ErrAuthorityClosed) {
				return nil
			}
			return err
		}, nil
	})

}
