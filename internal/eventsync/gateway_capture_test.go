package eventsync

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type paginatedGatewayListing struct {
	calendar.Provider
	pages int
}

func (*paginatedGatewayListing) Name() string { return "google" }

func (*paginatedGatewayListing) ListEventsV2(context.Context, calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	panic("expected the snapshot-scoped listing")
}

var errFixtureReachedEnd = errors.New("reached the last provider page")

func (p *paginatedGatewayListing) NewGatewayEventListing() func(context.Context, calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	return func(_ context.Context, request calendar.ListEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
		if request.PageToken == "254" {
			return calendar.Page[calendar.EventV2]{}, errFixtureReachedEnd
		}
		p.pages++
		return calendar.Page[calendar.EventV2]{Complete: true, NextPageToken: strconv.Itoa(p.pages)}, nil
	}
}

func TestGatewayPublicationReachesLastPageOfSequentialGoogleStreams(t *testing.T) {
	provider := &paginatedGatewayListing{}
	err := RefreshGatewayPublication(t.Context(), nil, provider, "synthetic", storage.GatewayPublicationTicket{}, time.Now)
	if !errors.Is(err, errFixtureReachedEnd) || provider.pages != 254 {
		t.Fatalf("wanted last Google provider page, got %d successful pages and %v", provider.pages, err)
	}
}
