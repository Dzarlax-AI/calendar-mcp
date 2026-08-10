package calendar

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUpdateEventRejectsMismatchedProviderPrefix(t *testing.T) {
	p := &fakeProvider{name: "google"}
	r := NewRegistry([]Provider{p})

	_, err := r.UpdateEvent(context.Background(), "google:primary", "microsoft:event", EventUpdate{})
	if err == nil {
		t.Fatal("UpdateEvent() returned nil error for mismatched provider prefix")
	}
	if p.updated {
		t.Fatal("provider UpdateEvent was called for mismatched prefix")
	}
}

func TestDeleteEventRejectsMismatchedProviderPrefix(t *testing.T) {
	p := &fakeProvider{name: "google"}
	r := NewRegistry([]Provider{p})

	err := r.DeleteEvent(context.Background(), "google:primary", "microsoft:event")
	if err == nil {
		t.Fatal("DeleteEvent() returned nil error for mismatched provider prefix")
	}
	if p.deleted {
		t.Fatal("provider DeleteEvent was called for mismatched prefix")
	}
}

func TestGetEventsFanOutReturnsErrorWhenProviderFails(t *testing.T) {
	r := NewRegistry([]Provider{
		&fakeProvider{name: "google", calendarsErr: errors.New("provider unavailable")},
		&fakeProvider{name: "apple", calendars: []Calendar{{ID: "cal"}}, events: []Event{{ID: "event", CalendarID: "cal"}}},
	})

	events, err := r.GetEvents(context.Background(), "", time.Now(), time.Now().Add(time.Hour))
	if err == nil {
		t.Fatal("GetEvents() returned nil error for incomplete fan-out")
	}
	if events != nil {
		t.Fatalf("events = %#v, want nil when completeness is unknown", events)
	}
}

type fakeProvider struct {
	name         string
	calendars    []Calendar
	calendarsErr error
	events       []Event
	eventsErr    error
	updated      bool
	deleted      bool
}

func (p *fakeProvider) Name() string { return p.name }
func (p *fakeProvider) ListCalendars(context.Context) ([]Calendar, error) {
	return p.calendars, p.calendarsErr
}
func (p *fakeProvider) GetEvents(context.Context, string, time.Time, time.Time) ([]Event, error) {
	return p.events, p.eventsErr
}
func (p *fakeProvider) CreateEvent(context.Context, string, EventCreate) (*Event, error) {
	return &Event{}, nil
}
func (p *fakeProvider) UpdateEvent(context.Context, string, string, EventUpdate) (*Event, error) {
	p.updated = true
	return &Event{}, nil
}
func (p *fakeProvider) DeleteEvent(context.Context, string, string) error {
	p.deleted = true
	return nil
}
