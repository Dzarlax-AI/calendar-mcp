package eventsync

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type gatewayEventListing interface {
	ListEventsV2(context.Context, calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error)
}

// RefreshGatewayPublication runs only in the worker, after the regular
// cached_events sync has completed. The expanded and series views add fields
// and cancellation evidence the incremental provider feeds do not guarantee.
func RefreshGatewayPublication(ctx context.Context, store *storage.Store, provider calendar.Provider, rawID string, ticket storage.GatewayPublicationTicket, now func() time.Time) error {
	observed := now().UTC()
	if bound, ok := provider.(interface{ CredentialBinding() (string, [32]byte) }); ok {
		if revision, _ := bound.CredentialBinding(); revision != ticket.Grant.ConnectionRevision {
			return storage.ErrAuthorityChanged
		}
	}
	list, ok := provider.(gatewayEventListing)
	if !ok {
		return errors.New("provider has no bounded event projection listing")
	}
	request := calendar.ListEventsRequestV2{CalendarID: rawID, Start: ticket.State.WindowStart, End: ticket.State.WindowEnd,
		View: calendar.RecurrenceBoth, ShowDeleted: true, MaxResults: 250, AvailabilityStrict: true, GatewayCancellationInventory: provider.Name() == "microsoft"}
	items := []calendar.EventV2{}
	seenPages := map[string]bool{}
	seenRows := map[string][32]byte{}
	var size int
	var verifiedSeries map[string]string
	for i := 0; i < 128; i++ {
		page, err := list.ListEventsV2(ctx, request)
		if err != nil {
			return err
		}
		if !page.Complete {
			return errors.New("provider returned incomplete event projection")
		}
		for _, item := range page.Items {
			if item.ID == "" {
				return errors.New("missing provider event identity")
			}
			b, err := json.Marshal(item)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(b)
			if previous, ok := seenRows[item.ID]; ok {
				if previous != digest {
					return errors.New("provider changed event across projection pages")
				}
				continue
			}
			size += len(b)
			if len(items) >= 16000 || size > 64<<20 {
				return storage.ErrGatewayQueryTooLarge
			}
			seenRows[item.ID] = digest
			items = append(items, item)
		}
		if i == 0 {
			verifiedSeries = page.VerifiedCancellationSeries
		} else {
			for id, zone := range verifiedSeries {
				if current, ok := page.VerifiedCancellationSeries[id]; !ok || current != zone {
					delete(verifiedSeries, id)
				}
			}
		}
		if page.NextPageToken == "" {
			// Graph can attest cancellations for a known master. Its views
			// do not attest deleted single events or missing entire series.
			return store.PublishGatewayEvents(ctx, ticket, items, provider.Name() != "microsoft", observed, now().UTC(), verifiedSeries)
		}
		if len(page.NextPageToken) > 8192 || seenPages[page.NextPageToken] {
			return errors.New("invalid provider projection continuation")
		}
		seenPages[page.NextPageToken] = true
		request.PageToken = page.NextPageToken
	}
	return storage.ErrGatewayQueryTooLarge
}
