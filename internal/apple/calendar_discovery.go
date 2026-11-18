package apple

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
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

const maxGatewayDiscoveryBytes = 4 << 20

type calendarInventory struct {
	calendars          []calendar.Calendar
	unknown            []string
	timezoneOnlyDenied bool
}

func (p *Provider) findCalendarInventory(ctx context.Context, homeSet string, allowPartial bool) (calendarInventory, error) {
	base, err := url.Parse(p.caldavURL)
	if err != nil {
		return calendarInventory{}, err
	}
	if !strings.HasPrefix(homeSet, "/") {
		homeSet = path.Join(base.Path, homeSet)
	}
	endpoint := url.URL{Scheme: base.Scheme, User: base.User, Host: base.Host, Path: homeSet}
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", endpoint.String(), strings.NewReader(calendarDiscoveryRequest))
	if err != nil {
		return calendarInventory{}, err
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "1")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return calendarInventory{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		return calendarInventory{}, fmt.Errorf("calendar PROPFIND returned HTTP %d", resp.StatusCode)
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
	var body io.Reader = resp.Body
	if allowPartial {
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxGatewayDiscoveryBytes+1))
		if err != nil {
			return calendarInventory{}, fmt.Errorf("calendar PROPFIND read: %w", err)
		}
		if len(data) > maxGatewayDiscoveryBytes {
			return calendarInventory{}, fmt.Errorf("calendar PROPFIND exceeds read bounds")
		}
		body = bytes.NewReader(data)
	}
	if err := xml.NewDecoder(body).Decode(&result); err != nil {
		return calendarInventory{}, fmt.Errorf("calendar PROPFIND parse: %w", err)
	}

	var inventory calendarInventory
	sawTimezoneDenial, sawOtherFailure := false, false
	seen := make(map[string]bool)
	if resp.Request != nil && resp.Request.URL != nil {
		endpoint = *resp.Request.URL
	}
	homePath := strings.TrimRight(endpoint.Path, "/")
entries:
	for _, entry := range result.Responses {
		status := webDAVStatus(entry.Status)
		if entry.Status != "" && status/100 != 2 && (!allowPartial || !temporaryCalendarDiscoveryStatus(status)) {
			return calendarInventory{}, fmt.Errorf("calendar PROPFIND entry returned HTTP %d", status)
		}
		if len(entry.Hrefs) != 1 {
			return calendarInventory{}, fmt.Errorf("calendar PROPFIND entry must have one href")
		}
		href, err := url.Parse(strings.TrimSpace(entry.Hrefs[0]))
		if err != nil || href.Path == "" {
			return calendarInventory{}, fmt.Errorf("calendar PROPFIND entry has an invalid href")
		}
		// Only a named collection under the enumerated home can be
		// inconclusive without invalidating the whole inventory.
		cleanPath := path.Clean(href.Path)
		canBeUnknown := allowPartial && strings.HasPrefix(cleanPath, homePath+"/") && cleanPath == strings.TrimRight(href.Path, "/") &&
			href.Opaque == "" && href.User == nil && href.RawQuery == "" && href.Fragment == "" &&
			(href.Host == "" || strings.EqualFold(href.Host, endpoint.Host)) &&
			(href.Scheme == "" || strings.EqualFold(href.Scheme, endpoint.Scheme))
		if allowPartial {
			id := strings.TrimRight(href.Path, "/")
			if seen[id] {
				return calendarInventory{}, fmt.Errorf("duplicate calendar PROPFIND href")
			}
			seen[id] = true
		}
		if entry.Status != "" && status/100 != 2 {
			if !canBeUnknown {
				return calendarInventory{}, fmt.Errorf("calendar PROPFIND entry returned HTTP %d", status)
			}
			inventory.unknown = append(inventory.unknown, href.Path)
			continue
		}
		var props calendarDiscoveryProps
		var displayNameStatus int
		var temporaryProperties bool
		for _, stat := range entry.PropStats {
			code := webDAVStatus(stat.Status)
			temporaryProperties = temporaryProperties || temporaryCalendarDiscoveryStatus(code)
			if stat.Props.ResourceType != nil && code/100 != 2 {
				if canBeUnknown && temporaryCalendarDiscoveryStatus(code) {
					inventory.unknown = append(inventory.unknown, href.Path)
					continue entries
				}
				return calendarInventory{}, fmt.Errorf("calendar PROPFIND resourcetype returned HTTP %d", code)
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
			if canBeUnknown && temporaryProperties {
				inventory.unknown = append(inventory.unknown, href.Path)
				continue
			}
			return calendarInventory{}, fmt.Errorf("calendar PROPFIND entry is missing resourcetype")
		}
		if props.ResourceType.Calendar == nil {
			continue
		}
		if displayNameStatus != 0 {
			if canBeUnknown && temporaryCalendarDiscoveryStatus(displayNameStatus) {
				inventory.unknown = append(inventory.unknown, href.Path)
				continue
			}
			return calendarInventory{}, fmt.Errorf("calendar PROPFIND displayname returned HTTP %d", displayNameStatus)
		}
		item := calendar.Calendar{ID: href.Path}
		if props.DisplayName != nil {
			item.Name = *props.DisplayName
		}
		if len(props.Timezone) == 1 {
			item.Timezone = calendarTimezone(props.Timezone[0])
		}
		inventory.calendars = append(inventory.calendars, item)
	}
	inventory.timezoneOnlyDenied = len(inventory.calendars) > 0 && len(inventory.unknown) == 0 && sawTimezoneDenial && !sawOtherFailure
	return inventory, nil
}

func temporaryCalendarDiscoveryStatus(status int) bool {
	return status/100 == 5 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests
}
