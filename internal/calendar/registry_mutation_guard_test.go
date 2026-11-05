package calendar

import (
	"bytes"
	"context"
	"errors"
	"log"
	"reflect"
	"strings"
	"testing"
)

type registryMutationGuard struct {
	ids                  []string
	calls                []string
	beginErr, resolveErr error
	nilTicket            bool
}

func (g *registryMutationGuard) BeginProviderMutation(_ context.Context, ids []string) (ProviderMutationIntent, error) {
	g.calls = append(g.calls, "begin")
	g.ids = append([]string(nil), ids...)
	if g.beginErr != nil {
		return nil, g.beginErr
	}
	if g.nilTicket {
		return nil, nil
	}
	return g, nil
}
func (g *registryMutationGuard) ResolveProviderMutation(context.Context) error {
	g.calls = append(g.calls, "resolve")
	return g.resolveErr
}

type registryGuardProvider struct {
	fakeProvider
	guard                 *registryMutationGuard
	providerErr           error
	acceptedBeforeFailure bool
	emptyResponse         bool
	calls                 int
}

func (p *registryGuardProvider) RouteName() string           { return "google@account" }
func (p *registryGuardProvider) OwnsCalendar(id string) bool { return id == "primary" }
func (p *registryGuardProvider) dispatch() error {
	p.calls++
	if p.guard != nil {
		p.guard.calls = append(p.guard.calls, "provider")
	}
	if p.providerErr != nil {
		p.acceptedBeforeFailure = true
	}
	return p.providerErr
}
func (p *registryGuardProvider) CreateEvent(context.Context, string, EventCreate) (*Event, error) {
	if err := p.dispatch(); err != nil {
		return nil, err
	}
	if p.emptyResponse {
		return &Event{}, nil
	}
	return &Event{ID: "created"}, nil
}
func (p *registryGuardProvider) UpdateEvent(context.Context, string, string, EventUpdate) (*Event, error) {
	if err := p.dispatch(); err != nil {
		return nil, err
	}
	if p.emptyResponse {
		return &Event{}, nil
	}
	return &Event{ID: "updated"}, nil
}
func (p *registryGuardProvider) DeleteEvent(context.Context, string, string) error {
	return p.dispatch()
}

func TestRegistryMutationGuardFencesLegacyWrites(t *testing.T) {
	for _, operation := range []string{"create", "update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			run := func(r *Registry) error {
				switch operation {
				case "create":
					_, err := r.CreateEvent(t.Context(), "google:account:primary", EventCreate{})
					return err
				case "update":
					_, err := r.UpdateEvent(t.Context(), "google:account:primary", "google:event", EventUpdate{})
					return err
				default:
					return r.DeleteEvent(t.Context(), "google:account:primary", "google:event")
				}
			}
			t.Run("success canonical and order", func(t *testing.T) {
				g := &registryMutationGuard{}
				p := &registryGuardProvider{fakeProvider: fakeProvider{name: "google"}, guard: g}
				r := NewRegistry([]Provider{p}, RegistryOptions{MutationGuard: g})
				if err := run(r); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(g.ids, []string{"google:account:primary"}) || !reflect.DeepEqual(g.calls, []string{"begin", "provider", "resolve"}) {
					t.Fatalf("fence %v %v", g.ids, g.calls)
				}
			})
			t.Run("begin failure prevents dispatch", func(t *testing.T) {
				g := &registryMutationGuard{beginErr: errors.New("unavailable")}
				p := &registryGuardProvider{fakeProvider: fakeProvider{name: "google"}, guard: g}
				r := NewRegistry([]Provider{p}, RegistryOptions{MutationGuard: g})
				if err := run(r); err == nil || p.calls != 0 {
					t.Fatal("begin failure dispatched provider")
				}
			})
			t.Run("nil ticket prevents dispatch", func(t *testing.T) {
				g := &registryMutationGuard{nilTicket: true}
				p := &registryGuardProvider{fakeProvider: fakeProvider{name: "google"}, guard: g}
				r := NewRegistry([]Provider{p}, RegistryOptions{MutationGuard: g})
				if err := run(r); err == nil || p.calls != 0 {
					t.Fatal("nil ticket dispatched provider")
				}
			})
			t.Run("provider error remains unresolved", func(t *testing.T) {
				g := &registryMutationGuard{}
				p := &registryGuardProvider{fakeProvider: fakeProvider{name: "google"}, guard: g, providerErr: errors.New("ambiguous")}
				r := NewRegistry([]Provider{p}, RegistryOptions{MutationGuard: g})
				if err := run(r); err == nil || !reflect.DeepEqual(g.calls, []string{"begin", "provider"}) {
					t.Fatalf("provider error resolved intent: %v", g.calls)
				}
			})
			t.Run("resolve failure preserves confirmed success", func(t *testing.T) {
				g := &registryMutationGuard{resolveErr: errors.New("storage failure")}
				p := &registryGuardProvider{fakeProvider: fakeProvider{name: "google"}, guard: g}
				observed := 0
				r := NewRegistry([]Provider{p}, RegistryOptions{MutationGuard: g, MutationGuardResolveFailure: func() { observed++ }})
				if err := run(r); err != nil || observed != 1 || p.calls != 1 {
					t.Fatalf("success altered or failure not observed: %v %d", err, observed)
				}
			})
			if operation != "delete" {
				t.Run("missing provider identity remains unresolved", func(t *testing.T) {
					g := &registryMutationGuard{}
					p := &registryGuardProvider{fakeProvider: fakeProvider{name: "google"}, guard: g, emptyResponse: true}
					r := NewRegistry([]Provider{p}, RegistryOptions{MutationGuard: g})
					if err := run(r); !errors.Is(err, ErrProviderMutationOutcomeUnknown) || !reflect.DeepEqual(g.calls, []string{"begin", "provider"}) {
						t.Fatalf("missing event identity: %v %v", err, g.calls)
					}
				})
			}
			t.Run("nil guard preserves provider path", func(t *testing.T) {
				p := &registryGuardProvider{fakeProvider: fakeProvider{name: "google"}}
				r := NewRegistry([]Provider{p})
				if err := run(r); err != nil || p.calls != 1 {
					t.Fatalf("nil guard: %v", err)
				}
			})
		})
	}
}

func TestRegistryMutationGuardCanonicalizesUnambiguousLegacyAlias(t *testing.T) {
	g := &registryMutationGuard{}
	p := &registryGuardProvider{fakeProvider: fakeProvider{name: "google"}, guard: g}
	r := NewRegistry([]Provider{p}, RegistryOptions{MutationGuard: g})
	if _, err := r.CreateEvent(t.Context(), "google:primary", EventCreate{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g.ids, []string{"google:account:primary"}) {
		t.Fatalf("guard received noncanonical IDs: %v", g.ids)
	}
}

func TestRegistryMutationGuardLogsStaticFallbackWithoutObserver(t *testing.T) {
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(old) })
	g := &registryMutationGuard{resolveErr: errors.New("private-provider-identifier")}
	p := &registryGuardProvider{fakeProvider: fakeProvider{name: "google"}, guard: g}
	r := NewRegistry([]Provider{p}, RegistryOptions{MutationGuard: g})
	if _, err := r.CreateEvent(t.Context(), "google:account:primary", EventCreate{}); err != nil {
		t.Fatalf("confirmed success was altered: %v", err)
	}
	if !strings.Contains(output.String(), "local reconciliation remains pending") {
		t.Fatal("missing fallback signal")
	}
	if strings.Contains(output.String(), "private-provider-identifier") || strings.Contains(output.String(), "primary") {
		t.Fatal("fallback disclosed private data")
	}
}

func TestGuardedLegacyTimeoutIsUnknownWithoutSecondDispatch(t *testing.T) {
	for _, op := range []string{"create", "update", "delete"} {
		t.Run(op, func(t *testing.T) {
			g := &registryMutationGuard{}
			p := &registryGuardProvider{fakeProvider: fakeProvider{name: "google"}, guard: g, providerErr: context.DeadlineExceeded}
			r := NewRegistry([]Provider{p}, RegistryOptions{MutationGuard: g})
			var err error
			switch op {
			case "create":
				_, err = r.CreateEvent(t.Context(), "google:account:primary", EventCreate{})
			case "update":
				_, err = r.UpdateEvent(t.Context(), "google:account:primary", "google:event", EventUpdate{})
			default:
				err = r.DeleteEvent(t.Context(), "google:account:primary", "google:event")
			}
			if !errors.Is(err, ErrProviderMutationOutcomeUnknown) || p.calls != 1 || !p.acceptedBeforeFailure || !reflect.DeepEqual(g.calls, []string{"begin", "provider"}) {
				t.Fatalf("err=%v calls=%d fence=%v", err, p.calls, g.calls)
			}
		})
	}
}

func TestNilGuardLegacyTimeoutRetainsProviderError(t *testing.T) {
	p := &registryGuardProvider{fakeProvider: fakeProvider{name: "google"}, providerErr: context.DeadlineExceeded}
	r := NewRegistry([]Provider{p})
	_, err := r.CreateEvent(t.Context(), "google:account:primary", EventCreate{})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrProviderMutationOutcomeUnknown) || p.calls != 1 {
		t.Fatalf("err=%v dispatches=%d", err, p.calls)
	}
}
