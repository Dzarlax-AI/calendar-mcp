package calendar

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Registry struct {
	providers       []Provider
	byName          map[string]Provider
	excludeIDs      map[string]struct{} // prefixed IDs to skip in fan-out
	includeImported bool                // if false, skip google:*@import.calendar.google.com in fan-out
}

type RegistryOptions struct {
	ExcludeIDs               []string
	IncludeImportedCalendars bool
}

func NewRegistry(providers []Provider, opts ...RegistryOptions) *Registry {
	byName := make(map[string]Provider, len(providers))
	for _, p := range providers {
		byName[p.Name()] = p
	}
	r := &Registry{providers: providers, byName: byName, excludeIDs: map[string]struct{}{}}
	if len(opts) > 0 {
		for _, id := range opts[0].ExcludeIDs {
			r.excludeIDs[id] = struct{}{}
		}
		r.includeImported = opts[0].IncludeImportedCalendars
	}
	return r
}

// skipInFanOut reports whether a prefixed calendar ID should be dropped from
// fan-out queries. It does NOT affect list_calendars or explicit calendar_id
// requests — users can always query excluded calendars directly.
func (r *Registry) skipInFanOut(prefixedID string) bool {
	if _, ok := r.excludeIDs[prefixedID]; ok {
		return true
	}
	if !r.includeImported && strings.HasPrefix(prefixedID, "google:") && strings.Contains(prefixedID, "@import.calendar.google.com") {
		return true
	}
	return false
}

func (r *Registry) SkipInFanOut(prefixedID string) bool {
	return r.skipInFanOut(prefixedID)
}

func (r *Registry) ListCalendars(ctx context.Context) ([]Calendar, error) {
	var all []Calendar
	for _, p := range r.providers {
		cals, err := p.ListCalendars(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Name(), err)
		}
		for i := range cals {
			cals[i].ID = p.Name() + ":" + cals[i].ID
			cals[i].Provider = p.Name()
		}
		all = append(all, cals...)
	}
	return all, nil
}

func (r *Registry) GetEvents(ctx context.Context, calendarID string, start, end time.Time) ([]Event, error) {
	if calendarID != "" {
		provider, rawID, err := r.resolve(calendarID)
		if err != nil {
			return nil, err
		}
		events, err := provider.GetEvents(ctx, rawID, start, end)
		if err != nil {
			return nil, err
		}
		prefixEvents(events, provider.Name())
		return events, nil
	}

	// Fan-out to all providers concurrently.
	type result struct {
		events []Event
		errs   []error
	}
	ch := make(chan result, len(r.providers))
	var wg sync.WaitGroup
	for _, p := range r.providers {
		wg.Add(1)
		go func(p Provider) {
			defer wg.Done()
			cals, err := p.ListCalendars(ctx)
			if err != nil {
				ch <- result{errs: []error{fmt.Errorf("%s: %w", p.Name(), err)}}
				return
			}
			var all []Event
			var providerErrs []error
			for _, cal := range cals {
				if r.skipInFanOut(p.Name() + ":" + cal.ID) {
					continue
				}
				events, err := p.GetEvents(ctx, cal.ID, start, end)
				if err != nil {
					providerErrs = append(providerErrs, fmt.Errorf("%s:%s: %w", p.Name(), cal.ID, err))
					continue
				}
				all = append(all, events...)
			}
			prefixEvents(all, p.Name())
			ch <- result{events: all, errs: providerErrs}
		}(p)
	}
	go func() { wg.Wait(); close(ch) }()

	var all []Event
	var fanOutErrs []error
	for res := range ch {
		all = append(all, res.events...)
		fanOutErrs = append(fanOutErrs, res.errs...)
	}
	if len(fanOutErrs) > 0 {
		return nil, fmt.Errorf("get events fan-out incomplete: %w", errors.Join(fanOutErrs...))
	}
	return all, nil
}

func (r *Registry) CreateEvent(ctx context.Context, calendarID string, event EventCreate) (*Event, error) {
	provider, rawID, err := r.resolve(calendarID)
	if err != nil {
		return nil, err
	}
	ev, err := provider.CreateEvent(ctx, rawID, event)
	if err != nil {
		return nil, err
	}
	ev.CalendarID = calendarID
	ev.Provider = provider.Name()
	ev.ID = provider.Name() + ":" + ev.ID
	return ev, nil
}

func (r *Registry) UpdateEvent(ctx context.Context, calendarID, eventID string, event EventUpdate) (*Event, error) {
	provider, rawCalID, err := r.resolve(calendarID)
	if err != nil {
		return nil, err
	}
	rawEventID, err := validateEventProvider(provider.Name(), eventID)
	if err != nil {
		return nil, err
	}
	ev, err := provider.UpdateEvent(ctx, rawCalID, rawEventID, event)
	if err != nil {
		return nil, err
	}
	ev.CalendarID = calendarID
	ev.Provider = provider.Name()
	ev.ID = provider.Name() + ":" + ev.ID
	return ev, nil
}

func (r *Registry) DeleteEvent(ctx context.Context, calendarID, eventID string) error {
	provider, rawCalID, err := r.resolve(calendarID)
	if err != nil {
		return err
	}
	rawEventID, err := validateEventProvider(provider.Name(), eventID)
	if err != nil {
		return err
	}
	return provider.DeleteEvent(ctx, rawCalID, rawEventID)
}

func validateEventProvider(providerName, eventID string) (string, error) {
	eventProvider, rawEventID := splitPrefix(eventID)
	if eventProvider == "" {
		if rawEventID == "" {
			return "", fmt.Errorf("event ID is empty")
		}
		return rawEventID, nil
	}
	if eventProvider != providerName {
		return "", fmt.Errorf("event provider %q does not match calendar provider %q", eventProvider, providerName)
	}
	if rawEventID == "" {
		return "", fmt.Errorf("event ID is empty")
	}
	return rawEventID, nil
}

func (r *Registry) resolve(prefixedID string) (Provider, string, error) {
	name, rawID := splitPrefix(prefixedID)
	p, ok := r.byName[name]
	if !ok {
		return nil, "", fmt.Errorf("unknown provider: %s", name)
	}
	return p, rawID, nil
}

func (r *Registry) Resolve(prefixedID string) (Provider, string, error) {
	return r.resolve(prefixedID)
}

func (r *Registry) Providers() []Provider {
	return append([]Provider(nil), r.providers...)
}

func splitPrefix(id string) (string, string) {
	parts := strings.SplitN(id, ":", 2)
	if len(parts) != 2 {
		return "", id
	}
	return parts[0], parts[1]
}

func prefixEvents(events []Event, provider string) {
	for i := range events {
		events[i].ID = provider + ":" + events[i].ID
		events[i].CalendarID = provider + ":" + events[i].CalendarID
		events[i].Provider = provider
	}
}
