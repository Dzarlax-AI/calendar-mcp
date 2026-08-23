package apple

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"

	"calendar-mcp/internal/calendar"
)

const maxDAVErrorBody = 32 << 10

// EventSyncPolicy supplies conservative defaults for Apple CalDAV. Sync
// collection availability varies by calendar, so polling remains slower than
// the API-native Google and Microsoft delta feeds.
func (p *Provider) EventSyncPolicy() calendar.EventSyncPolicy {
	return calendar.EventSyncPolicy{
		PollInterval: 5 * time.Minute,
		RetryBase:    time.Minute,
		RetryMax:     15 * time.Minute,
		MaxPages:     250,
		MaxResets:    2,
	}
}

// SyncEvents implements the optional read-model sync capability. CalDAV
// sync-tokens are deliberately confined to the returned cursor/page token and
// never included in errors or logs.
func (p *Provider) SyncEvents(ctx context.Context, request calendar.EventSyncRequest) (calendar.EventSyncPage, error) {
	if request.CalendarID == "" || !request.Window.End.After(request.Window.Start) {
		return calendar.EventSyncPage{}, appleEventSyncError(calendar.EventSyncProtocol)
	}

	// A page token is the server-issued continuation sync-token. It is used only
	// for a single coordinator run and is never made durable by the adapter.
	token := request.Cursor
	if request.PageToken != "" {
		token = calendar.EventSyncCursor(request.PageToken)
	}

	changes, fallback, reset, err := p.syncCollection(ctx, request.CalendarID, token)
	if err != nil {
		return calendar.EventSyncPage{}, err
	}
	if reset {
		return calendar.EventSyncPage{ResetRequired: true}, nil
	}
	if fallback {
		// A fallback scan is authoritative only as a fresh generation. The
		// coordinator resets an existing incremental cursor before it asks us for
		// the replacement page, preventing an unsafe incremental sweep.
		if request.Mode == calendar.EventSyncIncremental {
			return calendar.EventSyncPage{ResetRequired: true}, nil
		}
		return p.replacementFromInventory(ctx, request)
	}
	if request.Mode == calendar.EventSyncReplacement {
		// A successful sync-collection with an empty token is allowed to return
		// only the current token, not a complete collection snapshot. Seed the
		// projection from an authoritative inventory and retain the token only
		// for subsequent incremental runs.
		page, err := p.replacementFromCalendarQuery(ctx, request)
		if err != nil {
			return calendar.EventSyncPage{}, err
		}
		if !changes.limited {
			page.NextCursor = calendar.EventSyncCursor(changes.syncToken)
		}
		return page, nil
	}

	page, err := p.pageFromChanges(ctx, request, changes)
	if err != nil {
		return calendar.EventSyncPage{}, err
	}
	if changes.limited {
		page.NextPageToken = calendar.EventSyncPageToken(changes.syncToken)
		return page, nil
	}
	page.Complete = true
	page.NextCursor = calendar.EventSyncCursor(changes.syncToken)
	return page, nil
}

func (p *Provider) replacementFromCalendarQuery(ctx context.Context, request calendar.EventSyncRequest) (calendar.EventSyncPage, error) {
	query := &caldav.CalendarQuery{CompFilter: caldav.CompFilter{Name: "VCALENDAR", Comps: []caldav.CompFilter{{Name: "VEVENT", Start: request.Window.Start, End: request.Window.End}}}}
	objects, malformed, err := p.queryCalendarForEventSync(ctx, request.CalendarID, query)
	if err != nil {
		if class, ok := appleCalendarQueryErrorClass(err); ok {
			return calendar.EventSyncPage{}, appleEventSyncError(class)
		}
		return calendar.EventSyncPage{}, appleEventSyncError(calendar.EventSyncTransient)
	}
	if malformed {
		return p.replacementFromInventory(ctx, request)
	}
	return appleReplacementFromObjects(request, objects)
}

// QueryCalendar decodes every calendar-data property before returning its
// object list. If one resource is malformed, retry through the inventory GET
// lane, which can isolate that resource while retaining valid siblings.
func (p *Provider) queryCalendarForEventSync(ctx context.Context, calendarID string, query *caldav.CalendarQuery) (objects []caldav.CalendarObject, malformed bool, err error) {
	defer func() {
		if recover() != nil {
			objects = nil
			malformed = true
			err = nil
		}
	}()
	objects, err = p.client.QueryCalendar(ctx, calendarID, query)
	if err != nil && isAppleCalendarPayloadError(err) {
		return nil, true, nil
	}
	return objects, false, err
}

func isAppleCalendarPayloadError(err error) bool {
	var syntaxErr *xml.SyntaxError
	return errors.As(err, &syntaxErr) || errors.Is(err, io.ErrUnexpectedEOF) || strings.HasPrefix(err.Error(), "ical:")
}

func appleCalendarQueryErrorClass(err error) (calendar.EventSyncErrorClass, bool) {
	message := err.Error()
	switch {
	case strings.Contains(message, "HTTP multi-status request failed: 401 "):
		return calendar.EventSyncAuth, true
	case strings.Contains(message, "HTTP multi-status request failed: 403 "):
		return calendar.EventSyncPermission, true
	case strings.Contains(message, "HTTP multi-status request failed: 429 "):
		return calendar.EventSyncRateLimited, true
	default:
		return "", false
	}
}

func appleReplacementFromObjects(request calendar.EventSyncRequest, objects []caldav.CalendarObject) (calendar.EventSyncPage, error) {
	fetched := make([]appleFetchedObject, 0, len(objects))
	for i := range objects {
		fetched = append(fetched, appleFetchedObject{
			source: appleSyncObject{path: objects[i].Path, etag: objects[i].ETag},
			object: objects[i],
		})
	}
	return appleReplacementPage(request, fetched)
}

func appleEventSyncError(class calendar.EventSyncErrorClass) error {
	return &calendar.EventSyncError{Class: class}
}

type appleSyncChanges struct {
	syncToken string
	limited   bool
	objects   []appleSyncObject
}

type appleSyncObject struct {
	path    string
	etag    string
	deleted bool
}

// syncCollection asks for only stable object metadata. Event bodies are then
// fetched through the normal CalDAV client so recurring objects use the same
// parser as the rest of the Apple adapter.
func (p *Provider) syncCollection(ctx context.Context, calendarID string, token calendar.EventSyncCursor) (appleSyncChanges, bool, bool, error) {
	body := []byte(`<?xml version="1.0" encoding="utf-8"?><D:sync-collection xmlns:D="DAV:"><D:sync-token>` + xmlEscape(string(token)) + `</D:sync-token><D:sync-level>1</D:sync-level><D:prop><D:getetag/></D:prop></D:sync-collection>`)
	req, err := http.NewRequestWithContext(ctx, "REPORT", p.caldavURL+calendarID, bytes.NewReader(body))
	if err != nil {
		return appleSyncChanges{}, false, false, appleEventSyncError(calendar.EventSyncProtocol)
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "1")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return appleSyncChanges{}, false, false, appleEventSyncError(calendar.EventSyncTransient)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMultiStatus {
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return appleSyncChanges{}, false, false, appleEventSyncError(calendar.EventSyncAuth)
		case http.StatusForbidden:
			if hasValidSyncTokenPrecondition(resp.Body) {
				return appleSyncChanges{}, false, true, nil
			}
			return appleSyncChanges{}, false, false, appleEventSyncError(calendar.EventSyncPermission)
		case http.StatusTooManyRequests:
			return appleSyncChanges{}, false, false, &calendar.EventSyncError{Class: calendar.EventSyncRateLimited, RetryAfter: retryAfter(resp.Header)}
		case http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusNotImplemented, http.StatusConflict, http.StatusInternalServerError:
			// iCloud Family Sharing calendars are known to return a broken REPORT
			// response (often HTTP 500). A full PROPFIND replacement is safer than
			// treating that report as a provider-wide outage.
			return appleSyncChanges{}, true, false, nil
		default:
			return appleSyncChanges{}, false, false, appleEventSyncError(calendar.EventSyncTransient)
		}
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return appleSyncChanges{}, false, false, appleEventSyncError(calendar.EventSyncTransient)
	}
	var parsed appleSyncMultiStatus
	if err := xml.Unmarshal(data, &parsed); err != nil || strings.TrimSpace(parsed.SyncToken) == "" {
		return appleSyncChanges{}, true, false, nil
	}

	changes := appleSyncChanges{syncToken: strings.TrimSpace(parsed.SyncToken), limited: parsed.Limited != nil}
	for _, response := range parsed.Responses {
		object, ok := response.syncObject()
		if !ok {
			return appleSyncChanges{}, true, false, nil
		}
		if !strings.HasSuffix(strings.ToLower(object.path), ".ics") {
			continue
		}
		if _, err := appleObjectPath(calendarID, object.path); err != nil {
			return appleSyncChanges{}, false, false, appleEventSyncError(calendar.EventSyncProtocol)
		}
		changes.objects = append(changes.objects, object)
	}
	return changes, false, false, nil
}

// hasValidSyncTokenPrecondition consumes at most maxDAVErrorBody bytes. It
// recognizes DAV namespace bindings, not literal prefixes, and never returns
// the error body to callers or logs.
func hasValidSyncTokenPrecondition(body io.Reader) bool {
	limited := io.LimitReader(body, maxDAVErrorBody)
	decoder := xml.NewDecoder(limited)
	for {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		start, ok := token.(xml.StartElement)
		if ok && start.Name.Space == "DAV:" && strings.EqualFold(start.Name.Local, "valid-sync-token") {
			return true
		}
	}
}

type appleSyncMultiStatus struct {
	SyncToken string              `xml:"sync-token"`
	Responses []appleSyncResponse `xml:"response"`
	Limited   *struct{}           `xml:"number-of-matches-within-limits"`
}

type appleSyncResponse struct {
	Href      string `xml:"href"`
	Status    string `xml:"status"`
	PropStats []struct {
		Status string `xml:"status"`
		Prop   struct {
			ETag string `xml:"getetag"`
		} `xml:"prop"`
	} `xml:"propstat"`
}

func (r appleSyncResponse) syncObject() (appleSyncObject, bool) {
	path := strings.TrimSpace(r.Href)
	if path == "" {
		return appleSyncObject{}, false
	}
	if webDAVStatus(r.Status) == http.StatusNotFound || webDAVStatus(r.Status) == http.StatusGone {
		return appleSyncObject{path: path, deleted: true}, true
	}
	for _, propstat := range r.PropStats {
		if webDAVStatus(propstat.Status) == http.StatusNotFound || webDAVStatus(propstat.Status) == http.StatusGone {
			return appleSyncObject{path: path, deleted: true}, true
		}
		if webDAVStatus(propstat.Status) == http.StatusOK && strings.TrimSpace(propstat.Prop.ETag) != "" {
			return appleSyncObject{path: path, etag: strings.TrimSpace(propstat.Prop.ETag)}, true
		}
	}
	return appleSyncObject{}, false
}

func webDAVStatus(value string) int {
	fields := strings.Fields(value)
	for _, field := range fields {
		if code, err := strconv.Atoi(field); err == nil && code >= 100 && code <= 599 {
			return code
		}
	}
	return 0
}

func xmlEscape(value string) string {
	var escaped bytes.Buffer
	_ = xml.EscapeText(&escaped, []byte(value))
	return escaped.String()
}

func retryAfter(header http.Header) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(0, time.Until(when))
	}
	return 0
}

func (p *Provider) pageFromChanges(ctx context.Context, request calendar.EventSyncRequest, changes appleSyncChanges) (calendar.EventSyncPage, error) {
	objects, err := p.fetchSyncObjects(ctx, request.CalendarID, changes.objects)
	if err != nil {
		return calendar.EventSyncPage{}, err
	}
	return appleSyncPage(request, objects)
}

type appleFetchedObject struct {
	source    appleSyncObject
	object    caldav.CalendarObject
	deleted   bool
	malformed bool
}

func (p *Provider) fetchSyncObjects(ctx context.Context, calendarID string, inventory []appleSyncObject) ([]appleFetchedObject, error) {
	results := make([]appleFetchedObject, len(inventory))
	if len(inventory) == 0 {
		return results, nil
	}

	sem := make(chan struct{}, fallbackConcurrency)
	var wg sync.WaitGroup
	var failure error
	var failures sync.Mutex
	for i, item := range inventory {
		path, err := appleObjectPath(calendarID, item.path)
		if err != nil {
			return nil, appleEventSyncError(calendar.EventSyncProtocol)
		}
		// CalDAV hrefs are allowed to use non-canonical path spelling. Persist
		// and delete by the same canonical identity so a 404/410 change cannot
		// leave members that were previously stored after a successful GET.
		item.path = path
		results[i].source = item
		if item.deleted {
			results[i].deleted = true
			continue
		}
		wg.Add(1)
		go func(i int, path, etag string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			object, missing, malformed, err := p.fetchEventSyncObject(ctx, path, etag)
			if err != nil {
				failures.Lock()
				if failure == nil {
					failure = err
				}
				failures.Unlock()
				return
			}
			if missing {
				results[i].deleted = true
				return
			}
			if malformed {
				results[i].malformed = true
				return
			}
			results[i].object = *object
		}(i, path, item.etag)
	}
	wg.Wait()
	if failure != nil {
		return nil, failure
	}
	return results, nil
}

// fetchEventSyncObject uses the provider's authenticated HTTP client rather
// than caldav.Client.GetCalendarObject so sync can distinguish an HTTP or body
// transport failure from malformed iCalendar in one individual resource.
// Normal Apple reads keep using the CalDAV client unchanged.
func (p *Provider) fetchEventSyncObject(ctx context.Context, path, etag string) (*caldav.CalendarObject, bool, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.caldavURL+path, nil)
	if err != nil {
		return nil, false, false, appleEventSyncError(calendar.EventSyncProtocol)
	}
	req.Header.Set("Accept", ical.MIMEType)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, false, false, appleEventSyncError(calendar.EventSyncTransient)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return nil, true, false, nil
	case http.StatusUnauthorized:
		return nil, false, false, appleEventSyncError(calendar.EventSyncAuth)
	case http.StatusForbidden:
		return nil, false, false, appleEventSyncError(calendar.EventSyncPermission)
	case http.StatusTooManyRequests:
		return nil, false, false, &calendar.EventSyncError{Class: calendar.EventSyncRateLimited, RetryAfter: retryAfter(resp.Header)}
	}
	if resp.StatusCode/100 != 2 {
		return nil, false, false, appleEventSyncError(calendar.EventSyncTransient)
	}
	responseETag := strings.TrimSpace(resp.Header.Get("ETag"))
	if responseETag == "" {
		// A missing response ETag is conservatively treated as unchanged.
		responseETag = etag
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, false, appleEventSyncError(calendar.EventSyncTransient)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, ical.MIMEType) {
		// A successful HTTP response with an absent or unrelated media type is
		// a protocol failure, not malformed iCalendar. Treating it as a
		// recoverable object would hide HTML/login/proxy responses and could
		// silently discard a valid resource whose representation was wrong.
		return nil, false, false, appleEventSyncError(calendar.EventSyncProtocol)
	}
	container, valid := decodeAppleEventSyncCalendar(data)
	if !valid {
		return &caldav.CalendarObject{Path: path, ETag: responseETag}, false, true, nil
	}
	return &caldav.CalendarObject{Path: path, ETag: responseETag, Data: container}, false, false, nil
}

// RepairEventSyncObject isolates one CalDAV resource. The ETag belongs to the
// repair response; the caller ETag is only an input for identity/backoff and is
// never copied into a successful repaired object.
func (p *Provider) RepairEventSyncObject(ctx context.Context, request calendar.EventSyncObjectRepairRequest) (calendar.EventSyncObjectRepairResult, error) {
	if p == nil || p.httpClient == nil || request.CalendarID == "" || request.Object.ObjectID == "" {
		return calendar.EventSyncObjectRepairResult{}, appleEventSyncError(calendar.EventSyncProtocol)
	}
	path, err := appleObjectPath(request.CalendarID, request.Object.ObjectID)
	if err != nil {
		return calendar.EventSyncObjectRepairResult{}, appleEventSyncError(calendar.EventSyncProtocol)
	}
	object, missing, malformed, err := p.fetchEventSyncObject(ctx, path, request.Object.ETag)
	if err != nil {
		return calendar.EventSyncObjectRepairResult{}, err
	}
	if missing {
		return calendar.EventSyncObjectRepairResult{Object: calendar.SyncObject{ObjectID: path, ETag: request.Object.ETag}, Outcome: calendar.EventSyncObjectProviderDeleted}, nil
	}
	if malformed {
		return calendar.EventSyncObjectRepairResult{Object: calendar.SyncObject{ObjectID: path, ETag: object.ETag}, Outcome: calendar.EventSyncObjectStillQuarantined, Warning: &calendar.EventSyncWarning{Code: calendar.EventSyncProtocol, ObjectID: path, ETag: object.ETag}}, nil
	}
	members, err := appleSyncMembers(*object, calendar.EventSyncRequest{CalendarID: request.CalendarID, Window: request.Window})
	if err != nil {
		return calendar.EventSyncObjectRepairResult{Object: calendar.SyncObject{ObjectID: path, ETag: object.ETag}, Outcome: calendar.EventSyncObjectStillQuarantined, Warning: &calendar.EventSyncWarning{Code: calendar.EventSyncProtocol, ObjectID: path, ETag: object.ETag}}, nil
	}
	identity := calendar.SyncObject{ObjectID: path, ETag: object.ETag}
	if len(members) == 0 {
		return calendar.EventSyncObjectRepairResult{Object: identity, Outcome: calendar.EventSyncObjectAbsentFromProjection}, nil
	}
	upserts := make([]calendar.EventSyncUpsert, 0, len(members))
	for _, event := range members {
		upserts = append(upserts, calendar.EventSyncUpsert{Object: identity, Event: event})
	}
	return calendar.EventSyncObjectRepairResult{Object: identity, Outcome: calendar.EventSyncObjectReplaceMembership, Upserts: upserts}, nil
}

// go-ical reports malformed content as an error, but a malformed parameter can
// also reach one of its parser panics. An individual remote resource must not
// take down an otherwise valid sync page.
func decodeAppleEventSyncCalendar(data []byte) (container *ical.Calendar, valid bool) {
	defer func() {
		if recover() != nil {
			container = nil
			valid = false
		}
	}()
	container, err := ical.NewDecoder(bytes.NewReader(data)).Decode()
	return container, err == nil
}

func appleSyncPage(request calendar.EventSyncRequest, objects []appleFetchedObject) (calendar.EventSyncPage, error) {
	page := calendar.EventSyncPage{}
	for _, fetched := range objects {
		if fetched.deleted {
			page.DeletedObjectIDs = append(page.DeletedObjectIDs, fetched.source.path)
			continue
		}
		if fetched.malformed {
			page.Warnings = append(page.Warnings, calendar.EventSyncWarning{Code: calendar.EventSyncProtocol, ObjectID: fetched.source.path, ETag: fetched.source.etag})
			continue
		}
		members, err := appleSyncMembers(fetched.object, request)
		if err != nil {
			page.Warnings = append(page.Warnings, calendar.EventSyncWarning{Code: calendar.EventSyncProtocol, ObjectID: fetched.object.Path, ETag: fetched.object.ETag})
			continue
		}
		if len(members) == 0 {
			page.DeletedObjectIDs = append(page.DeletedObjectIDs, fetched.object.Path)
			continue
		}
		object := calendar.SyncObject{ObjectID: fetched.object.Path, ETag: fetched.object.ETag}
		page.Inventory = append(page.Inventory, object)
		page.ReplacedObjectIDs = append(page.ReplacedObjectIDs, object.ObjectID)
		for _, event := range members {
			page.Upserts = append(page.Upserts, calendar.EventSyncUpsert{Object: object, Event: event})
		}
	}
	return page, nil
}

func appleSyncMembers(object caldav.CalendarObject, request calendar.EventSyncRequest) ([]calendar.EventV2, error) {
	events := object.Data.Events()
	items := make([]calendar.EventV2, 0, len(events))
	masters := 0
	masterUIDs := make(map[string]struct{}, len(events))
	for i := range events {
		if events[i].Props.Get("RECURRENCE-ID") == nil {
			masters++
			uid, _ := events[i].Props.Text("UID")
			if uid == "" {
				continue
			}
			masterUIDs[uid] = struct{}{}
		}
	}
	for i := range events {
		recurrenceID := events[i].Props.Get("RECURRENCE-ID")
		if recurrenceID == nil {
			continue
		}
		uid, _ := events[i].Props.Text("UID")
		if uid == "" {
			return nil, appleEventSyncError(calendar.EventSyncProtocol)
		}
		if _, ok := masterUIDs[uid]; !ok {
			return nil, appleEventSyncError(calendar.EventSyncProtocol)
		}
		if _, err := recurrenceID.DateTime(nil); err != nil {
			return nil, err
		}
		status, _ := events[i].Props.Text("STATUS")
		if !strings.EqualFold(status, "CANCELLED") {
			for _, name := range []string{"DTSTART", "DTEND"} {
				prop := events[i].Props.Get(name)
				if prop == nil {
					return nil, appleEventSyncError(calendar.EventSyncProtocol)
				}
				if _, err := prop.DateTime(nil); err != nil {
					return nil, err
				}
			}
		}
	}
	for i := range events {
		if events[i].Props.Get("RECURRENCE-ID") != nil {
			continue
		}
		uid, _ := events[i].Props.Text("UID")
		data := *object.Data
		data.Children = nil
		group := caldav.CalendarObject{Path: object.Path, ETag: object.ETag, Data: &data}
		group.Data.Children = append(group.Data.Children, events[i].Component)
		for j := range events {
			if events[j].Props.Get("RECURRENCE-ID") == nil {
				continue
			}
			otherUID, _ := events[j].Props.Text("UID")
			if otherUID == uid {
				group.Data.Children = append(group.Data.Children, events[j].Component)
			}
		}
		expanded, err := appleEventsFromObject(group, calendar.ListEventsRequestV2{CalendarID: request.CalendarID, Start: request.Window.Start, End: request.Window.End, View: calendar.RecurrenceBoth})
		if err != nil {
			return nil, err
		}
		if masters > 1 {
			memberID := appleSyncMemberID(object.Path, uid, i)
			for k := range expanded {
				// Existing Apple mutation paths address one calendar resource as
				// one series. A multi-master resource needs a member selector that
				// those paths do not yet support, so expose the extra materialized
				// members honestly as read-only instead of offering broken writes.
				expanded[k].ReadOnly = true
				if expanded[k].OriginalStart != nil {
					expanded[k].ID = appleInstanceID(memberID, *expanded[k].OriginalStart)
				} else {
					expanded[k].ID = memberID
				}
				if expanded[k].RecurringEventID == object.Path {
					expanded[k].RecurringEventID = memberID
				}
			}
		}
		items = append(items, expanded...)
	}
	return items, nil
}

func appleSyncMemberID(path, uid string, index int) string {
	identity := "uid:" + uid
	if uid == "" {
		identity = "index:" + strconv.Itoa(index)
	}
	return path + "#calendar-mcp-member=" + base64.RawURLEncoding.EncodeToString([]byte(identity))
}

// replacementFromInventory intentionally fetches every object. The frozen
// EventSyncRequest has no previous object inventory, so ETag comparison cannot
// be made safely here. The completed cursorless replacement lets storage sweep
// stale membership atomically without claiming unsupported incrementality.
func (p *Provider) replacementFromInventory(ctx context.Context, request calendar.EventSyncRequest) (calendar.EventSyncPage, error) {
	inventory, err := p.propfindEventSyncInventory(ctx, request.CalendarID)
	if err != nil {
		return calendar.EventSyncPage{}, err
	}
	fetched, err := p.fetchSyncObjects(ctx, request.CalendarID, inventory)
	if err != nil {
		return calendar.EventSyncPage{}, err
	}
	return appleReplacementPage(request, fetched)
}

func appleReplacementPage(request calendar.EventSyncRequest, fetched []appleFetchedObject) (calendar.EventSyncPage, error) {
	page := calendar.EventSyncPage{Complete: true}
	for _, item := range fetched {
		if item.deleted {
			page.DeletedObjectIDs = append(page.DeletedObjectIDs, item.source.path)
			continue
		}
		if item.malformed {
			page.Warnings = append(page.Warnings, calendar.EventSyncWarning{Code: calendar.EventSyncProtocol, ObjectID: item.source.path})
			continue
		}
		members, err := appleSyncMembers(item.object, request)
		if err != nil {
			page.Warnings = append(page.Warnings, calendar.EventSyncWarning{Code: calendar.EventSyncProtocol, ObjectID: item.object.Path})
			continue
		}
		object := calendar.SyncObject{ObjectID: item.object.Path, ETag: item.object.ETag}
		if len(members) == 0 {
			// An object that materializes no events in this frozen window must
			// remove prior membership, if any. Its inventory is intentionally not
			// retained because the shared contract represents empty membership as a
			// deleted object.
			page.DeletedObjectIDs = append(page.DeletedObjectIDs, object.ObjectID)
			continue
		}
		page.Inventory = append(page.Inventory, object)
		page.ReplacedObjectIDs = append(page.ReplacedObjectIDs, object.ObjectID)
		for _, event := range members {
			page.Upserts = append(page.Upserts, calendar.EventSyncUpsert{Object: object, Event: event})
		}
	}
	return page, nil
}

func (p *Provider) propfindEventSyncInventory(ctx context.Context, calendarID string) ([]appleSyncObject, error) {
	body := []byte(`<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:prop><D:getetag/></D:prop></D:propfind>`)
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", p.caldavURL+calendarID, bytes.NewReader(body))
	if err != nil {
		return nil, appleEventSyncError(calendar.EventSyncProtocol)
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "1")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, appleEventSyncError(calendar.EventSyncTransient)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return nil, appleEventSyncError(calendar.EventSyncAuth)
	case http.StatusForbidden:
		return nil, appleEventSyncError(calendar.EventSyncPermission)
	case http.StatusTooManyRequests:
		return nil, &calendar.EventSyncError{Class: calendar.EventSyncRateLimited, RetryAfter: retryAfter(resp.Header)}
	}
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, appleEventSyncError(calendar.EventSyncTransient)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, appleEventSyncError(calendar.EventSyncTransient)
	}
	var parsed appleSyncMultiStatus
	if err := xml.Unmarshal(data, &parsed); err != nil {
		return nil, appleEventSyncError(calendar.EventSyncProtocol)
	}
	inventory := make([]appleSyncObject, 0, len(parsed.Responses))
	for _, response := range parsed.Responses {
		item, ok := response.syncObject()
		if !ok {
			href := strings.TrimSpace(response.Href)
			if strings.HasSuffix(strings.ToLower(href), ".ics") {
				item = appleSyncObject{path: href}
				ok = true
			}
		}
		if !ok {
			continue
		}
		if item.deleted || !strings.HasSuffix(strings.ToLower(item.path), ".ics") {
			continue
		}
		if _, err := appleObjectPath(calendarID, item.path); err != nil {
			return nil, appleEventSyncError(calendar.EventSyncProtocol)
		}
		inventory = append(inventory, item)
	}
	if len(inventory) == 0 {
		paths, pathErr := p.propfindCalendarObjects(ctx, calendarID)
		if pathErr != nil {
			return nil, appleEventSyncError(calendar.EventSyncTransient)
		}
		for _, path := range paths {
			if _, err := appleObjectPath(calendarID, path); err != nil {
				return nil, appleEventSyncError(calendar.EventSyncProtocol)
			}
			inventory = append(inventory, appleSyncObject{path: path})
		}
	}
	return inventory, nil
}
