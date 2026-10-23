package apple

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
)

const defaultAppleSearchPageSize int64 = 250
const maxAppleSearchPageSize int64 = 2500

type appleSearchCursor struct {
	Version int    `json:"v"`
	Request string `json:"request"`
	Offset  int    `json:"offset"`
}

// SearchEventsV2 searches expanded instances locally; CalDAV text filters are
// not relied upon. Both bounds are mandatory to avoid an unbounded history scan.
func (p *Provider) SearchEventsV2(ctx context.Context, request calendar.SearchEventsRequestV2) (calendar.Page[calendar.EventV2], error) {
	empty := calendar.Page[calendar.EventV2]{}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if strings.TrimSpace(request.Query) == "" {
		return empty, invalidApple(fmt.Errorf("query is required"))
	}
	if request.Start.IsZero() || request.End.IsZero() {
		return empty, unsupportedApple("Apple event search requires both start and end")
	}
	if !request.End.After(request.Start) {
		return empty, invalidApple(fmt.Errorf("end must be after start"))
	}
	if request.MaxResults < 0 {
		return empty, invalidApple(fmt.Errorf("max_results must not be negative"))
	}
	limit := request.MaxResults
	if limit == 0 {
		limit = defaultAppleSearchPageSize
	}
	if limit > maxAppleSearchPageSize {
		limit = maxAppleSearchPageSize
	}
	fingerprint := appleSearchFingerprint(request)
	offset := 0
	if request.PageToken != "" {
		if len(request.PageToken) > 2048 {
			return empty, invalidApple(fmt.Errorf("invalid Apple search page token"))
		}
		raw, err := base64.RawURLEncoding.DecodeString(request.PageToken)
		var cursor appleSearchCursor
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Version != 1 || cursor.Request != fingerprint || cursor.Offset <= 0 {
			return empty, invalidApple(fmt.Errorf("invalid Apple search page token for this request"))
		}
		offset = cursor.Offset
	}
	if len(request.EventTypes) > 0 {
		allowed := false
		for _, kind := range request.EventTypes {
			if kind == "default" {
				allowed = true
			}
		}
		if !allowed {
			return calendar.Page[calendar.EventV2]{Items: []calendar.EventV2{}, Complete: true}, nil
		}
	}
	page, err := p.listEventsV2(ctx, calendar.ListEventsRequestV2{CalendarID: request.CalendarID, Start: request.Start, End: request.End, View: calendar.RecurrenceExpanded, ShowDeleted: request.ShowDeleted}, true)
	if err != nil {
		return empty, err
	}
	query := strings.ToLower(request.Query)
	matches := make([]calendar.EventV2, 0)
	for _, event := range page.Items {
		if err := ctx.Err(); err != nil {
			return empty, err
		}
		cancelled := strings.EqualFold(event.Status, "cancelled")
		if cancelled && !request.ShowDeleted {
			continue
		}
		if cancelled && event.OriginalStart != nil {
			original, err := event.OriginalStart.Instant()
			if err != nil || original.Before(request.Start) || !original.Before(request.End) {
				continue
			}
		} else if !appleEventOverlaps(event, request.Start, request.End) {
			continue
		}
		if strings.Contains(strings.ToLower(event.Title), query) || strings.Contains(strings.ToLower(event.Description), query) || strings.Contains(strings.ToLower(event.Location), query) {
			matches = append(matches, event)
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		left := appleSearchStart(matches[i])
		right := appleSearchStart(matches[j])
		if !left.Equal(right) {
			return left.Before(right)
		}
		return matches[i].ID < matches[j].ID
	})
	// External edits between pages may remove matches. Return the remaining empty
	// page rather than inventing a snapshot or restarting from the beginning.
	if offset > len(matches) {
		offset = len(matches)
	}
	end := offset + min(int(limit), len(matches)-offset)
	result := calendar.Page[calendar.EventV2]{Items: matches[offset:end], Complete: page.Complete}
	if end < len(matches) {
		raw, _ := json.Marshal(appleSearchCursor{Version: 1, Request: fingerprint, Offset: end})
		result.NextPageToken = base64.RawURLEncoding.EncodeToString(raw)
	}
	return result, nil
}

func appleSearchStart(event calendar.EventV2) time.Time {
	start, err := event.Start.Instant()
	if err != nil && event.OriginalStart != nil {
		start, _ = event.OriginalStart.Instant()
	}
	return start
}

func appleSearchFingerprint(request calendar.SearchEventsRequestV2) string {
	// Explicit strings keep time bounds in the fingerprint: request Start/End
	// deliberately have json:"-" tags in the public request structure.
	raw, _ := json.Marshal(struct {
		CalendarID string
		Query      string
		Start      string
		End        string
		Types      []string
		Deleted    bool
	}{request.CalendarID, request.Query, request.Start.UTC().Format(time.RFC3339Nano), request.End.UTC().Format(time.RFC3339Nano), request.EventTypes, request.ShowDeleted})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

var _ calendar.SearchProviderV2 = (*Provider)(nil)
