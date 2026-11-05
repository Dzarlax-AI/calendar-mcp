package application

import (
	"calendar-mcp/internal/calendar"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

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

func TestEmptyCreateResponseReportsUnknownOutcome(t *testing.T) {
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
func TestCreateTimeoutRetainsProviderClassification(t *testing.T) {
	p := newGuardTestProvider()
	p.fail = context.DeadlineExceeded
	s := New(calendar.NewRegistry([]calendar.Provider{p}))
	_, err := s.CreateEvent(t.Context(), calendar.CreateEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
	var apiErr *calendar.APIError
	if errors.Is(err, calendar.ErrProviderMutationOutcomeUnknown) || !errors.As(err, &apiErr) || !apiErr.Retryable || p.writes != 1 {
		t.Fatalf("err=%v writes=%d", err, p.writes)
	}
}

type partialUpdateWithEventsProvider struct{ *guardTestProvider }

func (p *partialUpdateWithEventsProvider) UpdateEventV2(context.Context, calendar.UpdateEventRequestV2) (*calendar.OperationResult, error) {
	p.writes++
	return &calendar.OperationResult{Status: "partial_failure", Event: &calendar.EventV2{ID: "unconfirmed"}, RelatedEvents: []calendar.EventV2{{ID: "also-unconfirmed"}}}, nil
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

func TestPartialDeleteSchedulesReconciliationWithoutTombstone(t *testing.T) {
	p := newGuardTestProvider()
	p.resultStatus = "partial_failure"
	model := &stubEventReadModel{}
	s := New(calendar.NewRegistry([]calendar.Provider{p}), WithEventReadModel(model))
	result, err := s.DeleteEvent(t.Context(), calendar.DeleteEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle})
	if err != nil || result == nil || result.Status != "partial_failure" || len(model.deleted) != 0 || len(model.scheduled) != 1 {
		t.Fatalf("partial delete: %v", err)
	}
}
func TestPartialUpdateSchedulesReconciliationWithoutWriteThrough(t *testing.T) {
	p := &partialUpdateWithEventsProvider{newGuardTestProvider()}
	model := &stubEventReadModel{}
	s := New(calendar.NewRegistry([]calendar.Provider{p}), WithEventReadModel(model))
	result, err := s.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeSingle})
	if err != nil || result == nil || result.Status != "partial_failure" || len(model.upserts) != 0 || len(model.deleted) != 0 || len(model.scheduled) != 1 {
		t.Fatalf("partial update: %v", err)
	}
}

func TestEmptyMutationResponsesRemainUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Service) error
	}{
		{"create", func(s *Service) error {
			_, err := s.CreateEvent(t.Context(), calendar.CreateEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
			return err
		}},
		{"import", func(s *Service) error {
			_, err := s.ImportEvent(t.Context(), calendar.ImportEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newGuardTestProvider()
			p.nilResponse = true
			s := New(calendar.NewRegistry([]calendar.Provider{p}))
			err := tc.call(s)
			if !errors.Is(err, calendar.ErrProviderMutationOutcomeUnknown) || p.writes != 1 {
				t.Fatalf("err=%v calls=%d", err, p.writes)
			}
		})
	}
}

func TestProviderTimeoutClassificationPreservedAcrossMutations(t *testing.T) {
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
			s := New(calendar.NewRegistry([]calendar.Provider{p}))
			err := tc.call(s)
			var apiErr *calendar.APIError
			if errors.Is(err, calendar.ErrProviderMutationOutcomeUnknown) || !errors.As(err, &apiErr) || !apiErr.Retryable || p.writes != 1 {
				t.Fatalf("err=%v calls=%d", err, p.writes)
			}
		})
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
					s := New(calendar.NewRegistry([]calendar.Provider{p}))
					effective := calendar.EventTime{Date: "2026-11-05"}
					var err error
					if op == "update" {
						_, err = s.UpdateEvent(t.Context(), calendar.UpdateEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeFollowing, EffectiveFrom: &effective, PreviewOnly: preview})
					} else {
						_, err = s.DeleteEvent(t.Context(), calendar.DeleteEventRequestV2{Ref: guardRef(), Scope: calendar.ScopeFollowing, EffectiveFrom: &effective, PreviewOnly: preview})
					}
					var apiErr *calendar.APIError
					if err == nil || errors.Is(err, calendar.ErrProviderMutationOutcomeUnknown) || !errors.As(err, &apiErr) || apiErr.Code != calendar.ErrorProviderUnavailable || !apiErr.Retryable || strings.Contains(err.Error(), "private-") || p.writes != 0 {
						t.Fatalf("err=%v writes=%d", err, p.writes)
					}
				})
			}
		}
	}
}

func TestEmptyCreatedOrImportedIdentityIsUnknown(t *testing.T) {
	for _, op := range []string{"create", "import"} {
		t.Run(op, func(t *testing.T) {
			p := newGuardTestProvider()
			p.emptyEvent = true
			s := New(calendar.NewRegistry([]calendar.Provider{p}))
			var err error
			if op == "create" {
				_, err = s.CreateEvent(t.Context(), calendar.CreateEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
			} else {
				_, err = s.ImportEvent(t.Context(), calendar.ImportEventRequestV2{CalendarID: "google:source", Event: guardEvent()})
			}
			if !errors.Is(err, calendar.ErrProviderMutationOutcomeUnknown) || p.writes != 1 {
				t.Fatalf("err=%v calls=%d", err, p.writes)
			}
		})
	}
}

type routedMutationTestProvider struct {
	*guardTestProvider
	route string
}

func (p *routedMutationTestProvider) RouteName() string           { return p.route }
func (p *routedMutationTestProvider) OwnsCalendar(id string) bool { return id == "primary" }

func TestMoveRejectsCrossAccountBeforeProviderDispatch(t *testing.T) {
	first := &routedMutationTestProvider{guardTestProvider: newGuardTestProvider(), route: "google@first"}
	second := &routedMutationTestProvider{guardTestProvider: newGuardTestProvider(), route: "google@second"}
	s := New(calendar.NewRegistry([]calendar.Provider{first, second}))
	result, err := s.MoveEvent(t.Context(), calendar.MoveEventRequestV2{Ref: calendar.EventRef{CalendarID: "google:first:primary", EventID: "event"}, DestinationCalendarID: "google:second:primary"})
	if err == nil || result != nil || first.writes != 0 || second.writes != 0 {
		t.Fatalf("cross-account move dispatched: err=%v first=%d second=%d", err, first.writes, second.writes)
	}
}
