package apple

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"calendar-mcp/internal/calendar"
)

// go-webdav FindCalendars does not expose calendar-timezone. Request it in
// the same enumeration to avoid extra round trips per collection.
const calendarDiscoveryRequest = `<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop>
<D:resourcetype/><D:displayname/><C:calendar-timezone/>
</D:prop></D:propfind>`

type calendarDiscoveryProps struct {
	ResourceType *struct {
		Calendar *struct{} `xml:"urn:ietf:params:xml:ns:caldav calendar"`
	} `xml:"DAV: resourcetype"`
	DisplayName *string    `xml:"DAV: displayname"`
	Timezone    []string   `xml:"urn:ietf:params:xml:ns:caldav calendar-timezone"`
	Other       []struct{} `xml:",any"`
}

func (p *Provider) findCalendars(ctx context.Context, homeSet string) ([]calendar.Calendar, bool, error) {
	base, err := url.Parse(p.caldavURL)
	if err != nil {
		return nil, false, err
	}
	if !strings.HasPrefix(homeSet, "/") {
		homeSet = path.Join(base.Path, homeSet)
	}
	endpoint := url.URL{Scheme: base.Scheme, User: base.User, Host: base.Host, Path: homeSet}
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", endpoint.String(), strings.NewReader(calendarDiscoveryRequest))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "1")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, false, fmt.Errorf("calendar PROPFIND returned HTTP %d", resp.StatusCode)
	}

	var result struct {
		XMLName   xml.Name `xml:"DAV: multistatus"`
		Responses []struct {
			Hrefs     []string `xml:"DAV: href"`
			Status    string   `xml:"DAV: status"`
			PropStats []struct {
				Status string                 `xml:"DAV: status"`
				Props  calendarDiscoveryProps `xml:"DAV: prop"`
			} `xml:"DAV: propstat"`
		} `xml:"DAV: response"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, false, fmt.Errorf("calendar PROPFIND parse: %w", err)
	}

	var calendars []calendar.Calendar
	sawTimezoneDenial, sawOtherFailure := false, false
	for _, entry := range result.Responses {
		if entry.Status != "" && webDAVStatus(entry.Status)/100 != 2 {
			return nil, false, fmt.Errorf("calendar PROPFIND entry returned HTTP %d", webDAVStatus(entry.Status))
		}
		if len(entry.Hrefs) != 1 {
			return nil, false, fmt.Errorf("calendar PROPFIND entry must have one href")
		}
		href, err := url.Parse(strings.TrimSpace(entry.Hrefs[0]))
		if err != nil || href.Path == "" {
			return nil, false, fmt.Errorf("calendar PROPFIND entry has an invalid href")
		}
		var props calendarDiscoveryProps
		var displayNameStatus int
		for _, stat := range entry.PropStats {
			code := webDAVStatus(stat.Status)
			if stat.Props.ResourceType != nil && code/100 != 2 {
				return nil, false, fmt.Errorf("calendar PROPFIND resourcetype returned HTTP %d", code)
			}
			if stat.Props.DisplayName != nil && code/100 != 2 && code != http.StatusNotFound {
				displayNameStatus = code
			}
			if code/100 != 2 {
				if code == http.StatusForbidden && stat.Props.Timezone != nil &&
					stat.Props.ResourceType == nil && stat.Props.DisplayName == nil && len(stat.Props.Other) == 0 {
					sawTimezoneDenial = true
				} else if code != http.StatusNotFound {
					sawOtherFailure = true
				}
				continue
			}
			if stat.Props.ResourceType != nil {
				props.ResourceType = stat.Props.ResourceType
			}
			if stat.Props.DisplayName != nil {
				props.DisplayName = stat.Props.DisplayName
			}
			props.Timezone = append(props.Timezone, stat.Props.Timezone...)
		}
		if props.ResourceType == nil {
			return nil, false, fmt.Errorf("calendar PROPFIND entry is missing resourcetype")
		}
		if props.ResourceType.Calendar == nil {
			continue
		}
		if displayNameStatus != 0 {
			return nil, false, fmt.Errorf("calendar PROPFIND displayname returned HTTP %d", displayNameStatus)
		}
		item := calendar.Calendar{ID: href.Path}
		if props.DisplayName != nil {
			item.Name = *props.DisplayName
		}
		if len(props.Timezone) == 1 {
			item.Timezone = calendarTimezone(props.Timezone[0])
		}
		calendars = append(calendars, item)
	}
	return calendars, len(calendars) > 0 && sawTimezoneDenial && !sawOtherFailure, nil
}
