package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
)

// EventSyncPolicy supplies bounded defaults for Graph's calendarView delta
// feed. The coordinator remains responsible for applying these limits.
func (p *Provider) EventSyncPolicy() calendar.EventSyncPolicy {
	return calendar.EventSyncPolicy{
		PollInterval: time.Minute,
		RetryBase:    5 * time.Second,
		RetryMax:     5 * time.Minute,
		MaxPages:     250,
		MaxResets:    2,
	}
}

// SyncEvents reads one Microsoft calendarView delta page. Graph delta and
// continuation links are opaque values: this adapter only returns them to the
// coordinator after ensuring they remain on the configured Graph origin.
func (p *Provider) SyncEvents(ctx context.Context, request calendar.EventSyncRequest) (calendar.EventSyncPage, error) {
	if request.CalendarID == "" || request.Window.Start.IsZero() || request.Window.End.IsZero() || !request.Window.End.After(request.Window.Start) {
		return calendar.EventSyncPage{}, syncProtocolError()
	}

	next, err := p.syncURL(request)
	if err != nil {
		return calendar.EventSyncPage{}, syncProtocolError()
	}

	var response graphDeltaPage
	if err := p.getDeltaPage(ctx, next, &response); err != nil {
		if isInvalidDeltaState(err) {
			return calendar.EventSyncPage{ResetRequired: true}, nil
		}
		return calendar.EventSyncPage{}, syncErrorForDelta(err)
	}

	page := calendar.EventSyncPage{}
	for i := range response.Value {
		event := response.Value[i]
		if event.Removed != nil {
			if event.ID == "" {
				return calendar.EventSyncPage{}, syncProtocolError()
			}
			page.DeletedEventIDs = append(page.DeletedEventIDs, event.ID)
			continue
		}
		if err := validateGraphDeltaEventRange(event.graphEvent); err != nil {
			return calendar.EventSyncPage{}, syncProtocolError()
		}
		converted, err := event.graphEvent.toEventV2(request.CalendarID)
		if err != nil || converted.ID == "" {
			return calendar.EventSyncPage{}, syncProtocolError()
		}
		inWindow, err := eventV2InSyncWindow(converted, request.Window)
		if err != nil {
			return calendar.EventSyncPage{}, syncProtocolError()
		}
		if !inWindow {
			// A delta can report an event whose update moved it outside the
			// calendarView interval. Its formerly cached projection must go away.
			page.DeletedEventIDs = append(page.DeletedEventIDs, converted.ID)
			continue
		}
		page.Upserts = append(page.Upserts, calendar.EventSyncUpsert{Event: converted})
	}

	if response.NextLink != "" {
		if err := p.validateGraphPageURL(response.NextLink); err != nil {
			return calendar.EventSyncPage{}, syncProtocolError()
		}
		page.NextPageToken = calendar.EventSyncPageToken(response.NextLink)
		return page, nil
	}
	if response.DeltaLink == "" {
		return calendar.EventSyncPage{}, syncProtocolError()
	}
	if err := p.validateGraphPageURL(response.DeltaLink); err != nil {
		return calendar.EventSyncPage{}, syncProtocolError()
	}
	page.Complete = true
	page.NextCursor = calendar.EventSyncCursor(response.DeltaLink)
	return page, nil
}

func (p *Provider) syncURL(request calendar.EventSyncRequest) (string, error) {
	if request.PageToken != "" {
		if err := p.validateGraphPageURL(string(request.PageToken)); err != nil {
			return "", err
		}
		return string(request.PageToken), nil
	}
	if request.Mode == calendar.EventSyncIncremental {
		if request.Cursor == "" {
			return "", graphDeltaProtocolError{}
		}
		if err := p.validateGraphPageURL(string(request.Cursor)); err != nil {
			return "", err
		}
		return string(request.Cursor), nil
	}
	if request.Mode != calendar.EventSyncReplacement {
		return "", graphDeltaProtocolError{}
	}
	path := "/me/calendars/" + url.PathEscape(request.CalendarID) + "/calendarView/delta"
	params := url.Values{
		"startDateTime": {request.Window.Start.UTC().Format(time.RFC3339)},
		"endDateTime":   {request.Window.End.UTC().Format(time.RFC3339)},
	}
	return p.baseURL + path + "?" + params.Encode(), nil
}

type graphDeltaPage struct {
	Value     []graphDeltaEvent `json:"value"`
	NextLink  string            `json:"@odata.nextLink"`
	DeltaLink string            `json:"@odata.deltaLink"`
}

// Removed is deliberately modeled separately from cancellation: Graph's
// @removed marker means the resource must be deleted from the local view.
//
// It is embedded here, rather than in generic event mapping, because it is
// meaningful only for the delta protocol.
type graphDeltaEvent struct {
	graphEvent
	Removed *struct {
		Reason string `json:"reason"`
	} `json:"@removed"`
}

func (p *Provider) getDeltaPage(ctx context.Context, rawURL string, out *graphDeltaPage) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Prefer", `outlook.timezone="UTC", outlook.body-content-type="html"`)
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= http.StatusBadRequest {
		var graphErr struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.NewDecoder(response.Body).Decode(&graphErr)
		return graphDeltaHTTPError{status: response.StatusCode, code: graphErr.Error.Code, retryAfter: retryAfter(response.Header.Get("Retry-After"), time.Now())}
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		return graphDeltaProtocolError{}
	}
	return nil
}

type graphDeltaHTTPError struct {
	status     int
	code       string
	retryAfter time.Duration
}

func (e graphDeltaHTTPError) Error() string { return "Graph delta request failed" }

type graphDeltaProtocolError struct{}

func (graphDeltaProtocolError) Error() string { return "invalid Graph delta response" }

func isInvalidDeltaState(err error) bool {
	value, ok := err.(graphDeltaHTTPError)
	if !ok {
		return false
	}
	if value.status == http.StatusGone {
		return true
	}
	switch strings.ToLower(value.code) {
	case "syncstatenotfound", "invaliddeltatoken", "invalidsyncstate", "resyncrequired":
		return true
	default:
		return false
	}
}

func syncErrorForDelta(err error) error {
	if _, ok := err.(graphDeltaProtocolError); ok {
		return syncProtocolError()
	}
	value, ok := err.(graphDeltaHTTPError)
	if !ok {
		return &calendar.EventSyncError{Class: calendar.EventSyncTransient}
	}
	switch value.status {
	case http.StatusUnauthorized:
		return &calendar.EventSyncError{Class: calendar.EventSyncAuth}
	case http.StatusForbidden:
		return &calendar.EventSyncError{Class: calendar.EventSyncPermission}
	case http.StatusTooManyRequests:
		return &calendar.EventSyncError{Class: calendar.EventSyncRateLimited, RetryAfter: value.retryAfter}
	case http.StatusRequestTimeout:
		return &calendar.EventSyncError{Class: calendar.EventSyncTransient}
	default:
		if value.status >= 500 {
			return &calendar.EventSyncError{Class: calendar.EventSyncTransient}
		}
		return syncProtocolError()
	}
}

func syncProtocolError() error { return &calendar.EventSyncError{Class: calendar.EventSyncProtocol} }

func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}

func eventV2InSyncWindow(event calendar.EventV2, window calendar.EventSyncWindow) (bool, error) {
	if window.Start.IsZero() || window.End.IsZero() || !window.End.After(window.Start) {
		return false, errors.New("invalid sync window")
	}
	start, err := event.Start.Instant()
	if err != nil {
		return false, err
	}
	end, err := event.End.Instant()
	if err != nil {
		return false, err
	}
	if !end.After(start) {
		return false, errors.New("invalid event time range")
	}
	return start.Before(window.End) && end.After(window.Start), nil
}

func validateGraphDeltaEventRange(event graphEvent) error {
	if !event.IsAllDay && (parseGraphTime(event.Start).IsZero() || parseGraphTime(event.End).IsZero()) {
		return errors.New("invalid Graph event time")
	}
	var start, end calendar.EventTime
	if event.IsAllDay {
		start = calendar.EventTime{Date: graphDate(event.Start)}
		end = calendar.EventTime{Date: graphDate(event.End)}
	} else {
		start = graphEventTime(event.Start)
		end = graphEventTime(event.End)
	}
	return calendar.ValidateEventTimeRangeV2(start, end)
}

var _ calendar.EventSyncProvider = (*Provider)(nil)
