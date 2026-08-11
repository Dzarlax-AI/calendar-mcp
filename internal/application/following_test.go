package application

import (
	"context"
	"reflect"
	"testing"

	"calendar-mcp/internal/calendar"
)

func TestSplitRecurrenceUsesUTCUntilForTimedSeries(t *testing.T) {
	oldRules, futureRules, err := splitRecurrence(
		[]string{"RRULE:FREQ=WEEKLY;BYDAY=MO;UNTIL=20261231T225959Z", "EXDATE:20260817T070000Z"},
		calendar.EventTime{DateTime: "2026-08-24T09:00:00+02:00", TimeZone: "Europe/Belgrade"},
		2,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantOld := []string{"RRULE:FREQ=WEEKLY;BYDAY=MO;UNTIL=20260824T065959Z", "EXDATE:20260817T070000Z"}
	if !reflect.DeepEqual(oldRules, wantOld) {
		t.Fatalf("old rules = %#v, want %#v", oldRules, wantOld)
	}
	wantFuture := []string{"RRULE:FREQ=WEEKLY;BYDAY=MO;UNTIL=20261231T225959Z", "EXDATE:20260817T070000Z"}
	if !reflect.DeepEqual(futureRules, wantFuture) {
		t.Fatalf("future rules = %#v, want %#v", futureRules, wantFuture)
	}
}

func TestCountPriorInstancesIncludesCancelledSlotsAndDrainsPages(t *testing.T) {
	provider := &instancePageStub{}
	count, err := countPriorInstances(context.Background(), provider, calendar.EventRef{}, calendar.EventTime{Date: "2026-08-01"}, calendar.EventTime{Date: "2026-08-04"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}
	if len(provider.requests) != 2 || !provider.requests[0].ShowDeleted || provider.requests[1].PageToken != "next" {
		t.Fatalf("requests = %#v", provider.requests)
	}
}

type instancePageStub struct {
	stubV2Provider
	requests []calendar.InstancesRequestV2
}

func (p *instancePageStub) GetEventInstancesV2(_ context.Context, request calendar.InstancesRequestV2) (calendar.Page[calendar.EventV2], error) {
	p.requests = append(p.requests, request)
	if request.PageToken == "" {
		return calendar.Page[calendar.EventV2]{Items: []calendar.EventV2{{Status: "confirmed"}, {Status: "cancelled"}}, NextPageToken: "next", Complete: true}, nil
	}
	return calendar.Page[calendar.EventV2]{Items: []calendar.EventV2{{Status: "confirmed"}}, Complete: true}, nil
}

func TestSplitRecurrenceRecomputesCount(t *testing.T) {
	oldRules, futureRules, err := splitRecurrence(
		[]string{"RRULE:FREQ=DAILY;COUNT=10"},
		calendar.EventTime{Date: "2026-08-04"},
		3,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := oldRules[0], "RRULE:FREQ=DAILY;COUNT=3"; got != want {
		t.Fatalf("old rule = %q, want %q", got, want)
	}
	if got, want := futureRules[0], "RRULE:FREQ=DAILY;COUNT=7"; got != want {
		t.Fatalf("future rule = %q, want %q", got, want)
	}
	if !recurrenceTrimmedBefore(oldRules, calendar.EventTime{Date: "2026-08-04"}, 3) {
		t.Fatal("COUNT-trimmed recurrence was not recognized for idempotent retry")
	}
}

func TestFollowingRejectsRDateBecauseItCannotBeSplitSafely(t *testing.T) {
	err := followingRecurrenceSupported([]string{"RRULE:FREQ=DAILY", "RDATE:20260812T070000Z"})
	if err == nil {
		t.Fatal("followingRecurrenceSupported() returned nil for RDATE")
	}
}

func TestMergeFollowingPatchRequiresValidReplacementRange(t *testing.T) {
	event := calendar.EventCreateV2{
		Start: calendar.EventTime{Date: "2026-08-10"}, End: calendar.EventTime{Date: "2026-08-11"},
		Recurrence: []string{"RRULE:FREQ=DAILY"},
	}
	err := mergeFollowingPatch(&event, calendar.EventPatchV2{
		Start: calendar.PatchField[calendar.EventTime]{Present: true, Value: calendar.EventTime{DateTime: "2026-08-10T09:00:00+02:00", TimeZone: "Europe/Belgrade"}},
	})
	if err == nil {
		t.Fatal("mergeFollowingPatch() allowed mixed all-day and timed boundaries")
	}
}
