package calendar

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
)

type Registry struct {
	mu              sync.RWMutex
	providers       []Provider
	byName          map[string]Provider
	excludeIDs      map[string]struct{} // prefixed IDs to skip in fan-out
	refresh         func(context.Context, Provider) (Provider, error)
	discover        func(context.Context, string, map[string]bool) ([]Provider, error)
	googleOwner     func(context.Context, string) (connectionID, revision string, err error)
	generation      uint64
	includeImported bool // if false, skip google:*@import.calendar.google.com in fan-out
	selection       CalendarSelection
	writeGuard      WriteDispatchGuard
}

type CalendarSelection interface {
	CalendarEnabled(context.Context, string) (bool, error)
}

var ErrCalendarSelectionUnavailable = errors.New("calendar selection unavailable")

// CalendarDiscovery records calendars returned by a provider without changing
// existing user choices. Only actual provider discovery calls this interface.
type CalendarDiscovery interface {
	RegisterDiscoveredCalendar(context.Context, string, Calendar) error
}

type RegistryOptions struct {
	ExcludeIDs               []string
	IncludeImportedCalendars bool
	Selection                CalendarSelection
	WriteGuard               WriteDispatchGuard
}

func NewRegistry(providers []Provider, opts ...RegistryOptions) *Registry {
	byName := make(map[string]Provider, len(providers))
	for _, p := range providers {
		byName[ProviderRouteName(p)] = p
	}
	r := &Registry{providers: providers, byName: byName, excludeIDs: map[string]struct{}{}}
	if len(opts) > 0 {
		for _, id := range opts[0].ExcludeIDs {
			r.excludeIDs[id] = struct{}{}
		}
		r.includeImported = opts[0].IncludeImportedCalendars
		r.selection = opts[0].Selection
		r.writeGuard = opts[0].WriteGuard
	}
	return r
}

func (r *Registry) CalendarEnabled(ctx context.Context, provider Provider, rawID string) (bool, error) {
	if r.selection == nil {
		return true, nil
	}
	return r.selection.CalendarEnabled(ctx, CanonicalCalendarID(provider, rawID))
}

func (r *Registry) DiscoveredCalendarEnabled(ctx context.Context, provider Provider, discovered Calendar) (bool, error) {
	route := ProviderRouteName(provider)
	if bound, ok := provider.(interface{ CredentialBinding() (string, [32]byte) }); ok && route != provider.Name() {
		if discovery, ok := r.selection.(interface {
			RegisterDiscoveredCalendarWithRevision(context.Context, string, Calendar, string) error
		}); ok {
			revision, _ := bound.CredentialBinding()
			if err := discovery.RegisterDiscoveredCalendarWithRevision(ctx, route, discovered, revision); err != nil {
				return false, err
			}
			return r.CalendarEnabled(ctx, provider, discovered.ID)
		}
	}
	if discovery, ok := r.selection.(CalendarDiscovery); ok {
		if err := discovery.RegisterDiscoveredCalendar(ctx, route, discovered); err != nil {
			return false, err
		}
	}
	return r.CalendarEnabled(ctx, provider, discovered.ID)
}

func (r *Registry) ResolveEnabled(ctx context.Context, id string) (Provider, string, error) {
	provider, raw, err := r.ResolveContext(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if raw == "primary" && provider.Name() == "google" {
		accountID, scoped := strings.CutPrefix(ProviderRouteName(provider), "google@")
		bound, boundOK := provider.(interface{ CredentialBinding() (string, [32]byte) })
		primary, primaryOK := r.selection.(interface {
			ResolveGooglePrimaryCalendarID(context.Context, string, string) (string, error)
		})
		if scoped && boundOK && primaryOK {
			revision, _ := bound.CredentialBinding()
			raw, err = primary.ResolveGooglePrimaryCalendarID(ctx, accountID, revision)
			if err != nil {
				return nil, "", fmt.Errorf("%w: %w", ErrCalendarSelectionUnavailable, err)
			}
		}
	}
	enabled, err := r.CalendarEnabled(ctx, provider, raw)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrCalendarSelectionUnavailable, err)
	}
	if !enabled {
		return nil, "", fmt.Errorf("calendar is disabled or unavailable")
	}
	return provider, raw, nil
}

// SetGoogleOwnerLookup installs the durable platform short-ID owner lookup.
func (r *Registry) SetGoogleOwnerLookup(lookup func(context.Context, string) (connectionID, revision string, err error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.googleOwner = lookup
}

// SetProviderDiscovery installs metadata-only discovery for missing routes.
func (r *Registry) SetProviderDiscovery(discover func(context.Context, string, map[string]bool) ([]Provider, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.discover = discover
}

func (r *Registry) discoverProviders(ctx context.Context, id string) error {
	r.mu.RLock()
	discover, generation := r.discover, r.generation
	known := make(map[string]bool, len(r.byName))
	for name := range r.byName {
		known[name] = true
	}
	r.mu.RUnlock()
	if discover == nil {
		return nil
	}
	candidates, err := discover(ctx, id, known)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.generation != generation {
		return nil
	}
	for _, p := range candidates {
		route := ProviderRouteName(p)
		if id != "" && route != "google@"+id {
			continue
		}
		if _, exists := r.byName[route]; exists {
			continue
		}
		r.byName[route] = p
		r.providers = append(r.providers, p)
	}
	return nil
}

// ProvidersContext discovers newly ready routes before a list or fan-out.
func (r *Registry) ProvidersContext(ctx context.Context) ([]Provider, error) {
	if err := r.discoverProviders(ctx, ""); err != nil {
		return nil, err
	}
	return r.Providers(), nil
}

// SetProviderRefresh installs a snapshot check for platform providers.
func (r *Registry) SetProviderRefresh(refresh func(context.Context, Provider) (Provider, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh = refresh
}

func sameProvider(a, b Provider) bool {
	return a != nil && b != nil && reflect.TypeOf(a) == reflect.TypeOf(b) && reflect.ValueOf(a).Comparable() && a == b
}

// RefreshProvider conditionally replaces only the observed route, preserving all
// other registry entries. A concurrent replacement wins without replaying I/O.
func (r *Registry) RefreshProvider(ctx context.Context, old Provider) (Provider, error) {
	r.mu.RLock()
	refresh := r.refresh
	r.mu.RUnlock()
	if refresh == nil {
		return old, nil
	}
	next, err := refresh(ctx, old)
	if err != nil {
		return nil, err
	}
	if sameProvider(next, old) {
		r.mu.RLock()
		current := r.byName[ProviderRouteName(old)]
		r.mu.RUnlock()
		if !sameProvider(current, old) {
			return nil, fmt.Errorf("provider route changed during refresh")
		}
		return old, nil
	}
	route := ProviderRouteName(old)
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.byName[route]
	if !sameProvider(current, old) {
		return nil, fmt.Errorf("provider route changed during refresh")
	}
	if ProviderRouteName(next) != route {
		return nil, fmt.Errorf("refreshed provider route mismatch")
	}
	r.byName[route] = next
	for i, p := range r.providers {
		if sameProvider(p, old) {
			r.providers[i] = next
			break
		}
	}
	return next, nil
}
func (r *Registry) ResolveContext(ctx context.Context, id string) (Provider, string, error) {
	name, tail := splitPrefix(id)
	account, raw, scoped := strings.Cut(tail, ":")
	r.mu.RLock()
	discovery := r.discover != nil
	ownerLookup := r.googleOwner
	r.mu.RUnlock()
	var p Provider
	var err error
	var ownerRevision string
	var ownerSelected bool
	if name == "google" && !scoped && ownerLookup != nil {
		owner, revision, lookupErr := ownerLookup(ctx, tail)
		if lookupErr != nil {
			return nil, "", lookupErr
		}
		if owner == "" {
			return nil, "", fmt.Errorf("unknown provider or calendar: %s", name)
		}
		account, raw, scoped, ownerRevision = owner, tail, true, revision
		ownerSelected = true
	}
	if (discovery || ownerSelected) && name == "google" && scoped && account != "" {
		route := "google@" + account
		r.mu.RLock()
		p = r.byName[route]
		r.mu.RUnlock()
		if p == nil && discovery {
			if err = r.discoverProviders(ctx, account); err != nil {
				return nil, "", err
			}
			r.mu.RLock()
			p = r.byName[route]
			r.mu.RUnlock()
		}
		if p == nil {
			return nil, "", fmt.Errorf("unknown provider or calendar: %s", name)
		}
	} else {
		p, raw, err = r.resolve(id)
		if err != nil {
			return nil, "", err
		}
	}

	p, err = r.RefreshProvider(ctx, p)
	if err != nil || !ownerSelected {
		return p, raw, err
	}
	if bound, ok := p.(interface{ CredentialBinding() (string, [32]byte) }); ok {
		if revision, _ := bound.CredentialBinding(); revision == ownerRevision {
			return p, raw, nil
		}
	}
	return nil, "", fmt.Errorf("calendar authority changed during route resolution")
}

// skipInFanOut reports whether a prefixed calendar ID should be dropped from
// fan-out queries. It does NOT affect list_calendars or explicit calendar_id
// requests — users can always query excluded calendars directly.
func (r *Registry) skipInFanOut(prefixedID string) bool {
	if _, ok := r.excludeIDs[prefixedID]; ok {
		return true
	}
	if !r.includeImported && (strings.HasPrefix(prefixedID, "google:") || strings.HasPrefix(prefixedID, "google@")) && strings.Contains(prefixedID, "@import.calendar.google.com") {
		return true
	}
	return false
}

func (r *Registry) SkipInFanOut(prefixedID string) bool {
	return r.skipInFanOut(prefixedID)
}

func (r *Registry) ListCalendars(ctx context.Context) ([]Calendar, error) {
	providers, err := r.ProvidersContext(ctx)
	if err != nil {
		return nil, err
	}
	type result struct {
		calendars []Calendar
		err       error
	}
	results := make([]result, len(providers))
	workers := min(4, len(providers))
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := worker; i < len(providers); i += workers {
				if err := ctx.Err(); err != nil {
					results[i].err = err
					continue
				}
				provider, err := r.RefreshProvider(ctx, providers[i])
				if err != nil {
					results[i].err = err
					continue
				}
				cals, err := provider.ListCalendars(ctx)
				if err != nil {
					results[i].err = err
					continue
				}
				// Providers may retain their raw slice; canonicalize only our own copy.
				copied := append([]Calendar(nil), cals...)
				for j := range copied {
					enabled, selectionErr := r.DiscoveredCalendarEnabled(ctx, provider, copied[j])
					if selectionErr != nil {
						results[i].err = selectionErr
						break
					}
					if !enabled {
						copied[j].ID = ""
						continue
					}
					copied[j].ID = CanonicalCalendarID(provider, copied[j].ID)
					copied[j].Provider = provider.Name()
				}
				results[i].calendars = copied
			}
		}(worker)
	}
	wg.Wait()
	var all []Calendar
	for i, result := range results {
		if result.err != nil {
			return nil, fmt.Errorf("%s: %w", providers[i].Name(), result.err)
		}
		for _, c := range result.calendars {
			if c.ID != "" {
				all = append(all, c)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return all, nil
}

func (r *Registry) GetEvents(ctx context.Context, calendarID string, start, end time.Time) ([]Event, error) {
	if calendarID != "" {
		provider, rawID, err := r.ResolveEnabled(ctx, calendarID)
		if err != nil {
			return nil, err
		}
		events, err := provider.GetEvents(ctx, rawID, start, end)
		if err != nil {
			return nil, err
		}
		prefixEvents(events, provider)
		return events, nil
	}

	// Fan-out to all providers concurrently.
	type result struct {
		events []Event
		errs   []error
	}
	providers, err := r.ProvidersContext(ctx)
	if err != nil {
		return nil, err
	}
	ch := make(chan result, len(providers))
	var wg sync.WaitGroup
	for _, p := range providers {
		wg.Add(1)
		go func(p Provider) {
			defer wg.Done()
			p, err := r.RefreshProvider(ctx, p)
			if err != nil {
				ch <- result{errs: []error{err}}
				return
			}
			cals, err := p.ListCalendars(ctx)
			if err != nil {
				ch <- result{errs: []error{fmt.Errorf("%s: %w", p.Name(), err)}}
				return
			}
			var all []Event
			var providerErrs []error
			for _, cal := range cals {
				canonicalID := CanonicalCalendarID(p, cal.ID)
				legacyID := p.Name() + ":" + cal.ID
				if r.skipInFanOut(canonicalID) || r.skipInFanOut(legacyID) {
					continue
				}
				enabled, err := r.DiscoveredCalendarEnabled(ctx, p, cal)
				if err != nil {
					providerErrs = append(providerErrs, err)
					continue
				}
				if !enabled {
					continue
				}
				events, err := p.GetEvents(ctx, cal.ID, start, end)
				if err != nil {
					providerErrs = append(providerErrs, fmt.Errorf("%s:%s: %w", p.Name(), cal.ID, err))
					continue
				}
				all = append(all, events...)
			}
			prefixEvents(all, p)
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
	provider, rawID, err := r.ResolveEnabled(ctx, calendarID)
	if err != nil {
		return nil, err
	}
	ev, err := GuardedWrite(ctx, r.writeGuard, []string{CanonicalCalendarID(provider, rawID)}, func() (*Event, error) { return provider.CreateEvent(ctx, rawID, event) }, func(ev *Event) bool { return ev != nil && ev.ID != "" })
	if err != nil {
		return nil, err
	}
	if ev == nil {
		return nil, ErrProviderMutationOutcomeUnknown
	}
	ev.CalendarID = calendarID
	ev.Provider = provider.Name()
	ev.ID = provider.Name() + ":" + ev.ID
	return ev, nil
}

func (r *Registry) UpdateEvent(ctx context.Context, calendarID, eventID string, event EventUpdate) (*Event, error) {
	provider, rawCalID, err := r.ResolveEnabled(ctx, calendarID)
	if err != nil {
		return nil, err
	}
	rawEventID, err := validateEventProvider(ProviderRouteName(provider), provider.Name(), eventID)
	if err != nil {
		return nil, err
	}
	ev, err := GuardedWrite(ctx, r.writeGuard, []string{CanonicalCalendarID(provider, rawCalID)}, func() (*Event, error) { return provider.UpdateEvent(ctx, rawCalID, rawEventID, event) }, func(ev *Event) bool { return ev != nil && ev.ID != "" })
	if err != nil {
		return nil, err
	}
	if ev == nil {
		return nil, ErrProviderMutationOutcomeUnknown
	}
	ev.CalendarID = calendarID
	ev.Provider = provider.Name()
	ev.ID = provider.Name() + ":" + ev.ID
	return ev, nil
}

func (r *Registry) DeleteEvent(ctx context.Context, calendarID, eventID string) error {
	provider, rawCalID, err := r.ResolveEnabled(ctx, calendarID)
	if err != nil {
		return err
	}
	rawEventID, err := validateEventProvider(ProviderRouteName(provider), provider.Name(), eventID)
	if err != nil {
		return err
	}
	_, err = GuardedWrite(ctx, r.writeGuard, []string{CanonicalCalendarID(provider, rawCalID)}, func() (bool, error) { err := provider.DeleteEvent(ctx, rawCalID, rawEventID); return err == nil, err }, func(ok bool) bool { return ok })
	if err != nil {
		return err
	}
	return nil
}

func validateEventProvider(routeName, providerName, eventID string) (string, error) {
	eventProvider, rawEventID := splitPrefix(eventID)
	if eventProvider == "" {
		if rawEventID == "" {
			return "", fmt.Errorf("event ID is empty")
		}
		return rawEventID, nil
	}
	if eventProvider != routeName && eventProvider != providerName {
		return "", fmt.Errorf("event provider %q does not match calendar provider %q", eventProvider, providerName)
	}
	if rawEventID == "" {
		return "", fmt.Errorf("event ID is empty")
	}
	return rawEventID, nil
}

func (r *Registry) resolve(prefixedID string) (Provider, string, error) {
	name, rawID := splitPrefix(prefixedID)
	r.mu.RLock()
	p, ok := r.byName[name]
	if !ok {
		if connectionID, providerCalendarID, scoped := strings.Cut(rawID, ":"); scoped {
			p, ok = r.byName[name+"@"+connectionID]
			if ok {
				r.mu.RUnlock()
				return p, providerCalendarID, nil
			}
		}
	}
	providers := append([]Provider(nil), r.providers...)
	r.mu.RUnlock()
	if ok {
		return p, rawID, nil
	}
	var matched Provider
	for _, candidate := range providers {
		if candidate.Name() != name || !ProviderOwnsCalendar(candidate, rawID) {
			continue
		}
		if matched != nil {
			return nil, "", fmt.Errorf("calendar %q is ambiguous across multiple %s accounts; use an account-scoped calendar ID", prefixedID, name)
		}
		matched = candidate
	}
	if matched == nil {
		return nil, "", fmt.Errorf("unknown provider or calendar: %s", name)
	}
	return matched, rawID, nil
}

func (r *Registry) Resolve(prefixedID string) (Provider, string, error) {
	return r.resolve(prefixedID)
}

func (r *Registry) Providers() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Provider(nil), r.providers...)
}

func (r *Registry) ReplaceProviders(providers []Provider) {
	byName := make(map[string]Provider, len(providers))
	for _, provider := range providers {
		byName[ProviderRouteName(provider)] = provider
	}
	r.mu.Lock()
	r.providers = append([]Provider(nil), providers...)
	r.generation++
	r.byName = byName
	r.mu.Unlock()
}

func splitPrefix(id string) (string, string) {
	parts := strings.SplitN(id, ":", 2)
	if len(parts) != 2 {
		return "", id
	}
	return parts[0], parts[1]
}

func prefixEvents(events []Event, provider Provider) {
	for i := range events {
		events[i].ID = provider.Name() + ":" + events[i].ID
		events[i].CalendarID = CanonicalCalendarID(provider, events[i].CalendarID)
		events[i].Provider = provider.Name()
	}
}

// WriteGuard returns the registry-wide provider dispatch guard.
func (r *Registry) WriteGuard() WriteDispatchGuard { return r.writeGuard }
