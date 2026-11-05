package apple

import (
	"calendar-mcp/internal/calendar"
	"testing"
)

func TestNonFollowingPreviewNeverDispatchesApple(t *testing.T) {
	// The zero-value provider has no transport: reaching it would panic.
	p := &Provider{}
	for _, scope := range []calendar.MutationScope{calendar.ScopeSingle, calendar.ScopeSeries} {
		ref := calendar.EventRef{CalendarID: "calendar", EventID: "event"}
		if _, err := p.UpdateEventV2(t.Context(), calendar.UpdateEventRequestV2{Ref: ref, Scope: scope, PreviewOnly: true}); err == nil {
			t.Fatal("update preview accepted")
		}
		if _, err := p.DeleteEventV2(t.Context(), calendar.DeleteEventRequestV2{Ref: ref, Scope: scope, PreviewOnly: true}); err == nil {
			t.Fatal("delete preview accepted")
		}
	}
}
