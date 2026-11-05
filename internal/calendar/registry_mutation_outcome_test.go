package calendar

import (
	"context"
	"errors"
	"testing"
)

type outcomeProvider struct {
	*fakeProvider
	err       error
	nilResult bool
	calls     int
}

func (p *outcomeProvider) CreateEvent(context.Context, string, EventCreate) (*Event, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	if p.nilResult {
		return nil, nil
	}
	return &Event{ID: "created"}, nil
}
func (p *outcomeProvider) UpdateEvent(context.Context, string, string, EventUpdate) (*Event, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	if p.nilResult {
		return nil, nil
	}
	return &Event{ID: "updated"}, nil
}
func (p *outcomeProvider) DeleteEvent(context.Context, string, string) error { p.calls++; return p.err }
func TestRegistryPreservesMutationTimeoutAndEmptyResponse(t *testing.T) {
	for _, op := range []string{"create", "update", "delete"} {
		t.Run(op+"-timeout", func(t *testing.T) {
			p := &outcomeProvider{fakeProvider: &fakeProvider{name: "google"}, err: context.DeadlineExceeded}
			r := NewRegistry([]Provider{p})
			var err error
			switch op {
			case "create":
				_, err = r.CreateEvent(t.Context(), "google:primary", EventCreate{})
			case "update":
				_, err = r.UpdateEvent(t.Context(), "google:primary", "event", EventUpdate{})
			default:
				err = r.DeleteEvent(t.Context(), "google:primary", "event")
			}
			if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrProviderMutationOutcomeUnknown) || p.calls != 1 {
				t.Fatalf("err=%v calls=%d", err, p.calls)
			}
		})
	}
	for _, op := range []string{"create", "update"} {
		t.Run(op+"-empty", func(t *testing.T) {
			p := &outcomeProvider{fakeProvider: &fakeProvider{name: "google"}, nilResult: true}
			r := NewRegistry([]Provider{p})
			var err error
			if op == "create" {
				_, err = r.CreateEvent(t.Context(), "google:primary", EventCreate{})
			} else {
				_, err = r.UpdateEvent(t.Context(), "google:primary", "event", EventUpdate{})
			}
			if !errors.Is(err, ErrProviderMutationOutcomeUnknown) || p.calls != 1 {
				t.Fatalf("err=%v calls=%d", err, p.calls)
			}
		})
	}
}
