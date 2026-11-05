package runtime

import (
	"strings"
	"testing"

	"calendar-mcp/internal/syncengine"
)

func TestRunWarningsExplainIncompleteParticipantsAndDetachedSeries(t *testing.T) {
	if got := runWarningsSummary(syncengine.Result{}); got != "" {
		t.Fatalf("unexpected warning: %q", got)
	}
	got := runWarningsSummary(syncengine.Result{ParticipantsIncomplete: 1, DetachedSeries: 1})
	if !strings.Contains(got, "participant lists are incomplete") || !strings.Contains(got, syncengine.DetachedSeriesWarning) {
		t.Fatalf("missing warning: %q", got)
	}
}
