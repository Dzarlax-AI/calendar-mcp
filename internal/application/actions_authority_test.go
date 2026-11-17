package application

import (
	"calendar-mcp/internal/calendar"
	"context"
	"testing"
)

type actionAuthorityProvider struct{ stubV2Provider }

func (*actionAuthorityProvider) RespondToEventV2(_ context.Context, r calendar.RespondToEventRequestV2) (*calendar.OperationResult, error) {
	if r.Notifications != calendar.NotificationsNone {
		panic("notifications enabled")
	}
	return &calendar.OperationResult{Status: "completed", Event: &calendar.EventV2{ID: "e"}}, nil
}
func (*actionAuthorityProvider) MoveEventV2(_ context.Context, r calendar.MoveEventRequestV2) (*calendar.OperationResult, error) {
	if r.Notifications != calendar.NotificationsNone {
		panic("notifications enabled")
	}
	return &calendar.OperationResult{Status: "completed", Event: &calendar.EventV2{ID: "e"}}, nil
}
func (*actionAuthorityProvider) ImportEventV2(context.Context, calendar.ImportEventRequestV2) (*calendar.EventV2, error) {
	return &calendar.EventV2{ID: "e"}, nil
}
func TestRespondMoveImportDoNotWriteThroughProjection(t *testing.T) {
	p := &actionAuthorityProvider{stubV2Provider: stubV2Provider{name: "google", capabilities: calendar.CalendarCapabilities{NotificationPolicies: []calendar.NotificationPolicy{calendar.NotificationsNone}}}}
	model := &stubEventReadModel{}
	svc := New(calendar.NewRegistry([]calendar.Provider{p}), WithEventReadModel(model))
	ctx := context.Background()
	if _, err := svc.RespondToEvent(ctx, calendar.RespondToEventRequestV2{Ref: calendar.EventRef{CalendarID: "google:cal", EventID: "e"}, Response: "accepted", Notifications: calendar.NotificationsNone}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MoveEvent(ctx, calendar.MoveEventRequestV2{Ref: calendar.EventRef{CalendarID: "google:cal", EventID: "e"}, DestinationCalendarID: "google:other", Notifications: calendar.NotificationsNone}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ImportEvent(ctx, calendar.ImportEventRequestV2{CalendarID: "google:cal", Event: calendar.EventCreateV2{Start: calendar.EventTime{Date: "2026-01-01"}, End: calendar.EventTime{Date: "2026-01-02"}}}); err != nil {
		t.Fatal(err)
	}
	if len(model.upserts) != 0 || len(model.deleted) != 0 || len(model.scheduled) != 0 {
		t.Fatal("action wrote projection")
	}
}
