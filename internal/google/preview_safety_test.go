package google

import (
	"net/http"
	"sync/atomic"
	"testing"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/calendar"
)

func TestNonFollowingPreviewNeverDispatchesGoogle(t *testing.T) {
	var requests atomic.Int32
	provider, closeServer := testProvider(t, func(http.ResponseWriter, *http.Request) { requests.Add(1) })
	defer closeServer()
	s := application.New(calendar.NewRegistry([]calendar.Provider{provider}))
	for _, scope := range []calendar.MutationScope{calendar.ScopeSingle, calendar.ScopeSeries} {
		ref := calendar.EventRef{CalendarID: "google:primary", EventID: "event"}
		if _, err := s.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: ref, Scope: scope, PreviewOnly: true}); err == nil {
			t.Fatal("update preview accepted")
		}
		if _, err := s.DeleteEvent(t.Context(), calendar.DeleteEventRequestV2{Ref: ref, Scope: scope, PreviewOnly: true}); err == nil {
			t.Fatal("delete preview accepted")
		}
		if _, err := provider.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Ref: ref, Scope: scope, PreviewOnly: true}); err == nil {
			t.Fatal("direct update preview accepted")
		}
		if _, err := provider.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{Ref: ref, Scope: scope, PreviewOnly: true}); err == nil {
			t.Fatal("direct delete preview accepted")
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("provider requests=%d", requests.Load())
	}
}
