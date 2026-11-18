package google

import (
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/token"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/oauth2"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	gcal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

type discoveryRoundTrip func(*http.Request) (*http.Response, error)

func (f discoveryRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDetailDiscoveryStagesPagesAndRejectsDuplicates(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprint(duplicate), func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: discoveryRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				body := `{"items":[{"id":"a","accessRole":"owner","primary":true}],"nextPageToken":"next"}`
				if r.URL.Query().Get("pageToken") == "next" {
					id := "b"
					if duplicate {
						id = "a"
					}
					body = fmt.Sprintf(`{"items":[{"id":%q,"accessRole":"reader"}]}`, id)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			svc, err := gcal.NewService(context.Background(), option.WithHTTPClient(client))
			if err != nil {
				t.Fatal(err)
			}
			found, err := (&Provider{svc: svc}).DiscoverDetailCalendars(context.Background())
			if duplicate {
				if err == nil || found != nil {
					t.Fatalf("duplicate accepted: %v %v", found, err)
				}
				return
			}
			if err != nil || calls != 2 || len(found) != 2 || found[0].AccessRole != "owner" || found[1].AccessRole != "reader" {
				t.Fatalf("discovery: %v %v calls=%d", found, err, calls)
			}
		})
	}
}

func TestListEventsObservesRoleBeforeReturningItems(t *testing.T) {
	client := &http.Client{Transport: discoveryRoundTrip(func(r *http.Request) (*http.Response, error) {
		if binding := token.RequestBindingFromContext(r.Context()); binding != nil {
			binding.Set = true
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"accessRole":"reader","items":[]}`)), Request: r}, nil
	})}
	svc, err := gcal.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{svc: svc}
	observed := false
	p.SetRoleObserver(func(_ context.Context, id string) (func(context.Context, string, string, [32]byte) error, error) {
		if id != "c" {
			t.Fatalf("id=%q", id)
		}
		return func(_ context.Context, role, _ string, _ [32]byte) error {
			observed = true
			if role != "reader" {
				t.Fatalf("role=%q", role)
			}
			return errors.New("authority closed")
		}, nil
	})
	_, err = p.ListEventsV2(context.Background(), calendar.ListEventsRequestV2{CalendarID: "c", Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)})
	if err == nil || !observed {
		t.Fatalf("role not enforced: %v", err)
	}
}

type failingSnapshotStore struct{}

func (failingSnapshotStore) Load() (*oauth2.Token, error) { return nil, errors.New("unavailable") }
func (failingSnapshotStore) Save(*oauth2.Token) error     { return nil }
func TestSnapshotConstructorFailsOnLoadError(t *testing.T) {
	if p, err := NewWithSnapshotTokenStore("client", "secret", failingSnapshotStore{}); err == nil || p != nil {
		t.Fatal("failed snapshot load accepted")
	}
}

func TestDetailDiscoveryBoundsAndEmptyResults(t *testing.T) {
	cases := []struct {
		name     string
		makeBody func(int) string
		want     int
		fail     bool
	}{
		{"empty", func(int) string { return `{"items":[]}` }, 0, false},
		{"all-denied", func(int) string {
			return `{"items":[{"id":"a","accessRole":"freeBusyReader"},{"id":"b","accessRole":"none"}]}`
		}, 2, false},
		{"seventeen", func(int) string {
			items := make([]string, 17)
			for i := range items {
				items[i] = fmt.Sprintf(`{"id":"c%d","accessRole":"reader"}`, i)
			}
			return `{"items":[` + strings.Join(items, ",") + `]}`
		}, 17, false},
		{"overflow", func(int) string {
			items := make([]string, 257)
			for i := range items {
				items[i] = fmt.Sprintf(`{"id":"c%d","accessRole":"reader"}`, i)
			}
			return `{"items":[` + strings.Join(items, ",") + `]}`
		}, 0, true},
		{"repeated-token", func(int) string { return `{"items":[],"nextPageToken":"same"}` }, 0, true},
		{"page-overflow", func(i int) string { return fmt.Sprintf(`{"items":[],"nextPageToken":"token-%d"}`, i) }, 0, true},
		{"null-item", func(int) string { return `{"items":[null]}` }, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: discoveryRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.makeBody(calls))), Request: r}, nil
			})}
			svc, err := gcal.NewService(context.Background(), option.WithHTTPClient(client))
			if err != nil {
				t.Fatal(err)
			}
			entries, err := (&Provider{svc: svc}).DiscoverDetailCalendars(context.Background())
			if tc.fail {
				if err == nil || entries != nil {
					t.Fatalf("accepted invalid listing: %v", err)
				}
				return
			}
			if err != nil || len(entries) != tc.want {
				t.Fatalf("entries=%d err=%v", len(entries), err)
			}
		})
	}
}

func TestCalendarListObservesEveryPageWithRequestBinding(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: discoveryRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		b := token.RequestBindingFromContext(r.Context())
		b.Set = true
		b.Revision = fmt.Sprintf("revision-%d", calls)
		body := `{"items":[{"id":"a","accessRole":"owner"}],"nextPageToken":"next"}`
		if calls == 2 {
			body = `{"items":[{"id":"b","accessRole":"freeBusyReader"}]}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	svc, err := gcal.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{svc: svc}
	captures := 0
	var observations []string
	p.SetAccountRoleObserver(func(context.Context) (func(context.Context, string, string, string, [32]byte) error, error) {
		captures++
		if calls != 0 {
			t.Fatal("capture after I/O")
		}
		return func(_ context.Context, id, role, revision string, _ [32]byte) error {
			observations = append(observations, id+":"+role+":"+revision)
			if id == "b" {
				return errors.New("closed")
			}
			return nil
		}, nil
	})
	_, err = p.ListCalendars(context.Background())
	if err == nil || calls != 2 || captures != 1 || strings.Join(observations, ",") != "a:owner:revision-1,b:freeBusyReader:revision-2" {
		t.Fatalf("calls=%d captures=%d observations=%v err=%v", calls, captures, observations, err)
	}
}

func TestCalendarGetObservesRoleBeforeReturn(t *testing.T) {
	client := &http.Client{Transport: discoveryRoundTrip(func(r *http.Request) (*http.Response, error) {
		b := token.RequestBindingFromContext(r.Context())
		b.Set = true
		b.Revision = "used"
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"a","accessRole":"freeBusyReader"}`)), Request: r}, nil
	})}
	svc, err := gcal.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{svc: svc}
	p.SetRoleObserver(func(_ context.Context, id string) (func(context.Context, string, string, [32]byte) error, error) {
		if id != "a" {
			t.Fatal(id)
		}
		return func(_ context.Context, role, rev string, _ [32]byte) error {
			if role != "freeBusyReader" || rev != "used" {
				t.Fatalf("%s %s", role, rev)
			}
			return errors.New("closed")
		}, nil
	})
	if _, err := p.Capabilities(context.Background(), "a"); err == nil {
		t.Fatal("downgrade ignored")
	}
}

func TestListCalendarsAllowsMoreThanDiscoveryPageLimit(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: discoveryRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		body := fmt.Sprintf(`{"items":[],"nextPageToken":"%d"}`, calls)
		if calls == 65 {
			body = `{"items":[]}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	svc, err := gcal.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Provider{svc: svc}).ListCalendars(context.Background())
	if err != nil || calls != 65 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestSyncEventsPreservesRoleOnMalformedItem(t *testing.T) {
	client := &http.Client{Transport: discoveryRoundTrip(func(r *http.Request) (*http.Response, error) {
		b := token.RequestBindingFromContext(r.Context())
		b.Set = true
		b.Revision = "used"
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"accessRole":"freeBusyReader","items":[{}]}`)), Request: r}, nil
	})}
	svc, err := gcal.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	page, err := (&Provider{svc: svc}).SyncEvents(context.Background(), calendar.EventSyncRequest{CalendarID: "a", Window: calendar.EventSyncWindow{Start: now, End: now.Add(time.Hour)}, Mode: calendar.EventSyncReplacement})
	if err == nil || !page.RoleConfirmed || page.AccessRole != "freeBusyReader" || page.CredentialRevision != "used" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}

func TestGatewayDiscoveryRestrictsObservedRoleAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &http.Client{Transport: discoveryRoundTrip(func(r *http.Request) (*http.Response, error) {
		cancel()
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"items":[{"id":"cal","accessRole":"reader"}]}`)), Request: r}, nil
	})}
	svc, err := gcal.NewService(t.Context(), option.WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	var restricted bool
	ctx = calendar.WithGatewayReadRestriction(ctx, func(call context.Context, id string, denied bool) error {
		if call.Err() != nil || id != "cal" || denied {
			t.Fatalf("bad restriction context: %v, %s, %t", call.Err(), id, denied)
		}
		restricted = true
		return nil
	})
	p := &Provider{svc: svc}
	if _, err := p.CheckCalendarReadEvidence(ctx); err == nil || !restricted {
		t.Fatalf("cancelled check must restrict observed role without publishing full evidence, restricted=%t err=%v", restricted, err)
	}
}

func TestGatewayWriteMarkerSurvivesClearedProperties(t *testing.T) {
	event := &gcal.Event{NullFields: []string{"ExtendedProperties", "Description"}}
	setGatewayWriteMarker(event, "operation-id")
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ExtendedProperties gcal.EventExtendedProperties
		Description        json.RawMessage
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.ExtendedProperties.Private[googleGatewayWriteProperty] != "operation-id" || string(result.Description) != "null" {
		t.Fatalf("encoded patch: %s", raw)
	}
}
