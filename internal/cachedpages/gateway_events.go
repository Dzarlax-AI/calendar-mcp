package cachedpages

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
	"github.com/mark3labs/mcp-go/mcp"
)

var (
	ErrGatewayCachePending   = errors.New("event publication is pending after a write")
	ErrGatewayCacheMiss      = errors.New("event has no confirmed cached result")
	ErrGatewayNotFound       = errors.New("event deletion is confirmed")
	ErrGatewayResultTooLarge = errors.New("cached result exceeds the size limit")
)

type GatewayDetailsRequest struct {
	CalendarID, EventID, Query string
	Start, End                 time.Time
	EventTypes                 []string
	ShowDeleted                bool
	MaxResults                 int64
	PageToken                  string
}

type GatewayDetailSource struct {
	Source
	Complete bool `json:"complete"`
}

type GatewayDetailsPage struct {
	Items         []calendar.EventV2    `json:"items"`
	Sources       []GatewayDetailSource `json:"sources"`
	NextPageToken string                `json:"next_page_token,omitempty"`
	Complete      bool                  `json:"complete"`
	Partial       bool                  `json:"partial"`
}

type gatewayQuery struct {
	Tool    string
	Storage storage.GatewayEventQuery
	Text    string
	Types   []string
	Size    int
}

func (s *Service) GatewayCoverage(ctx context.Context, request Request, token string) (Page, PageStats, error) {
	var stats PageStats
	request.WithCoverage = true
	normalized, err := normalizeRequest(request)
	if err != nil {
		return Page{}, stats, ErrInvalidArgument
	}
	query := gatewayQuery{Tool: "get_events", Storage: storage.GatewayEventQuery{CalendarIDs: normalized.CalendarIDs,
		Window: storage.SyncWindow{Start: normalized.Start, End: normalized.End}, CivilStart: normalized.StartDate, CivilEnd: normalized.EndDate}, Size: normalized.PageSize}
	read, binding, at, err := s.gatewayRead(ctx, &query)
	if err != nil {
		return Page{}, stats, err
	}
	items, continuedPartial, err := s.gatewayContinuation(read.Items, binding, token)
	if err != nil {
		return Page{}, stats, err
	}
	rows := make([]Row, 0, min(len(items), query.Size+1))
	for _, item := range items[:min(len(items), query.Size+1)] {
		event := item.Event
		summary := Summary{Kind: "event", EventID: event.ID, CalendarID: event.CalendarID, Title: event.Title,
			Status: event.Status, Start: event.Start, End: event.End}
		if item.Oversized {
			summary = MarkerSummary(summary)
		}
		rows = append(rows, Row{Summary: summary, Position: gatewayPosition(event)})
	}
	sources := make([]Source, 0, len(read.Sources))
	for _, src := range read.Sources {
		sources = append(sources, src.Source)
	}
	page, err := Pack(ReadPage{Rows: rows, Sources: sources, ObservedAt: at, Partial: read.Incomplete || continuedPartial, WithCoverage: true}, query.Size, s.gatewayMint(binding, at))
	if err != nil {
		return Page{}, stats, err
	}
	if err := s.recheckGatewayRead(ctx, query, binding, token); err != nil {
		return Page{}, stats, err
	}
	return page, stats, nil
}

func (s *Service) GatewayEvent(ctx context.Context, ref calendar.EventRef) (calendar.EventV2, error) {
	if !validGatewayID(ref.CalendarID) || !validGatewayID(ref.EventID) {
		return calendar.EventV2{}, ErrInvalidArgument
	}
	query := gatewayQuery{Tool: "get_event", Storage: storage.GatewayEventQuery{CalendarRef: ref.CalendarID, EventID: ref.EventID}}
	read, binding, _, err := s.gatewayRead(ctx, &query)
	if err != nil {
		return calendar.EventV2{}, err
	}
	if err := s.recheckGatewayRead(ctx, query, binding, ""); err != nil {
		return calendar.EventV2{}, err
	}
	if len(read.Sources) == 0 {
		return calendar.EventV2{}, ErrUnavailable
	}
	if len(read.Items) == 0 {
		if read.Sources[0].Source.ErrorCode == "cache_pending" {
			return calendar.EventV2{}, ErrGatewayCachePending
		}
		if read.Sources[0].Version == "" {
			return calendar.EventV2{}, ErrUnavailable
		}
		return calendar.EventV2{}, ErrGatewayCacheMiss
	}
	item := read.Items[0]
	if item.Cancelled {
		return calendar.EventV2{}, ErrGatewayNotFound
	}
	if item.Oversized || !GatewayResultFits(item.Event) {
		return calendar.EventV2{}, ErrGatewayResultTooLarge
	}
	return item.Event, nil
}

func (s *Service) GatewayDetails(ctx context.Context, tool string, req GatewayDetailsRequest) (GatewayDetailsPage, error) {
	query, err := s.gatewayDetailsQuery(tool, req)
	if err != nil {
		return GatewayDetailsPage{}, err
	}
	read, binding, at, err := s.gatewayRead(ctx, &query)
	if err != nil {
		return GatewayDetailsPage{}, err
	}
	oversizedSources := map[string]bool{}
	if tool == "search_events" {
		read.Items = slices.DeleteFunc(read.Items, func(item storage.GatewayReadItem) bool {
			if item.Oversized {
				oversizedSources[item.Event.CalendarID] = true
			}
			return item.Oversized
		})
	}
	items, carried, err := s.gatewayContinuation(read.Items, binding, req.PageToken)
	if err != nil {
		return GatewayDetailsPage{}, err
	}
	if req.CalendarID != "" && len(read.Sources) == 0 {
		return GatewayDetailsPage{}, ErrUnavailable
	}
	page := GatewayDetailsPage{Items: []calendar.EventV2{}, Sources: []GatewayDetailSource{}, Partial: read.Incomplete || carried || len(oversizedSources) != 0}
	for _, src := range read.Sources {
		if oversizedSources[src.Source.CalendarID] {
			src.Source.Coverage = "partial"
			if src.Source.ErrorCode == "" {
				src.Source.ErrorCode = "result_too_large"
			}
		}
		page.Sources = append(page.Sources, GatewayDetailSource{Source: src.Source, Complete: src.Source.Coverage == "covered"})
	}
	mint := s.gatewayMint(binding, at)
	for i := 0; i < min(query.Size, len(items)); i++ {
		item := items[i]
		if item.Oversized {
			return GatewayDetailsPage{}, ErrGatewayResultTooLarge
		}
		candidate := page
		candidate.Items = append(page.Items, item.Event)
		candidate.Complete = i+1 == len(items) && !candidate.Partial
		if i+1 < len(items) {
			pos := gatewayPosition(item.Event)
			pos.Partial = candidate.Partial
			candidate.NextPageToken, err = mint(pos)
			if err != nil {
				return GatewayDetailsPage{}, err
			}
		} else {
			candidate.NextPageToken = ""
		}
		if !GatewayResultFits(candidate) {
			if i == 0 {
				return GatewayDetailsPage{}, ErrGatewayResultTooLarge
			}
			break
		}
		page = candidate
	}
	if len(items) == 0 {
		page.Complete = !page.Partial
	}
	if !GatewayResultFits(page) {
		return GatewayDetailsPage{}, ErrGatewayResultTooLarge
	}
	if err := s.recheckGatewayRead(ctx, query, binding, req.PageToken); err != nil {
		return GatewayDetailsPage{}, err
	}
	return page, nil
}

func (s *Service) gatewayDetailsQuery(tool string, req GatewayDetailsRequest) (gatewayQuery, error) {
	isSearch := tool == "search_events"
	if !isSearch && (!validGatewayID(req.CalendarID) || !validGatewayID(req.EventID) || req.Start.IsZero() || req.End.IsZero()) {
		return gatewayQuery{}, ErrInvalidArgument
	}
	if (req.CalendarID != "" && !validGatewayID(req.CalendarID)) || req.MaxResults < 0 || len(req.EventTypes) > 32 || len(req.Query) > 1024 {
		return gatewayQuery{}, ErrInvalidArgument
	}
	text := strings.ToLower(strings.TrimSpace(req.Query))
	if isSearch && text == "" {
		return gatewayQuery{}, ErrInvalidArgument
	}
	target := s.gatewayWindow(s.now())
	start, end := req.Start, req.End
	if start.IsZero() {
		start = target.Start
	}
	if end.IsZero() {
		end = target.End
	}
	norm, err := normalizeRequest(Request{Start: start, End: end, PageSize: defaultPageSize, WithCoverage: true})
	if err != nil {
		return gatewayQuery{}, ErrInvalidArgument
	}
	size := defaultPageSize
	if req.MaxResults > 0 {
		size = int(min(req.MaxResults, maxPageSize))
	}
	types := slices.Clone(req.EventTypes)
	slices.Sort(types)
	return gatewayQuery{Tool: tool, Storage: storage.GatewayEventQuery{CalendarRef: req.CalendarID, SeriesID: req.EventID,
		Window: storage.SyncWindow{Start: norm.Start, End: norm.End}, CivilStart: norm.StartDate, CivilEnd: norm.EndDate, ShowDeleted: req.ShowDeleted},
		Text: text, Types: slices.Compact(types), Size: size}, nil
}

func (s *Service) gatewayRead(ctx context.Context, query *gatewayQuery) (storage.GatewayEventRead, [32]byte, time.Time, error) {
	var empty storage.GatewayEventRead
	if !s.gatewayCaller(ctx) {
		return empty, [32]byte{}, time.Time{}, ErrUnavailable
	}
	query.Storage.ExcludeIDs = s.excludeCalendarIDs
	query.Storage.IncludeImported = s.includeImportedCalendars
	now := s.now().UTC()
	read, err := s.store.ReadGatewayEvents(ctx, query.Storage, now, false)
	if err != nil {
		return empty, [32]byte{}, now, err
	}
	if query.Tool == "search_events" {
		read.Items = slices.DeleteFunc(read.Items, func(item storage.GatewayReadItem) bool {
			if item.Oversized {
				return false
			}
			e := item.Event
			eventType := "default"
			if e.Google != nil && e.Google.EventType != "" {
				eventType = e.Google.EventType
			}
			if len(query.Types) > 0 && !slices.Contains(query.Types, eventType) {
				return true
			}
			return !strings.Contains(strings.ToLower(e.Title), query.Text) && !strings.Contains(strings.ToLower(e.Description), query.Text) &&
				!strings.Contains(strings.ToLower(e.Location), query.Text)
		})
	}
	slices.SortFunc(read.Items, func(a, b storage.GatewayReadItem) int {
		ap, bp := gatewayPosition(a.Event), gatewayPosition(b.Event)
		if ap.AllDay != bp.AllDay {
			if ap.AllDay {
				return 1
			}
			return -1
		}
		for _, order := range []int{strings.Compare(ap.StartAt, bp.StartAt), strings.Compare(ap.StartDate, bp.StartDate), strings.Compare(ap.CalendarID, bp.CalendarID), strings.Compare(ap.EventID, bp.EventID)} {
			if order != 0 {
				return order
			}
		}
		return 0
	})
	return read, s.gatewayBinding(ctx, *query, read, now), now, nil
}

func (s *Service) gatewayWindow(now time.Time) storage.SyncWindow {
	if s.windowAt != nil {
		return utcWindow(s.windowAt(now))
	}
	return s.window
}

func (s *Service) gatewayBinding(ctx context.Context, query gatewayQuery, read storage.GatewayEventRead, now time.Time) [32]byte {
	caller, _ := auth.VerifiedCallerFromContext(ctx)
	type version struct{ ID, Grant, Connection, Receipt, Content, Coverage, Status, ErrorCode string }
	versions := make([]version, 0, len(read.Sources))
	for _, v := range read.Sources {
		versions = append(versions, version{v.Grant.Metadata.ID, v.Grant.Revision, v.Grant.ConnectionRevision, v.Grant.DetailReceipt, v.Version, v.Source.Coverage, v.Source.Status, v.Source.ErrorCode})
	}
	raw, _ := json.Marshal([]any{"gateway-cache-only-v1", caller.KeyFingerprint(), caller.ConfigurationFingerprint(), processBootNonce,
		s.gatewayWindow(now), query, versions, read.Incomplete})
	return sha256.Sum256(raw)
}

func (s *Service) recheckGatewayRead(ctx context.Context, query gatewayQuery, prior [32]byte, token string) error {
	now := s.now().UTC()
	current, err := s.store.ReadGatewayEvents(ctx, query.Storage, now, true)
	if err != nil || s.gatewayBinding(ctx, query, current, now) != prior {
		if token != "" {
			return ErrCursorUnavailable
		}
		return ErrUnavailable
	}
	return nil
}

func gatewayPosition(event calendar.EventV2) Position {
	pos := Position{CalendarID: event.CalendarID, EventID: event.ID}
	if event.Start.IsAllDay() {
		pos.AllDay, pos.StartDate = true, event.Start.Date
	} else if event.OriginalStart != nil && event.Start.DateTime == "" {
		return gatewayPosition(calendar.EventV2{ID: event.ID, CalendarID: event.CalendarID, Start: *event.OriginalStart})
	} else if start, err := event.Start.Instant(); err == nil {
		pos.StartAt = start.UTC().Format("2006-01-02T15:04:05.000000000Z")
	} else {
		pos.StartAt = "unknown"
	}
	return pos
}

func (s *Service) gatewayContinuation(items []storage.GatewayReadItem, binding [32]byte, token string) ([]storage.GatewayReadItem, bool, error) {
	if token == "" {
		return items, false, nil
	}
	position, _, err := s.codec.Decode(token, binding)
	if err != nil {
		return nil, false, ErrCursorUnavailable
	}
	partial := position.Partial
	position.Partial = false
	for i, item := range items {
		if gatewayPosition(item.Event) == position {
			return items[i+1:], partial, nil
		}
	}
	return nil, false, ErrCursorUnavailable
}

func (s *Service) gatewayMint(binding [32]byte, now time.Time) func(Position) (string, error) {
	return func(p Position) (string, error) { return s.codec.Encode(p, binding, now.Add(cursorLifetime)) }
}

func validGatewayID(id string) bool { return id != "" && len(id) <= maxCursorIDBytes }

func GatewayResultFits(value any) bool {
	raw, err := json.Marshal(value)
	if err != nil {
		return false
	}
	wire, err := json.Marshal(mcp.NewToolResultStructured(value, string(raw)))
	return err == nil && len(wire) <= MaxResultBytes
}
