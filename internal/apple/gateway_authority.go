package apple

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/emersion/go-webdav/caldav"

	"calendar-mcp/internal/calendar"
)

const appleAuthorityProperties = `<D:propfind xmlns:D="DAV:"><D:prop><D:owner/><D:current-user-privilege-set/></D:prop></D:propfind>`

type appleCollectionAccess struct {
	known, read, write bool
	owner              string
}

func (p *Provider) CheckCalendarReadEvidence(ctx context.Context) ([]calendar.CalendarReadEvidence, error) {
	status := &discoveryStatus{}
	ctx = context.WithValue(ctx, gatewayRawStatusKey{}, status)
	inventory, err := p.discoverCalendars(ctx, true)
	if status.code.Load() == http.StatusUnauthorized {
		return nil, calendar.NewAPIError(calendar.ErrorPermissionDenied, "Apple calendar authentication failed")
	}
	if err != nil {
		return nil, err
	}
	items := inventory.calendars
	if len(items)+len(inventory.unknown) > 256 {
		return nil, errors.New("Apple calendar inventory exceeds read bounds")
	}
	principal, err := p.client.FindCurrentUserPrincipal(ctx)
	if status.code.Load() == http.StatusUnauthorized {
		return nil, calendar.NewAPIError(calendar.ErrorPermissionDenied, "Apple calendar authentication failed")
	}
	if err != nil {
		return nil, err
	}
	if principal == "" {
		return nil, errors.New("Apple calendar principal unavailable")
	}
	now := time.Now().UTC().Truncate(time.Minute)
	proofs := make([]calendar.CalendarReadEvidence, 0, len(items)+len(inventory.unknown))
	for _, id := range inventory.unknown {
		proofs = append(proofs, calendar.CalendarReadEvidence{Calendar: calendar.Calendar{ID: id}, Unknown: true})
	}
	for i, item := range items {
		probeCtx, cancel := calendar.GatewayCalendarProbe(ctx, len(items)-i)
		proof, err := p.checkGatewayCollection(ctx, probeCtx, status, item, principal, now)
		cancel()
		if err != nil {
			return nil, err
		}
		proofs = append(proofs, proof)
	}
	return proofs, nil
}

func (p *Provider) checkGatewayCollection(ctx, probeCtx context.Context, status *discoveryStatus, item calendar.Calendar, principal string, now time.Time) (calendar.CalendarReadEvidence, error) {
	unknown := calendar.CalendarReadEvidence{Calendar: item, Unknown: true}
	access, err := p.gatewayCollectionAccess(probeCtx, item.ID)
	if status.code.Load() == http.StatusUnauthorized {
		return unknown, calendar.NewAPIError(calendar.ErrorPermissionDenied, "Apple calendar authentication failed")
	}
	if err != nil {
		var api *calendar.APIError
		if errors.As(err, &api) && api.Code == calendar.ErrorPermissionDenied {
			return unknown, err
		}
		return unknown, nil
	}
	owned := appleSameCollectionPrincipal(p.caldavURL, access.owner, principal)
	if access.owner != "" && !owned {
		if err := calendar.RestrictGatewayCalendarRead(ctx, item.ID, false); err != nil {
			return unknown, err
		}
	}
	caps, err := p.Capabilities(probeCtx, item.ID)
	if err != nil {
		return unknown, nil
	}
	proof := calendar.CalendarReadEvidence{Calendar: item, Capabilities: caps}
	if !access.known || access.read {
		events, err := p.probeGatewayEventRead(probeCtx, item.ID, now, owned && access.read)
		if status.code.Load() == http.StatusUnauthorized {
			return unknown, calendar.NewAPIError(calendar.ErrorPermissionDenied, "Apple calendar authentication failed")
		}
		var denied *calendar.EventSyncError
		if err != nil && (!errors.As(err, &denied) || denied.Class != calendar.EventSyncPermission) {
			var api *calendar.APIError
			if errors.As(err, &api) && api.Code == calendar.ErrorPermissionDenied {
				return unknown, err
			}
			return unknown, nil
		}
		proof.ReadEvents = err == nil && (access.read || events)
		proof.ReadPrivate = proof.ReadEvents && events && owned
	}
	if !proof.ReadEvents {
		if err := calendar.RestrictGatewayCalendarRead(ctx, item.ID, true); err != nil {
			return unknown, err
		}
	}
	canWrite := proof.ReadEvents && access.write
	proof.LimitOperations(canWrite)
	proof.Calendar.ReadOnly = !canWrite
	return proof, nil
}

func (p *Provider) gatewayCollectionAccess(ctx context.Context, id string) (appleCollectionAccess, error) {
	base, err := url.Parse(p.caldavURL)
	if err != nil || !strings.HasPrefix(id, "/") {
		return appleCollectionAccess{}, fmt.Errorf("invalid Apple calendar path")
	}
	endpoint := url.URL{Scheme: base.Scheme, Host: base.Host, Path: id}
	request, err := http.NewRequestWithContext(ctx, "PROPFIND", endpoint.String(), strings.NewReader(appleAuthorityProperties))
	if err != nil {
		return appleCollectionAccess{}, err
	}
	request.Header.Set("Depth", "0")
	request.Header.Set("Content-Type", "application/xml; charset=utf-8")
	response, err := p.httpClient.Do(request)
	if err != nil {
		return appleCollectionAccess{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return appleCollectionAccess{}, calendar.NewAPIError(calendar.ErrorPermissionDenied, "Apple calendar authentication failed")
	}
	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusNotFound {
		return appleCollectionAccess{known: true}, nil
	}
	if response.StatusCode != http.StatusMultiStatus {
		return appleCollectionAccess{}, fmt.Errorf("Apple privilege response unavailable")
	}
	const maxPrivilegesBytes = 64 << 10
	data, err := io.ReadAll(io.LimitReader(response.Body, maxPrivilegesBytes+1))
	if err != nil || len(data) > maxPrivilegesBytes {
		return appleCollectionAccess{}, fmt.Errorf("Apple privilege response exceeds read bounds")
	}
	var result struct {
		XMLName   xml.Name `xml:"DAV: multistatus"`
		Responses []struct {
			Href  string `xml:"DAV: href"`
			Props []struct {
				Status     string `xml:"DAV: status"`
				Privileges *struct {
					Values []struct {
						Read         *struct{} `xml:"DAV: read"`
						Write        *struct{} `xml:"DAV: write"`
						WriteContent *struct{} `xml:"DAV: write-content"`
						Bind         *struct{} `xml:"DAV: bind"`
						Unbind       *struct{} `xml:"DAV: unbind"`
					} `xml:"DAV: privilege"`
				} `xml:"DAV: prop>current-user-privilege-set"`
				Owner string `xml:"DAV: prop>owner>href"`
			} `xml:"DAV: propstat"`
		} `xml:"DAV: response"`
	}
	if err := xml.Unmarshal(data, &result); err != nil || len(result.Responses) != 1 || !appleSameCollectionPrincipal(p.caldavURL, endpoint.String(), result.Responses[0].Href) {
		return appleCollectionAccess{}, fmt.Errorf("invalid Apple privilege response")
	}
	var access appleCollectionAccess
	var content, bind, unbind bool
	for _, props := range result.Responses[0].Props {
		if webDAVStatus(props.Status)/100 != 2 {
			continue
		}
		if props.Owner != "" {
			access.owner = props.Owner
		}
		if props.Privileges == nil {
			continue
		}
		access.known = true
		for _, value := range props.Privileges.Values {
			access.read = access.read || value.Read != nil
			access.write = access.write || value.Write != nil
			content = content || value.WriteContent != nil
			bind = bind || value.Bind != nil
			unbind = unbind || value.Unbind != nil
		}
	}
	access.write = access.write || content && bind && unbind
	return access, nil
}

func appleSameCollectionPrincipal(server, first, second string) bool {
	base, err := url.Parse(server)
	if err != nil || first == "" || second == "" {
		return false
	}
	pathOnServer := func(value string) string {
		u, err := base.Parse(strings.TrimSpace(value))
		if err != nil || !strings.EqualFold(u.Host, base.Host) || u.Scheme != base.Scheme || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return ""
		}
		return path.Clean(u.Path)
	}
	a, b := pathOnServer(first), pathOnServer(second)
	return a != "" && a == b
}

func (p *Provider) probeGatewayEventRead(ctx context.Context, id string, start time.Time, proveOwnedAccess bool) (bool, error) {
	query := &caldav.CalendarQuery{CompFilter: caldav.CompFilter{Name: "VCALENDAR", Comps: []caldav.CompFilter{
		{Name: "VEVENT", Start: start, End: start.Add(time.Minute)},
	}}}
	objects, malformed, err := p.queryCalendarForEventSync(ctx, id, query)
	querySucceeded := err == nil && !malformed
	if querySucceeded {
		for i := range objects {
			if objects[i].Data != nil && normalizeAppleFixedOffsetTimezone(&objects[i]) == nil && len(objects[i].Data.Events()) > 0 {
				return true, nil
			}
		}
		if !proveOwnedAccess {
			return false, nil
		}
		// A narrow empty REPORT says nothing about access to private
		// content elsewhere in an owned collection.
	}
	if err != nil {
		if class, ok := appleCalendarQueryErrorClass(err); ok {
			if class == calendar.EventSyncAuth {
				return false, calendar.NewAPIError(calendar.ErrorPermissionDenied, "Apple calendar authentication failed")
			}
			return false, &calendar.EventSyncError{Class: class}
		}
		if !strings.Contains(err.Error(), "HTTP 500") {
			return false, errors.New("Apple event access check unavailable")
		}
	}
	// On shared collections with a broken calendar-query, only an actual
	// normalized object read validates the fallback. A PROPFIND alone proves
	// the listing, not the right to read an object's body.
	inventory, err := p.propfindEventSyncInventory(ctx, id)
	if err != nil {
		return false, err
	}
	if len(inventory) == 0 {
		if querySucceeded {
			return false, nil
		}
		return false, errors.New("Apple fallback event access unconfirmed")
	}
	item := inventory[0]
	object, missing, malformed, _, err := p.fetchEventSyncObject(ctx, item.path, item.etag)
	if err != nil || missing || malformed || object == nil {
		return false, errors.New("Apple fallback event access unconfirmed")
	}
	if err := normalizeAppleFixedOffsetTimezone(object); err != nil || len(object.Data.Events()) == 0 {
		return false, errors.New("Apple fallback event payload unconfirmed")
	}
	return true, nil
}
