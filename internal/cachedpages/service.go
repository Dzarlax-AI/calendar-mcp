package cachedpages

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"calendar-mcp/internal/auth"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
)

var (
	ErrUnavailable       = errors.New("cached event scope unavailable")
	ErrInvalidArgument   = errors.New("invalid cached event page arguments")
	ErrCursorUnavailable = errors.New("page cursor unavailable")
)

const (
	defaultPageSize = 25
	maxPageSize     = 50
	maxScopeSize    = 16
	cursorLifetime  = 5 * time.Minute
)

var processBootNonce [32]byte
var processBootNonceErr error

func init() {
	_, processBootNonceErr = rand.Read(processBootNonce[:])
}

type Options struct {
	Window   storage.SyncWindow
	WindowAt func(time.Time) storage.SyncWindow
	Now      func() time.Time
}

type PageStats struct {
	Read              storage.CachedPageReadStats
	AuthorityNS       int64
	ReadNS            int64
	PackNS            int64
	FinalValidationNS int64
}

type Service struct {
	store    *storage.Store
	cipher   *credentials.Cipher
	codec    *Codec
	window   storage.SyncWindow
	windowAt func(time.Time) storage.SyncWindow
	now      func() time.Time
	ready    bool

	// testBeforeFinalValidation is available only to package tests.
	testBeforeFinalValidation func()
}

func NewService(store *storage.Store, cipher *credentials.Cipher, opts Options) (*Service, error) {
	if store == nil || cipher == nil {
		return nil, errors.New("cached page service requires store and cipher")
	}
	if processBootNonceErr != nil {
		return nil, fmt.Errorf("initialize cached page process nonce: %w", processBootNonceErr)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	window := opts.Window
	if opts.WindowAt != nil {
		window = opts.WindowAt(opts.Now().UTC())
	}
	window = utcWindow(window)
	if !validWindow(window) {
		return nil, errors.New("cached page service requires a valid sync window")
	}
	return &Service{store: store, cipher: cipher, codec: NewCodec(cipher, opts.Now), window: window, windowAt: opts.WindowAt, now: opts.Now, ready: true}, nil
}

func utcWindow(window storage.SyncWindow) storage.SyncWindow {
	return storage.SyncWindow{Start: window.Start.UTC(), End: window.End.UTC()}
}

func validWindow(window storage.SyncWindow) bool {
	return window.End.After(window.Start) && window.Start.Year() >= 1 && window.End.Year() <= 9999
}

// Ready reports whether the service has all constructor-owned prerequisites
// needed for MCP registration.
func (s *Service) Ready() bool {
	return s != nil && s.ready && s.store != nil && s.cipher != nil && s.codec != nil && processBootNonceErr == nil && s.window.End.After(s.window.Start)
}

func (s *Service) Read(ctx context.Context, request Request, pageToken string) (Page, PageStats, error) {
	var stats PageStats
	if !s.Ready() {
		return Page{}, stats, ErrUnavailable
	}
	caller, ok := auth.VerifiedCallerFromContext(ctx)
	if !ok {
		return Page{}, stats, ErrUnavailable
	}
	request, err := normalizeRequest(request)
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return Page{}, stats, ErrUnavailable
		}
		return Page{}, stats, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if pageToken != "" && (len(pageToken) > MaxTokenBytes || !isASCII(pageToken)) {
		return Page{}, stats, fmt.Errorf("%w: invalid cursor encoding", ErrCursorUnavailable)
	}
	window := s.window
	if s.windowAt != nil {
		window = utcWindow(s.windowAt(s.now().UTC()))
	}
	if !validWindow(window) || request.Start.Before(window.Start) || request.End.After(window.End) {
		if pageToken != "" {
			return Page{}, stats, fmt.Errorf("%w: window unavailable", ErrCursorUnavailable)
		}
		return Page{}, stats, ErrUnavailable
	}
	authorityStart := time.Now()
	captured, err := s.store.CaptureCachedPageAuthority(ctx, request)
	stats.AuthorityNS = time.Since(authorityStart).Nanoseconds()
	if err != nil {
		if pageToken != "" {
			return Page{}, stats, fmt.Errorf("%w: source authority is no longer available", ErrCursorUnavailable)
		}
		return Page{}, stats, ErrUnavailable
	}
	if err := validateTickets(request, captured); err != nil {
		if pageToken != "" {
			return Page{}, stats, fmt.Errorf("%w: source authority is no longer available", ErrCursorUnavailable)
		}
		return Page{}, stats, ErrUnavailable
	}
	binding := pageBinding(request, captured, caller, window)
	var after *Position
	expiry := s.now().UTC().Add(cursorLifetime)
	if pageToken != "" {
		position, decodedExpiry, decodeErr := s.codec.Decode(pageToken, binding)
		if decodeErr != nil || decodedExpiry.After(s.now().UTC().Add(cursorLifetime)) || validateCursorRequest(request, position) != nil {
			return Page{}, stats, fmt.Errorf("%w: invalid, expired, or stale cursor", ErrCursorUnavailable)
		}
		after = &position
		expiry = decodedExpiry
	}

	readStart := time.Now()
	read, err := s.store.ReadCachedPage(ctx, request, after)
	stats.ReadNS = time.Since(readStart).Nanoseconds()
	if err != nil {
		if errors.Is(err, storage.ErrCachedPageUnavailable) || errors.Is(err, storage.ErrInvalidCachedPageData) {
			if pageToken != "" {
				return Page{}, stats, fmt.Errorf("%w: source scope changed", ErrCursorUnavailable)
			}
			return Page{}, stats, ErrUnavailable
		}
		return Page{}, stats, fmt.Errorf("read cached event page: %w", err)
	}
	stats.Read = read.Stats
	if !sameTickets(captured, read.Tickets) {
		if pageToken != "" {
			return Page{}, stats, fmt.Errorf("%w: source proof changed", ErrCursorUnavailable)
		}
		return Page{}, stats, ErrUnavailable
	}

	packStart := time.Now()
	page, err := Pack(read, request.PageSize, func(position Position) (string, error) {
		return s.codec.Encode(position, binding, expiry)
	})
	stats.PackNS = time.Since(packStart).Nanoseconds()
	if err != nil {
		return Page{}, stats, err
	}
	// Packing or a stalled reader can cross the absolute cursor deadline. Do
	// not release a page with a continuation that has already expired.
	if !expiry.After(s.now().UTC()) {
		if pageToken != "" {
			return Page{}, stats, ErrCursorUnavailable
		}
		return Page{}, stats, ErrUnavailable
	}
	if s.testBeforeFinalValidation != nil {
		s.testBeforeFinalValidation()
	}
	validationStart := time.Now()
	err = s.store.ValidateAuthorityTickets(ctx, captured)
	stats.FinalValidationNS = time.Since(validationStart).Nanoseconds()
	if err != nil {
		if pageToken != "" {
			return Page{}, stats, fmt.Errorf("%w: source authority changed", ErrCursorUnavailable)
		}
		return Page{}, stats, ErrUnavailable
	}
	return page, stats, nil
}

func normalizeRequest(request Request) (Request, error) {
	request.Start = request.Start.UTC()
	request.End = request.End.UTC()
	if request.PageSize == 0 {
		request.PageSize = defaultPageSize
	}
	if request.PageSize < 1 || request.PageSize > maxPageSize || !request.End.After(request.Start) || request.Start.Year() < 1 || request.End.Year() > 9999 {
		return Request{}, errors.New("invalid date range or page size")
	}
	if len(request.CalendarIDs) < 1 || len(request.CalendarIDs) > maxScopeSize {
		return Request{}, errors.New("calendar scope must contain 1 to 16 IDs")
	}
	ids := make(map[string]struct{}, len(request.CalendarIDs))
	for _, id := range request.CalendarIDs {
		if id == "" || len(id) > 1024 || !utf8.ValidString(id) {
			return Request{}, errors.New("invalid calendar ID")
		}
		provider, rest, ok := strings.Cut(id, ":")
		connectionID, providerCalendarID, found := strings.Cut(rest, ":")
		if ok && provider != "google" {
			return Request{}, ErrUnavailable
		}
		if !ok || !found || connectionID == "" || providerCalendarID == "" {
			return Request{}, errors.New("unsupported calendar ID")
		}
		if _, duplicate := ids[id]; duplicate {
			return Request{}, errors.New("duplicate calendar ID")
		}
		ids[id] = struct{}{}
	}
	if len(ids) == 0 || len(ids) > maxScopeSize {
		return Request{}, errors.New("calendar scope must contain 1 to 16 unique IDs")
	}
	request.CalendarIDs = make([]string, 0, len(ids))
	for id := range ids {
		request.CalendarIDs = append(request.CalendarIDs, id)
	}
	sort.Strings(request.CalendarIDs)
	return request, nil
}

func validateCursorRequest(request Request, position Position) error {
	if err := validatePosition(position); err != nil {
		return err
	}
	for _, id := range request.CalendarIDs {
		if id == position.CalendarID {
			return nil
		}
	}
	return errors.New("cursor calendar is outside request scope")
}

func validateTickets(request Request, tickets []storage.AuthorityTicket) error {
	if len(tickets) != len(request.CalendarIDs) {
		return ErrUnavailable
	}
	seen := make(map[string]bool, len(tickets))
	for _, ticket := range tickets {
		if ticket.ConnectionID == "" || ticket.CalendarID == "" || ticket.AuthorizationRevision == "" || ticket.DetailReceipt == "" || len(ticket.ConnectionID) > 1024 || len(ticket.AuthorizationRevision) > 128 || len(ticket.DetailReceipt) > 1024 || seen[ticket.CalendarID] {
			return ErrUnavailable
		}
		seen[ticket.CalendarID] = true
	}
	for _, id := range request.CalendarIDs {
		if !seen[id] {
			return ErrUnavailable
		}
	}
	return nil
}

func sameTickets(a, b []storage.AuthorityTicket) bool {
	if len(a) != len(b) {
		return false
	}
	byCalendar := make(map[string]storage.AuthorityTicket, len(a))
	for _, ticket := range a {
		byCalendar[ticket.CalendarID] = ticket
	}
	for _, ticket := range b {
		prior, ok := byCalendar[ticket.CalendarID]
		if !ok || prior.ConnectionID != ticket.ConnectionID || prior.AuthorizationRevision != ticket.AuthorizationRevision || prior.DetailReceipt != ticket.DetailReceipt {
			return false
		}
	}
	return true
}

func pageBinding(request Request, tickets []storage.AuthorityTicket, caller auth.VerifiedCaller, window storage.SyncWindow) [32]byte {
	tickets = append([]storage.AuthorityTicket(nil), tickets...)
	sort.Slice(tickets, func(i, j int) bool { return tickets[i].CalendarID < tickets[j].CalendarID })
	material := struct {
		Purpose       string                   `json:"purpose"`
		BootNonce     [32]byte                 `json:"boot_nonce"`
		Tool          string                   `json:"tool"`
		Catalog       string                   `json:"catalog"`
		Schema        string                   `json:"schema"`
		Key           [32]byte                 `json:"key"`
		Configuration [32]byte                 `json:"configuration"`
		CalendarIDs   []string                 `json:"calendar_ids"`
		Start         time.Time                `json:"start"`
		End           time.Time                `json:"end"`
		PageSize      int                      `json:"page_size"`
		WindowStart   time.Time                `json:"window_start"`
		WindowEnd     time.Time                `json:"window_end"`
		Limits        [3]int                   `json:"limits"`
		Sources       []bindingAuthorityTicket `json:"sources"`
	}{
		Purpose: "calendar-cached-pages-binding:v1", BootNonce: processBootNonce,
		Tool: "get_cached_events", Catalog: "modern", Schema: "get_cached_events:v1",
		Key: caller.KeyFingerprint(), Configuration: caller.ConfigurationFingerprint(),
		CalendarIDs: request.CalendarIDs, Start: request.Start.UTC(), End: request.End.UTC(), PageSize: request.PageSize,
		WindowStart: window.Start.UTC(), WindowEnd: window.End.UTC(),
		Limits:  [3]int{MaxResultBytes, MaxPayloadBytes, MaxTokenBytes},
		Sources: make([]bindingAuthorityTicket, 0, len(tickets)),
	}
	for _, ticket := range tickets {
		material.Sources = append(material.Sources, bindingAuthorityTicket{ConnectionID: ticket.ConnectionID, CalendarID: ticket.CalendarID, Revision: ticket.AuthorizationRevision, Receipt: ticket.DetailReceipt})
	}
	encoded, _ := json.Marshal(material)
	return sha256.Sum256(encoded)
}

type bindingAuthorityTicket struct {
	ConnectionID string `json:"connection_id"`
	CalendarID   string `json:"calendar_id"`
	Revision     string `json:"revision"`
	Receipt      string `json:"receipt"`
}
