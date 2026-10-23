package application

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/api/googleapi"

	"calendar-mcp/internal/calendar"
)

type searchStub struct {
	*stubV2Provider
	searchErr error
}

func (p *searchStub) SearchEventsV2(_ context.Context, r calendar.SearchEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	if p.searchErr != nil {
		return calendar.Page[calendar.EventV2]{}, p.searchErr
	}
	if r.PageToken == "" {
		return calendar.Page[calendar.EventV2]{Items: []calendar.EventV2{{ID: "one"}}, NextPageToken: "next", Complete: true}, nil
	}
	return calendar.Page[calendar.EventV2]{Items: []calendar.EventV2{{ID: "two"}}, Complete: true}, nil
}
func TestSearchFanOutUnsupportedIsNotRetryable(t *testing.T) {
	p := &stubV2Provider{name: "apple"}
	s := New(calendar.NewRegistry([]calendar.Provider{p}))
	page, err := s.SearchEvents(t.Context(), calendar.SearchEventsRequestV2{Query: "JetBrains"})
	if err != nil || page.Complete || len(page.Sources) != 1 {
		t.Fatalf("%#v %v", page, err)
	}
	e := page.Sources[0].Error
	if e == nil || e.Code != calendar.ErrorUnsupportedCapability || e.Retryable {
		t.Fatalf("wrong error %#v", e)
	}
	_, err = s.SearchEvents(t.Context(), calendar.SearchEventsRequestV2{CalendarID: "apple:cal", Query: "JetBrains"})
	var direct *calendar.APIError
	if !errors.As(err, &direct) || direct.Code != e.Code || direct.Retryable != e.Retryable {
		t.Fatalf("direct %v vs %#v", err, e)
	}
}
func TestSearchFanOutDrainsAppleAndGoogle(t *testing.T) {
	var providers []calendar.Provider
	for _, name := range []string{"apple", "google"} {
		providers = append(providers, &searchStub{stubV2Provider: &stubV2Provider{name: name, calendars: []calendar.Calendar{{ID: "cal"}}}})
	}
	page, err := New(calendar.NewRegistry(providers)).SearchEvents(t.Context(), calendar.SearchEventsRequestV2{Query: "JetBrains", MaxResults: 1})
	if err != nil || !page.Complete || len(page.Items) != 4 || len(page.Sources) != 2 {
		t.Fatalf("%#v %v", page, err)
	}
}
func TestFanOutPreservesProviderErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		code  calendar.ErrorCode
		retry bool
	}{
		{"unsupported", unsupported("apple", "cal", "bounded range required"), calendar.ErrorUnsupportedCapability, false},
		{"invalid", invalidArgument("invalid token"), calendar.ErrorInvalidArgument, false},
		{"permission", &googleapi.Error{Code: 403}, calendar.ErrorPermissionDenied, false},
		{"limited", &googleapi.Error{Code: 429}, calendar.ErrorRateLimited, true},
		{"unavailable", &googleapi.Error{Code: 503}, calendar.ErrorProviderUnavailable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &searchStub{stubV2Provider: &stubV2Provider{name: "apple", calendars: []calendar.Calendar{{ID: "cal"}}}, searchErr: tc.err}
			page, err := New(calendar.NewRegistry([]calendar.Provider{p})).SearchEvents(t.Context(), calendar.SearchEventsRequestV2{Query: "JetBrains"})
			if err != nil || page.Complete || len(page.Sources) != 1 {
				t.Fatalf("%#v %v", page, err)
			}
			e := page.Sources[0].Error
			if e.Code != tc.code || e.Retryable != tc.retry {
				t.Fatalf("error %#v", e)
			}
		})
	}
}

func TestFailedSourceCopiesErrorContext(t *testing.T) {
	original := unsupported("apple", "/raw/", "bounded range required")
	result := failedSource("apple", "apple:connection:/raw/", original)
	if result.status.Error.CalendarID != "apple:connection:/raw/" || original.CalendarID != "/raw/" || result.status.Error == original {
		t.Fatalf("context=%#v original=%#v", result.status.Error, original)
	}
}
