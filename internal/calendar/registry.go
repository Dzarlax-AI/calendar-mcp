package calendar

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

type Registry struct {
	providers []Provider
	byName    map[string]Provider
}

func NewRegistry(providers []Provider) *Registry {
	byName := make(map[string]Provider, len(providers))
	for _, p := range providers {
		byName[p.Name()] = p
	}
	return &Registry{providers: providers, byName: byName}
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
		err    error
	}
	ch := make(chan result, len(r.providers))
	var wg sync.WaitGroup
	for _, p := range r.providers {
		wg.Add(1)
		go func(p Provider) {
			defer wg.Done()
			cals, err := p.ListCalendars(ctx)
			if err != nil {
				ch <- result{err: fmt.Errorf("%s: %w", p.Name(), err)}
				return
			}
			var all []Event
			for _, cal := range cals {
				events, err := p.GetEvents(ctx, cal.ID, start, end)
				if err != nil {
					ch <- result{err: fmt.Errorf("%s/%s: %w", p.Name(), cal.ID, err)}
					return
				}
				all = append(all, events...)
			}
			prefixEvents(all, p.Name())
			ch <- result{events: all}
		}(p)
	}
	go func() { wg.Wait(); close(ch) }()

	var all []Event
	for res := range ch {
		if res.err != nil {
			log.Printf("GetEvents fan-out (skipping): %v", res.err)
			continue
		}
		all = append(all, res.events...)
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
	_, rawEventID := splitPrefix(eventID)
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
	_, rawEventID := splitPrefix(eventID)
	return provider.DeleteEvent(ctx, rawCalID, rawEventID)
}

func (r *Registry) resolve(prefixedID string) (Provider, string, error) {
	name, rawID := splitPrefix(prefixedID)
	p, ok := r.byName[name]
	if !ok {
		return nil, "", fmt.Errorf("unknown provider: %s", name)
	}
	return p, rawID, nil
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
