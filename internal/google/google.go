package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	gcal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/token"
)

type Provider struct {
	svc                  *gcal.Service
	routeName            string
	calendars            map[string]struct{}
	bindingRevision      string
	bindingDigest        [32]byte
	tokenStore           token.Store
	httpClient           *http.Client
	accountRoleObserver  func(context.Context) (func(context.Context, string, string, string, [32]byte) error, error)
	calendarRoleObserver func(context.Context, string) (func(context.Context, string, string, [32]byte) error, error)
	roleObserver         func(context.Context, string) (func(context.Context, string, string, [32]byte) error, error)
}

func New(clientID, clientSecret, refreshToken, tokenDir string) (*Provider, error) {
	store := token.NewFileStore(tokenDir, "google")
	return NewWithTokenStore(clientID, clientSecret, store, &oauth2.Token{RefreshToken: refreshToken})
}

func NewWithTokenStore(clientID, clientSecret string, store token.Store, initial *oauth2.Token) (*Provider, error) {
	cfg := newOAuthConfig(clientID, clientSecret)
	client := newHTTPClient(store, cfg, initial)

	svc, err := gcal.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		return nil, err
	}
	return &Provider{svc: svc, tokenStore: store, httpClient: client}, nil
}

// NewWithSnapshotTokenStore fails closed if captured credentials cannot load.
func NewWithSnapshotTokenStore(clientID, clientSecret string, store token.Store) (*Provider, error) {
	client, err := newSnapshotHTTPClient(store, newOAuthConfig(clientID, clientSecret))
	if err != nil {
		return nil, err
	}
	svc, err := gcal.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		return nil, err
	}
	return &Provider{svc: svc, tokenStore: store, httpClient: client}, nil
}

// SetCredentialBinding associates this provider with its construction snapshot.
func (p *Provider) SetCredentialBinding(revision string, digest [32]byte) {
	p.bindingRevision = revision
	p.bindingDigest = digest
}
func (p *Provider) CredentialBinding() (string, [32]byte) {
	if bound, ok := p.tokenStore.(interface{ CredentialBinding() (string, [32]byte) }); ok {
		return bound.CredentialBinding()
	}
	return p.bindingRevision, p.bindingDigest
}

// SetDenialObserver attaches a bounded, source-scoped transport observer.
// It must be called before the provider is shared with callers.
func (p *Provider) SetDenialObserver(begin func(context.Context, string) (func(context.Context, string, int, string, string, [32]byte) error, error)) {
	if p.httpClient == nil {
		return
	}
	base := p.httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	p.httpClient.Transport = &authorityTransport{base: base, begin: begin, binding: p.CredentialBinding}
}
func (p *Provider) SetAccountRoleObserver(observer func(context.Context) (func(context.Context, string, string, string, [32]byte) error, error)) {
	p.accountRoleObserver = observer
}

func (p *Provider) SetCalendarRoleObserver(observer func(context.Context, string) (func(context.Context, string, string, [32]byte) error, error)) {
	p.calendarRoleObserver = observer
}
func (p *Provider) startCalendarRoleObservation(ctx context.Context, id string) (context.Context, func(context.Context, string, string, [32]byte) error, error) {
	if p.calendarRoleObserver == nil {
		return p.startRoleObservation(ctx, id)
	}
	observe, err := p.calendarRoleObserver(ctx, id)
	if err != nil {
		return ctx, observe, err
	}
	return token.WithRequestBinding(ctx, &token.RequestBinding{}), observe, nil
}

// SetRoleObserver installs optional local authority checks around Events.List.
func (p *Provider) SetRoleObserver(observer func(context.Context, string) (func(context.Context, string, string, [32]byte) error, error)) {
	p.roleObserver = observer
}
func (p *Provider) startRoleObservation(ctx context.Context, id string) (context.Context, func(context.Context, string, string, [32]byte) error, error) {
	if p.roleObserver == nil {
		return token.WithRequestBinding(ctx, &token.RequestBinding{}), nil, nil
	}
	observe, err := p.roleObserver(ctx, id)
	if err != nil {
		return ctx, nil, err
	}
	return token.WithRequestBinding(ctx, &token.RequestBinding{}), observe, nil
}
func authorityError(ctx context.Context, fallback error) error {
	if b := token.RequestBindingFromContext(ctx); b != nil && b.AuthorityError != nil {
		return b.AuthorityError
	}
	return fallback
}
func observeRole(ctx context.Context, observe func(context.Context, string, string, [32]byte) error, role string) error {
	if observe == nil {
		return nil
	}
	b := token.RequestBindingFromContext(ctx)
	if b == nil || !b.Set {
		return fmt.Errorf("request credential binding unavailable")
	}
	if err := observe(ctx, role, b.Revision, b.Digest); err != nil {
		return err
	}
	return ctx.Err()
}

func (p *Provider) Name() string { return "google" }
func (p *Provider) RouteName() string {
	if p.routeName != "" {
		return p.routeName
	}
	return p.Name()
}
func (p *Provider) OwnsCalendar(id string) bool {
	if len(p.calendars) == 0 {
		return true
	}
	_, ok := p.calendars[id]
	return ok
}
func (p *Provider) SetRoute(route string, calendarIDs []string) {
	p.routeName = route
	p.calendars = make(map[string]struct{}, len(calendarIDs))
	for _, id := range calendarIDs {
		p.calendars[id] = struct{}{}
	}
}

func (p *Provider) ListCalendars(ctx context.Context) ([]calendar.Calendar, error) {
	var cals []calendar.Calendar
	err := p.listCalendarPages(ctx, 0, func(list *gcal.CalendarList, binding *token.RequestBinding) error {
		for _, c := range list.Items {
			if c == nil {
				continue
			}
			item := calendar.Calendar{
				ID:                      c.Id,
				Timezone:                c.TimeZone,
				Name:                    c.Summary,
				Color:                   c.BackgroundColor,
				Primary:                 c.Primary,
				ReadOnly:                c.AccessRole == "reader" || c.AccessRole == "freeBusyReader",
				GoogleWriteAccessProven: c.AccessRole == "owner" || c.AccessRole == "writer",
			}
			if binding.Set {
				item.GoogleDiscoveryCredentialDigest = binding.Digest
			}
			cals = append(cals, item)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cals, nil
}

// DiscoverDetailCalendars stages a complete, bounded raw-role listing.
func (p *Provider) DiscoverDetailCalendars(ctx context.Context) ([]calendar.DetailDiscoveryEntry, error) {
	result := make([]calendar.DetailDiscoveryEntry, 0)
	seen := map[string]bool{}
	err := p.listCalendarPages(ctx, 64, func(response *gcal.CalendarList, _ *token.RequestBinding) error {
		for _, c := range response.Items {
			if c == nil || c.Id == "" || seen[c.Id] || len(result) >= 256 {
				return fmt.Errorf("invalid or oversized calendar discovery")
			}
			seen[c.Id] = true
			result = append(result, calendar.DetailDiscoveryEntry{Calendar: calendar.Calendar{ID: c.Id, Name: c.Summary, Color: c.BackgroundColor, Primary: c.Primary, Timezone: c.TimeZone, ReadOnly: c.AccessRole != "owner" && c.AccessRole != "writer"}, AccessRole: c.AccessRole})
		}
		return nil
	})
	if err != nil {
		return nil, authorityError(ctx, err)
	}
	return result, nil
}

func (p *Provider) listCalendarPages(ctx context.Context, maxPages int, consume func(*gcal.CalendarList, *token.RequestBinding) error) error {
	var observe func(context.Context, string, string, string, [32]byte) error
	var err error
	if p.accountRoleObserver != nil {
		observe, err = p.accountRoleObserver(ctx)
		if err != nil {
			return err
		}
	}
	tokens := map[string]bool{}
	next := ""
	for page := 0; ; page++ {
		if maxPages > 0 && page >= maxPages {
			return fmt.Errorf("calendar discovery page limit exceeded")
		}
		binding := &token.RequestBinding{}
		pageCtx := token.WithRequestBinding(ctx, binding)
		call := p.svc.CalendarList.List().Context(pageCtx)
		if next != "" {
			call = call.PageToken(next)
		}
		response, err := call.Do()
		if err != nil {
			return authorityError(pageCtx, err)
		}
		if observe != nil {
			if !binding.Set {
				return fmt.Errorf("request credential binding unavailable")
			}
			// Start the local deadline only after a response supplies evidence.
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(pageCtx), 5*time.Second)
			var observationErrors []error
			for _, c := range response.Items {
				if c != nil && c.Id != "" {
					if err := observe(closeCtx, c.Id, c.AccessRole, binding.Revision, binding.Digest); err != nil {
						observationErrors = append(observationErrors, err)
					}
				}
			}
			cancel()
			if err := errors.Join(observationErrors...); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := consume(response, binding); err != nil {
			return err
		}
		next = response.NextPageToken
		if next == "" {
			return nil
		}
		if tokens[next] {
			return fmt.Errorf("repeated calendar page token")
		}
		tokens[next] = true
	}
}

func (p *Provider) GetEvents(ctx context.Context, calendarID string, start, end time.Time) ([]calendar.Event, error) {
	call := p.svc.Events.List(calendarID).
		TimeMin(start.Format(time.RFC3339)).
		TimeMax(end.Format(time.RFC3339)).
		SingleEvents(true).
		OrderBy("startTime")
	var result []calendar.Event
	ctx, observe, err := p.startRoleObservation(ctx, calendarID)
	if err != nil {
		return nil, err
	}
	err = call.Pages(ctx, func(events *gcal.Events) error {
		if err := observeRole(ctx, observe, events.AccessRole); err != nil {
			return err
		}
		for _, e := range events.Items {
			result = append(result, convertEvent(e, calendarID))
		}
		return nil
	})
	if err != nil {
		return nil, authorityError(ctx, err)
	}
	return result, nil
}

func (p *Provider) CreateEvent(ctx context.Context, calendarID string, event calendar.EventCreate) (*calendar.Event, error) {
	ge := &gcal.Event{
		Summary:     event.Title,
		Description: event.Description,
		Location:    event.Location,
		Start:       toGoogleEventTime(event.Start, event.AllDay),
		End:         toGoogleEventTime(event.End, event.AllDay),
		Attendees:   toGoogleAttendees(event.Attendees),
	}
	if event.VideoCall {
		ge.ConferenceData = &gcal.ConferenceData{
			CreateRequest: &gcal.CreateConferenceRequest{
				RequestId:             fmt.Sprintf("book-%d", time.Now().UnixNano()),
				ConferenceSolutionKey: &gcal.ConferenceSolutionKey{Type: "hangoutsMeet"},
			},
		}
	}
	insertCall := p.svc.Events.Insert(calendarID, ge).SendUpdates("none").Context(ctx)
	if event.VideoCall {
		insertCall = insertCall.ConferenceDataVersion(1)
	}
	created, err := insertCall.Do()
	if err != nil {
		return nil, err
	}
	ev := convertEvent(created, calendarID)
	return &ev, nil
}

func (p *Provider) UpdateEvent(ctx context.Context, calendarID, eventID string, event calendar.EventUpdate) (*calendar.Event, error) {
	existing, err := p.svc.Events.Get(calendarID, eventID).Context(ctx).Do()
	if err != nil {
		return nil, calendar.DefinitiveNoDispatch(err)
	}
	if event.Title != nil {
		existing.Summary = *event.Title
	}
	if event.Description != nil {
		existing.Description = *event.Description
	}
	if event.Location != nil {
		existing.Location = *event.Location
	}
	allDay := false
	if event.AllDay != nil {
		allDay = *event.AllDay
	} else if existing.Start != nil && existing.Start.Date != "" {
		allDay = true
	}
	if event.Start != nil {
		existing.Start = toGoogleEventTime(*event.Start, allDay)
	}
	if event.End != nil {
		existing.End = toGoogleEventTime(*event.End, allDay)
	}
	if event.Attendees != nil {
		existing.Attendees = toGoogleAttendees(*event.Attendees)
		if len(*event.Attendees) == 0 {
			existing.ForceSendFields = append(existing.ForceSendFields, "Attendees")
		}
	}
	updated, err := p.svc.Events.Update(calendarID, eventID, existing).SendUpdates("none").Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	ev := convertEvent(updated, calendarID)
	return &ev, nil
}

func (p *Provider) DeleteEvent(ctx context.Context, calendarID, eventID string) error {
	return p.svc.Events.Delete(calendarID, eventID).SendUpdates("none").Context(ctx).Do()
}

func toGoogleEventTime(t time.Time, allDay bool) *gcal.EventDateTime {
	if allDay {
		return &gcal.EventDateTime{Date: t.Format(calendar.DateLayout)}
	}
	return &gcal.EventDateTime{DateTime: t.Format(time.RFC3339)}
}

func toGoogleAttendees(attendees []calendar.Attendee) []*gcal.EventAttendee {
	if len(attendees) == 0 {
		return nil
	}
	var out []*gcal.EventAttendee
	for _, a := range attendees {
		out = append(out, &gcal.EventAttendee{
			Email:       a.Email,
			DisplayName: a.Name,
			Optional:    a.Optional,
		})
	}
	return out
}

func fromGoogleAttendees(attendees []*gcal.EventAttendee) []calendar.Attendee {
	if len(attendees) == 0 {
		return nil
	}
	var out []calendar.Attendee
	for _, a := range attendees {
		out = append(out, calendar.Attendee{
			Email:    a.Email,
			Name:     a.DisplayName,
			Status:   a.ResponseStatus,
			Optional: a.Optional,
		})
	}
	return out
}

func convertEvent(e *gcal.Event, calendarID string) calendar.Event {
	ev := calendar.Event{
		ID:          e.Id,
		CalendarID:  calendarID,
		Title:       e.Summary,
		Description: e.Description,
		Location:    e.Location,
		Status:      e.Status,
		Attendees:   fromGoogleAttendees(e.Attendees),
	}
	if e.Start != nil {
		if e.Start.DateTime != "" {
			ev.Start, _ = time.Parse(time.RFC3339, e.Start.DateTime)
		} else if e.Start.Date != "" {
			ev.Start, _ = time.Parse("2006-01-02", e.Start.Date)
			ev.AllDay = true
		}
	}
	if e.End != nil {
		if e.End.DateTime != "" {
			ev.End, _ = time.Parse(time.RFC3339, e.End.DateTime)
		} else if e.End.Date != "" {
			ev.End, _ = time.Parse("2006-01-02", e.End.Date)
		}
	}
	if e.ConferenceData != nil && len(e.ConferenceData.EntryPoints) > 0 {
		for _, ep := range e.ConferenceData.EntryPoints {
			if ep.EntryPointType == "video" {
				ev.OnlineMeeting = ep.Uri
				break
			}
		}
	}
	return ev
}
