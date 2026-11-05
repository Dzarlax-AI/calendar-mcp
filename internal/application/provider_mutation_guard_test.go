package application

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

type testMutationGuard struct {
	starts               [][]string
	beginErr, resolveErr error
	resolves             int
}
type testMutationTicket struct{ guard *testMutationGuard }

func (g *testMutationGuard) BeginProviderMutation(_ context.Context, ids []string) (calendar.ProviderMutationIntent, error) {
	g.starts = append(g.starts, append([]string(nil), ids...))
	if g.beginErr != nil {
		return nil, g.beginErr
	}
	return &testMutationTicket{g}, nil
}
func (t *testMutationTicket) ResolveProviderMutation(context.Context) error {
	t.guard.resolves++
	return t.guard.resolveErr
}

type guardTestProvider struct {
	*stubV2Provider
	writes                int
	fail                  error
	resultStatus          string
	nilResponse           bool
	acceptedBeforeFailure bool
	emptyEvent            bool
	createRequiresSafe    bool
	updateRequiresSafe    bool
}

func (p *guardTestProvider) CreateEventV2(_ context.Context, request calendar.CreateEventRequestV2) (*calendar.EventV2, error) {
	p.createRequiresSafe = request.RequireNoAttendees
	p.writes++
	if p.fail != nil {
		p.acceptedBeforeFailure = true // Simulated provider committed before its response failed.
		return nil, p.fail
	}
	if p.nilResponse {
		return nil, nil
	}
	if p.emptyEvent {
		return &calendar.EventV2{}, nil
	}
	return &calendar.EventV2{ID: "created"}, nil
}
func (p *guardTestProvider) UpdateEventV2(_ context.Context, r calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	p.updateRequiresSafe = r.RequireNoAttendees
	if r.PreviewOnly {
		return &calendar.OperationResult{Status: "preview"}, nil
	}
	p.writes++
	if p.fail != nil {
		p.acceptedBeforeFailure = true // Simulated provider committed before its response failed.
		return nil, p.fail
	}
	if p.nilResponse {
		return nil, nil
	}
	return &calendar.OperationResult{Status: p.resultStatus}, nil
}
func (p *guardTestProvider) DeleteEventV2(_ context.Context, r calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	if r.PreviewOnly {
		return &calendar.OperationResult{Status: "preview"}, nil
	}
	p.writes++
	if p.fail != nil {
		p.acceptedBeforeFailure = true // Simulated provider committed before its response failed.
		return nil, p.fail
	}
	if p.nilResponse {
		return nil, nil
	}
	return &calendar.OperationResult{Status: p.resultStatus}, nil
}
func (p *guardTestProvider) RespondToEventV2(context.Context, calendar.RespondToEventRequestV2) (*calendar.OperationResult, error) {
	p.writes++
	if p.fail != nil {
		p.acceptedBeforeFailure = true // Simulated provider committed before its response failed.
		return nil, p.fail
	}
	if p.nilResponse {
		return nil, nil
	}
	return &calendar.OperationResult{Status: p.resultStatus}, nil
}
func (p *guardTestProvider) MoveEventV2(context.Context, calendar.MoveEventRequestV2) (*calendar.OperationResult, error) {
	p.writes++
	if p.fail != nil {
		p.acceptedBeforeFailure = true // Simulated provider committed before its response failed.
		return nil, p.fail
	}
	if p.nilResponse {
		return nil, nil
	}
	return &calendar.OperationResult{Status: p.resultStatus}, nil
}
func (p *guardTestProvider) ImportEventV2(context.Context, calendar.ImportEventRequestV2) (*calendar.EventV2, error) {
	p.writes++
	if p.fail != nil {
		p.acceptedBeforeFailure = true // Simulated provider committed before its response failed.
		return nil, p.fail
	}
	if p.nilResponse {
		return nil, nil
	}
	if p.emptyEvent {
		return &calendar.EventV2{}, nil
	}
	return &calendar.EventV2{ID: "imported"}, nil
}
func newGuardTestProvider() *guardTestProvider {
	return &guardTestProvider{stubV2Provider: &stubV2Provider{name: "google", capabilities: calendar.CalendarCapabilities{MutationScopes: []calendar.MutationScope{calendar.ScopeSingle, calendar.ScopeSeries, calendar.ScopeFollowing}, NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone}}}, resultStatus: "completed"}
}
func guardEvent() calendar.EventCreateV2 {
	return calendar.EventCreateV2{Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}
}
func guardRef() calendar.EventRef {
	return calendar.EventRef{CalendarID: "google:source", EventID: "event"}
}

func TestProviderMutationGuardBlocksEveryProviderDispatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Service) error
	}{
		{"create", func(s *Service) error {
			_, err := s.CreateEvent(t.Context(), calendar.CreateEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
			return err
		}},
		{"update", func(s *Service) error {
			_, err := s.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle})
			return err
		}},
		{"delete", func(s *Service) error {
			_, err := s.DeleteEvent(t.Context(), calendar.DeleteEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle})
			return err
		}},
		{"respond", func(s *Service) error {
			_, err := s.RespondToEvent(t.Context(), calendar.RespondToEventRequestV2{Ref: guardRef(), Response: "accepted"})
			return err
		}},
		{"move", func(s *Service) error {
			_, err := s.MoveEvent(t.Context(), calendar.MoveEventRequestV2{Ref: guardRef(), DestinationCalendarID: "google:target"})
			return err
		}},
		{"import", func(s *Service) error {
			_, err := s.ImportEvent(t.Context(), calendar.ImportEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newGuardTestProvider()
			g := &testMutationGuard{beginErr: errors.New("synthetic")}
			s := New(calendar.NewRegistry([]calendar.Provider{p}), WithProviderMutationGuard(g))
			if err := tc.call(s); err == nil || p.writes != 0 || len(g.starts) != 1 {
				t.Fatalf("err=%v writes=%d begins=%d", err, p.writes, len(g.starts))
			}
		})
	}
}

func TestProviderMutationGuardLeavesTimeoutAndPartialUnresolved(t *testing.T) {
	p := newGuardTestProvider()
	g := &testMutationGuard{}
	s := New(calendar.NewRegistry([]calendar.Provider{p}), WithProviderMutationGuard(g))
	p.fail = context.DeadlineExceeded
	if _, err := s.CreateEvent(t.Context(), calendar.CreateEventRequestV2{CalendarID: "google:source", Event: guardEvent()}); err == nil {
		t.Fatal("missing timeout")
	}
	if g.resolves != 0 {
		t.Fatal("resolved ambiguous timeout")
	}
	p.fail = nil
	p.resultStatus = "partial_failure"
	result, err := s.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle})
	if err != nil || result.Status != "partial_failure" || g.resolves != 0 {
		t.Fatalf("result=%v err=%v resolves=%d", result, err, g.resolves)
	}
}

func TestProviderMutationGuardMoveUsesBothCalendarsAndResolveFailureKeepsSuccess(t *testing.T) {
	p := newGuardTestProvider()
	g := &testMutationGuard{resolveErr: errors.New("synthetic")}
	s := New(calendar.NewRegistry([]calendar.Provider{p}), WithProviderMutationGuard(g))
	result, err := s.MoveEvent(t.Context(), calendar.MoveEventRequestV2{Ref: guardRef(), DestinationCalendarID: "google:target"})
	if err != nil || result.Status != "completed" || len(result.Warnings) != 1 || g.resolves != 1 || !reflect.DeepEqual(g.starts, [][]string{{"google:source", "google:target"}}) {
		t.Fatalf("result=%v err=%v starts=%v", result, err, g.starts)
	}
}

func TestProviderMutationGuardPreviewAndNilCompatibility(t *testing.T) {
	p := newGuardTestProvider()
	g := &testMutationGuard{}
	reg := calendar.NewRegistry([]calendar.Provider{p})
	s := New(reg, WithProviderMutationGuard(g))
	_, err := s.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle, PreviewOnly: true})
	if err == nil || len(g.starts) != 0 || p.writes != 0 {
		t.Fatalf("preview err=%v begins=%d writes=%d", err, len(g.starts), p.writes)
	}
	_, err = New(reg).CreateEvent(t.Context(), calendar.CreateEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
	if err != nil || p.writes != 1 {
		t.Fatalf("nil guard err=%v writes=%d", err, p.writes)
	}
}

type guardFollowingProvider struct {
	*guardTestProvider
	instanceCalls int
	parentMode    string
}

func (p *guardFollowingProvider) SupportsNoAttendeeMutations() bool { return true }
func (p *guardFollowingProvider) GetEventV2(context.Context, calendar.EventRef) (*calendar.EventV2, error) {
	event := &calendar.EventV2{ID: "parent", ETag: "etag", AttendeesKnown: true, Start: calendar.EventTime{Date: "2026-11-01"}, End: calendar.EventTime{Date: "2026-11-02"}, Recurrence: []string{"RRULE:FREQ=DAILY"}}
	if p.parentMode == "unknown" {
		event.AttendeesKnown = false
	}
	if p.parentMode == "attendee" {
		event.Attendees = []calendar.AttendeeV2{{}}
	}
	if p.parentMode == "etag" {
		event.ETag = ""
	}
	return event, nil
}
func (p *guardFollowingProvider) GetEventInstancesV2(context.Context, calendar.InstancesRequestV2) (calendar.Page[calendar.EventV2], error) {
	p.instanceCalls++
	if p.instanceCalls%2 == 1 {
		return calendar.Page[calendar.EventV2]{Items: []calendar.EventV2{{ID: "occurrence", Start: calendar.EventTime{Date: "2026-11-05"}, End: calendar.EventTime{Date: "2026-11-06"}}}}, nil
	}
	return calendar.Page[calendar.EventV2]{Items: []calendar.EventV2{{ID: "prior"}}}, nil
}
func (p *guardFollowingProvider) FindEventByOperationIDV2(context.Context, string, string) (*calendar.EventV2, error) {
	return nil, nil
}

func TestProviderMutationGuardFollowingUsesOneTicketForAllWrites(t *testing.T) {
	p := &guardFollowingProvider{guardTestProvider: newGuardTestProvider()}
	g := &testMutationGuard{}
	s := New(calendar.NewRegistry([]calendar.Provider{p}), WithProviderMutationGuard(g))
	effective := calendar.EventTime{Date: "2026-11-05"}
	result, err := s.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeFollowing, EffectiveFrom: &effective})
	if err == nil || result != nil || p.writes != 0 || len(g.starts) != 1 || g.resolves != 0 {
		t.Fatalf("result=%v err=%v writes=%d begins=%d resolves=%d", result, err, p.writes, len(g.starts), g.resolves)
	}
}

func TestProviderMutationGuardDoesNotResolveAfterWriteThroughFailure(t *testing.T) {
	p := newGuardTestProvider()
	g := &testMutationGuard{}
	m := &stubEventReadModel{upsertErr: errors.New("synthetic")}
	s := New(calendar.NewRegistry([]calendar.Provider{p}), WithEventReadModel(m), WithProviderMutationGuard(g))
	_, warnings, err := s.CreateEventWithReconciliation(t.Context(), calendar.CreateEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
	if err != nil || p.writes != 1 || g.resolves != 0 || len(warnings) != 2 {
		t.Fatalf("err=%v writes=%d resolves=%d warnings=%v", err, p.writes, g.resolves, warnings)
	}
}

func TestProviderMutationGuardCloneAndSameCalendarDeduplicate(t *testing.T) {
	p := newGuardTestProvider()
	g := &testMutationGuard{}
	base := New(calendar.NewRegistry([]calendar.Provider{p}), WithProviderMutationGuard(g))
	s := base.CloneWithEventReadModel(&stubEventReadModel{}, storage.SyncWindow{})
	_, err := s.MoveEvent(t.Context(), calendar.MoveEventRequestV2{Ref: guardRef(), DestinationCalendarID: "google:source"})
	if err != nil || g.resolves != 1 || !reflect.DeepEqual(g.starts, [][]string{{"google:source"}}) {
		t.Fatalf("err=%v resolves=%d starts=%v", err, g.resolves, g.starts)
	}
}

type guardRoutedProvider struct {
	*guardTestProvider
	route string
}

func (p *guardRoutedProvider) RouteName() string           { return p.route }
func (p *guardRoutedProvider) OwnsCalendar(id string) bool { return id == "primary" }

func TestMoveRejectsCrossAccountBeforeGuardAndProvider(t *testing.T) {
	for _, useGuard := range []bool{false, true} {
		t.Run(map[bool]string{false: "nil guard", true: "guard enabled"}[useGuard], func(t *testing.T) {
			first := &guardRoutedProvider{guardTestProvider: newGuardTestProvider(), route: "google@first"}
			second := &guardRoutedProvider{guardTestProvider: newGuardTestProvider(), route: "google@second"}
			g := &testMutationGuard{}
			reg := calendar.NewRegistry([]calendar.Provider{first, second})
			var s *Service
			if useGuard {
				s = New(reg, WithProviderMutationGuard(g))
			} else {
				s = New(reg)
			}
			result, err := s.MoveEvent(t.Context(), calendar.MoveEventRequestV2{Ref: calendar.EventRef{CalendarID: "google:first:primary", EventID: "event"}, DestinationCalendarID: "google:second:primary"})
			if err == nil || result != nil || first.writes != 0 || second.writes != 0 || len(g.starts) != 0 {
				t.Fatalf("cross-account move dispatched: err=%v first=%d second=%d guards=%d", err, first.writes, second.writes, len(g.starts))
			}
		})
	}
}

func TestProviderMutationGuardNilAndEmptyResponsesRemainUnresolved(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hasEvent bool
		call     func(*Service) error
	}{
		{"create", true, func(s *Service) error {
			_, e := s.CreateEvent(t.Context(), calendar.CreateEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
			return e
		}},
		{"import", true, func(s *Service) error {
			_, e := s.ImportEvent(t.Context(), calendar.ImportEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
			return e
		}},
		{"update", false, func(s *Service) error {
			_, e := s.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle})
			return e
		}},
		{"delete", false, func(s *Service) error {
			_, e := s.DeleteEvent(t.Context(), calendar.DeleteEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle})
			return e
		}},
		{"respond", false, func(s *Service) error {
			_, e := s.RespondToEvent(t.Context(), calendar.RespondToEventRequestV2{Ref: guardRef(), Response: "accepted"})
			return e
		}},
		{"move", false, func(s *Service) error {
			_, e := s.MoveEvent(t.Context(), calendar.MoveEventRequestV2{Ref: guardRef(), DestinationCalendarID: "google:target"})
			return e
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modes := []string{"nil"}
			if tc.hasEvent {
				modes = append(modes, "empty id")
			}
			for _, mode := range modes {
				t.Run(mode, func(t *testing.T) {
					p := newGuardTestProvider()
					p.nilResponse = mode == "nil"
					p.emptyEvent = mode == "empty id"
					g := &testMutationGuard{}
					s := New(calendar.NewRegistry([]calendar.Provider{p}), WithProviderMutationGuard(g))
					err := tc.call(s)
					var apiErr *calendar.APIError
					if !errors.Is(err, calendar.ErrProviderMutationOutcomeUnknown) || !errors.As(err, &apiErr) || apiErr.Code != calendar.ErrorPartialFailure || apiErr.Retryable || p.writes != 1 || g.resolves != 0 {
						t.Fatalf("err=%v writes=%d resolves=%d", err, p.writes, g.resolves)
					}
				})
			}
		})
	}
}

func TestProviderMutationGuardPreservesTypedBeginErrorWithoutLeakingIt(t *testing.T) {
	p := newGuardTestProvider()
	cause := &storage.ProjectionMutationBeginError{OperationID: "private-opaque-operation-reference", Cause: errors.New("private-storage-detail")}
	g := &testMutationGuard{beginErr: cause}
	s := New(calendar.NewRegistry([]calendar.Provider{p}), WithProviderMutationGuard(g))
	_, err := s.CreateEvent(t.Context(), calendar.CreateEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
	var retained *storage.ProjectionMutationBeginError
	if !errors.As(err, &retained) || retained.OperationID != cause.OperationID || strings.Contains(err.Error(), cause.OperationID) || strings.Contains(err.Error(), "private-storage-detail") || p.writes != 0 {
		t.Fatalf("typed begin error was lost or leaked: %v", err)
	}
}

func TestProviderMutationGuardFollowingPartialNeverResolves(t *testing.T) {
	for _, op := range []string{"update", "delete"} {
		t.Run(op, func(t *testing.T) {
			p := &guardFollowingProvider{guardTestProvider: newGuardTestProvider()}
			p.resultStatus = "partial_failure"
			g := &testMutationGuard{}
			s := New(calendar.NewRegistry([]calendar.Provider{p}), WithProviderMutationGuard(g))
			effective := calendar.EventTime{Date: "2026-11-05"}
			var result *calendar.OperationResult
			var err error
			if op == "update" {
				result, err = s.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeFollowing, EffectiveFrom: &effective})
			} else {
				result, err = s.DeleteEvent(t.Context(), calendar.DeleteEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeFollowing, EffectiveFrom: &effective})
			}
			if err == nil || result != nil || p.writes != 0 || g.resolves != 0 {
				t.Fatalf("result=%v err=%v resolves=%d", result, err, g.resolves)
			}
		})
	}
}

type followingNilTrimProvider struct{ *guardFollowingProvider }

func (p *followingNilTrimProvider) UpdateEventV2(context.Context, calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	p.writes++
	return nil, nil
}

func TestProviderMutationGuardFollowingNilTrimNeverResolves(t *testing.T) {
	p := &followingNilTrimProvider{&guardFollowingProvider{guardTestProvider: newGuardTestProvider()}}
	g := &testMutationGuard{}
	s := New(calendar.NewRegistry([]calendar.Provider{p}), WithProviderMutationGuard(g))
	effective := calendar.EventTime{Date: "2026-11-05"}
	_, err := s.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeFollowing, EffectiveFrom: &effective})
	if err == nil || p.writes != 0 || g.resolves != 0 {
		t.Fatalf("err=%v writes=%d resolves=%d", err, p.writes, g.resolves)
	}
}

type followingUnknownCompensationProvider struct {
	*guardFollowingProvider
	trimApplied bool
	deleteCalls int
}

func (p *followingUnknownCompensationProvider) UpdateEventV2(context.Context, calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	p.writes++
	p.trimApplied = true // Provider applied the trim, then the response was lost.
	return nil, context.DeadlineExceeded
}
func (p *followingUnknownCompensationProvider) DeleteEventV2(context.Context, calendar.DeleteEventRequestV2) (*calendar.OperationResult, error) {
	p.writes++
	p.deleteCalls++
	return nil, nil
}

func TestProviderMutationGuardFollowingTimeoutDoesNotDeleteReplacement(t *testing.T) {
	p := &followingUnknownCompensationProvider{guardFollowingProvider: &guardFollowingProvider{guardTestProvider: newGuardTestProvider()}}
	g := &testMutationGuard{}
	s := New(calendar.NewRegistry([]calendar.Provider{p}), WithProviderMutationGuard(g))
	effective := calendar.EventTime{Date: "2026-11-05"}
	result, err := s.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeFollowing, EffectiveFrom: &effective})
	if err == nil || result != nil || p.writes != 0 || p.trimApplied || p.deleteCalls != 0 || g.resolves != 0 {
		t.Fatalf("result=%v err=%v writes=%d resolves=%d", result, err, p.writes, g.resolves)
	}
}

func TestPartialDeleteSchedulesReconciliationWithoutTombstone(t *testing.T) {
	for _, guardEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "nil guard", true: "guard enabled"}[guardEnabled], func(t *testing.T) {
			p := newGuardTestProvider()
			p.resultStatus = "partial_failure"
			model := &stubEventReadModel{}
			g := &testMutationGuard{}
			opts := []Option{WithEventReadModel(model)}
			if guardEnabled {
				opts = append(opts, WithProviderMutationGuard(g))
			}
			s := New(calendar.NewRegistry([]calendar.Provider{p}), opts...)
			result, err := s.DeleteEvent(t.Context(), calendar.DeleteEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle})
			if err != nil || result == nil || result.Status != "partial_failure" || len(model.deleted) != 0 || len(model.scheduled) != 1 || g.resolves != 0 {
				t.Fatalf("result=%v err=%v deleted=%d scheduled=%d resolved=%d", result, err, len(model.deleted), len(model.scheduled), g.resolves)
			}
		})
	}
}

func TestNilGuardStillReportsUnknownProviderOutcome(t *testing.T) {
	p := newGuardTestProvider()
	p.nilResponse = true
	s := New(calendar.NewRegistry([]calendar.Provider{p}))
	_, err := s.CreateEvent(t.Context(), calendar.CreateEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
	if !errors.Is(err, calendar.ErrProviderMutationOutcomeUnknown) || p.writes != 1 {
		t.Fatalf("err=%v writes=%d", err, p.writes)
	}
}

// Each fake dispatch simulates a provider that persisted the mutation before
// returning a timeout; no adapter may suggest an automatic retry.
func TestGuardedProviderTimeoutIsUnknownWithoutSecondDispatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Service) error
	}{
		{"create", func(s *Service) error {
			_, e := s.CreateEvent(t.Context(), calendar.CreateEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
			return e
		}},
		{"import", func(s *Service) error {
			_, e := s.ImportEvent(t.Context(), calendar.ImportEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
			return e
		}},
		{"update", func(s *Service) error {
			_, e := s.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle})
			return e
		}},
		{"delete", func(s *Service) error {
			_, e := s.DeleteEvent(t.Context(), calendar.DeleteEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle})
			return e
		}},
		{"respond", func(s *Service) error {
			_, e := s.RespondToEvent(t.Context(), calendar.RespondToEventRequestV2{Ref: guardRef(), Response: "accepted"})
			return e
		}},
		{"move", func(s *Service) error {
			_, e := s.MoveEvent(t.Context(), calendar.MoveEventRequestV2{Ref: guardRef(), DestinationCalendarID: "google:target"})
			return e
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newGuardTestProvider()
			p.fail = context.DeadlineExceeded
			g := &testMutationGuard{}
			s := New(calendar.NewRegistry([]calendar.Provider{p}), WithProviderMutationGuard(g))
			err := tc.call(s)
			var apiErr *calendar.APIError
			if !errors.Is(err, calendar.ErrProviderMutationOutcomeUnknown) || !errors.As(err, &apiErr) || apiErr.Retryable || p.writes != 1 || !p.acceptedBeforeFailure || g.resolves != 0 {
				t.Fatalf("err=%v dispatches=%d resolves=%d", err, p.writes, g.resolves)
			}
		})
	}
}

func TestNilGuardProviderTimeoutRetainsPreviousClassification(t *testing.T) {
	p := newGuardTestProvider()
	p.fail = context.DeadlineExceeded
	s := New(calendar.NewRegistry([]calendar.Provider{p}))
	_, err := s.CreateEvent(t.Context(), calendar.CreateEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
	var apiErr *calendar.APIError
	if errors.Is(err, calendar.ErrProviderMutationOutcomeUnknown) || !errors.As(err, &apiErr) || !apiErr.Retryable || p.writes != 1 {
		t.Fatalf("err=%v writes=%d", err, p.writes)
	}
}

type previewFailureProvider struct{ *guardTestProvider }

func (p *previewFailureProvider) UpdateEventV2(_ context.Context, r calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	if r.PreviewOnly {
		return nil, context.DeadlineExceeded
	}
	return p.guardTestProvider.UpdateEventV2(context.Background(), r)
}
func TestGuardedPreviewErrorIsNotMutationOutcomeUnknown(t *testing.T) {
	p := &previewFailureProvider{newGuardTestProvider()}
	g := &testMutationGuard{}
	s := New(calendar.NewRegistry([]calendar.Provider{p}), WithProviderMutationGuard(g))
	_, err := s.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle, PreviewOnly: true})
	if err == nil || errors.Is(err, calendar.ErrProviderMutationOutcomeUnknown) || len(g.starts) != 0 || p.writes != 0 {
		t.Fatalf("err=%v starts=%d writes=%d", err, len(g.starts), p.writes)
	}
}

func TestGuardedFollowingPreDispatchConflictRetainsClassification(t *testing.T) {
	p := &guardFollowingProvider{guardTestProvider: newGuardTestProvider()}
	g := &testMutationGuard{}
	s := New(calendar.NewRegistry([]calendar.Provider{p}), WithProviderMutationGuard(g))
	effective := calendar.EventTime{Date: "2026-11-05"}
	_, err := s.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeFollowing, EffectiveFrom: &effective, ExpectedETag: "different"})
	var apiErr *calendar.APIError
	if errors.Is(err, calendar.ErrProviderMutationOutcomeUnknown) || !errors.As(err, &apiErr) || apiErr.Code != calendar.ErrorConflict || p.writes != 0 {
		t.Fatalf("err=%v writes=%d", err, p.writes)
	}
}

type followingPreflightProvider struct {
	*guardFollowingProvider
	failure string
}

func (p *followingPreflightProvider) GetEventV2(ctx context.Context, ref calendar.EventRef) (*calendar.EventV2, error) {
	if p.failure == "nil-parent" {
		return nil, nil
	}
	if p.failure == "parent-error" {
		return nil, errors.New("private-parent-secret")
	}
	return p.guardFollowingProvider.GetEventV2(ctx, ref)
}
func (p *followingPreflightProvider) FindEventByOperationIDV2(context.Context, string, string) (*calendar.EventV2, error) {
	if p.failure == "lookup-error" {
		return nil, errors.New("private-lookup-secret")
	}
	return nil, nil
}
func (p *followingPreflightProvider) GetEventInstancesV2(ctx context.Context, req calendar.InstancesRequestV2) (calendar.Page[calendar.EventV2], error) {
	if p.failure == "instances-error" {
		return calendar.Page[calendar.EventV2]{}, errors.New("private-instances-secret")
	}
	return p.guardFollowingProvider.GetEventInstancesV2(ctx, req)
}

func TestFollowingPreflightReadFailuresAreSafeAndNotMutationUnknown(t *testing.T) {
	for _, op := range []string{"update", "delete"} {
		for _, preview := range []bool{false, true} {
			for _, failure := range []string{"nil-parent", "parent-error", "lookup-error", "instances-error"} {
				if op == "delete" && failure == "lookup-error" {
					continue
				}
				name := fmt.Sprintf("%s/preview=%t/%s", op, preview, failure)
				t.Run(name, func(t *testing.T) {
					p := &followingPreflightProvider{guardFollowingProvider: &guardFollowingProvider{guardTestProvider: newGuardTestProvider()}, failure: failure}
					g := &testMutationGuard{}
					s := New(calendar.NewRegistry([]calendar.Provider{p}), WithProviderMutationGuard(g))
					effective := calendar.EventTime{Date: "2026-11-05"}
					var err error
					if op == "update" {
						_, err = s.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeFollowing, EffectiveFrom: &effective, PreviewOnly: preview})
					} else {
						_, err = s.DeleteEvent(t.Context(), calendar.DeleteEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeFollowing, EffectiveFrom: &effective, PreviewOnly: preview})
					}
					var apiErr *calendar.APIError
					if err == nil || errors.Is(err, calendar.ErrProviderMutationOutcomeUnknown) || !errors.As(err, &apiErr) || apiErr.Code != calendar.ErrorProviderUnavailable || !apiErr.Retryable || strings.Contains(err.Error(), "private-") || p.writes != 0 || g.resolves != 0 {
						t.Fatalf("err=%v writes=%d resolves=%d", err, p.writes, g.resolves)
					}
					if preview && len(g.starts) != 0 {
						t.Fatal("preview started mutation guard")
					}
				})
			}
		}
	}
}

type partialUpdateWithEventsProvider struct{ *guardTestProvider }

func (p *partialUpdateWithEventsProvider) UpdateEventV2(context.Context, calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	p.writes++
	return &calendar.OperationResult{Status: "partial_failure", Event: &calendar.EventV2{ID: "unconfirmed"}, RelatedEvents: []calendar.EventV2{{ID: "also-unconfirmed"}}}, nil
}
func TestPartialUpdateSchedulesReconciliationWithoutWriteThrough(t *testing.T) {
	for _, guardEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "nil guard", true: "guard enabled"}[guardEnabled], func(t *testing.T) {
			p := &partialUpdateWithEventsProvider{newGuardTestProvider()}
			m := &stubEventReadModel{}
			g := &testMutationGuard{}
			opts := []Option{WithEventReadModel(m)}
			if guardEnabled {
				opts = append(opts, WithProviderMutationGuard(g))
			}
			s := New(calendar.NewRegistry([]calendar.Provider{p}), opts...)
			result, err := s.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle})
			if err != nil || result == nil || result.Status != "partial_failure" || len(m.upserts) != 0 || len(m.deleted) != 0 || len(m.scheduled) != 1 || g.resolves != 0 {
				t.Fatalf("result=%v err=%v upserts=%d deleted=%d scheduled=%d resolves=%d", result, err, len(m.upserts), len(m.deleted), len(m.scheduled), g.resolves)
			}
		})
	}
}

func TestFollowingWriteBlocksAttendeesOrUnknownButAllowsPreview(t *testing.T) {
	for _, mode := range []string{"safe", "attendee", "unknown", "etag", "patch"} {
		for _, deleteOp := range []bool{false, true} {
			if mode == "patch" && deleteOp {
				continue
			}
			t.Run(fmt.Sprintf("%s-delete-%t", mode, deleteOp), func(t *testing.T) {
				p := &guardFollowingProvider{guardTestProvider: newGuardTestProvider(), parentMode: mode}
				s := New(calendar.NewRegistry([]calendar.Provider{p}))
				effective := calendar.EventTime{Date: "2026-11-05"}
				var err error
				if deleteOp {
					_, err = s.DeleteEvent(t.Context(), calendar.DeleteEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeFollowing, EffectiveFrom: &effective})
				} else {
					request := calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeFollowing, EffectiveFrom: &effective}
					if mode == "patch" {
						request.Patch.Attendees = calendar.PatchField[[]calendar.AttendeeV2]{Present: true, Value: []calendar.AttendeeV2{{}}}
					}
					_, err = s.UpdateEvent(t.Context(), request)
				}
				if err == nil || p.writes != 0 {
					t.Fatalf("unsafe following write: err=%v writes=%d", err, p.writes)
				}
				p.instanceCalls = 0
				var preview *calendar.OperationResult
				if deleteOp {
					preview, err = s.DeleteEvent(t.Context(), calendar.DeleteEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeFollowing, EffectiveFrom: &effective, PreviewOnly: true})
				} else {
					preview, err = s.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeFollowing, EffectiveFrom: &effective, PreviewOnly: true})
				}
				if err != nil || preview == nil || preview.Status != "preview" || p.writes != 0 {
					t.Fatalf("preview err=%v writes=%d", err, p.writes)
				}
			})
		}
	}
}
