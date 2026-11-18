package cachedpages

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// MetadataOverflowError reports that mandatory source/page metadata cannot fit
// within the public tool-result budget.
type MetadataOverflowError struct{ Bytes int }

func (e *MetadataOverflowError) Error() string {
	return fmt.Sprintf("page metadata exceeds result size limit (%d bytes)", e.Bytes)
}

// Pack selects whole summaries in reader order and mints a continuation only
// for the last row actually emitted. read.Rows may contain at most one extra
// lookahead row beyond pageSize.
func Pack(read ReadPage, pageSize int, mint func(Position) (string, error)) (Page, error) {
	if pageSize < 1 || pageSize > 50 {
		return Page{}, errors.New("page size must be between 1 and 50")
	}
	if len(read.Rows) > pageSize+1 {
		return Page{}, errors.New("reader returned more rows than page size plus lookahead")
	}
	page := Page{
		Items:       make([]Summary, 0, min(len(read.Rows), pageSize)),
		Sources:     append([]Source{}, read.Sources...),
		Consistency: "cached_weak",
		ObservedAt:  read.ObservedAt,
	}
	if !fitsWire(page) {
		return Page{}, &MetadataOverflowError{Bytes: wireSize(page)}
	}
	sizer, err := newPageWireSizer(page)
	if err != nil {
		return Page{}, fmt.Errorf("account page size: %w", err)
	}

	consumed := 0
	for consumed < len(read.Rows) && consumed < pageSize {
		row := read.Rows[consumed]
		candidate := row.Summary
		candidatePage := page
		candidatePage.Items = append(page.Items, candidate)
		candidatePage.Partial = page.Partial || rowMarker(candidate)
		candidateSizer := sizer
		if candidateSizer.add(candidate) != nil {
			return Page{}, &MetadataOverflowError{Bytes: MaxResultBytes + 1}
		}
		if candidatePage.Partial != page.Partial {
			candidateSizer, err = sizerForItems(candidatePage, candidatePage.Items)
			if err != nil {
				return Page{}, fmt.Errorf("account page size: %w", err)
			}
		}
		if candidateSizer.size() <= MaxResultBytes {
			page = candidatePage
			sizer = candidateSizer
			consumed++
			continue
		}
		if len(page.Items) != 0 {
			break
		}

		// If the full summary cannot fit on an otherwise empty page, emit one
		// explicit marker while retaining the row's real keyset position.
		if !rowMarker(candidate) {
			candidate = MarkerSummary(candidate)
		}
		page.Items = append(page.Items, candidate)
		page.Partial = true
		if !fitsWire(page) {
			return Page{}, &MetadataOverflowError{Bytes: wireSize(page)}
		}
		sizer, err = sizerForItems(page, page.Items)
		if err != nil {
			return Page{}, fmt.Errorf("account page size: %w", err)
		}
		consumed++
	}

	more := consumed < len(read.Rows)
	if more {
		if mint == nil {
			return Page{}, errors.New("page token mint function is required when another row exists")
		}
		for {
			if len(page.Items) == 0 || consumed == 0 {
				return Page{}, &MetadataOverflowError{Bytes: wireSize(page)}
			}
			token, err := mint(read.Rows[consumed-1].Position)
			if err != nil {
				return Page{}, fmt.Errorf("mint page token: %w", err)
			}
			withCursor := page
			withCursor.NextPageToken = token
			if fitsWire(withCursor) {
				page = withCursor
				break
			}
			if consumed == 1 {
				// A summary that fits alone can still fail once the mandatory cursor
				// is included. Keep its key/time/status and retry with a compact marker.
				if !rowMarker(page.Items[0]) {
					page.Items[0] = MarkerSummary(page.Items[0])
					page.Partial = true
					sizer, err = sizerForItems(page, page.Items)
					if err != nil {
						return Page{}, fmt.Errorf("account page size: %w", err)
					}
					withCursor = page
					withCursor.NextPageToken = token
					if fitsWire(withCursor) {
						page = withCursor
						break
					}
				}
				return Page{}, &MetadataOverflowError{Bytes: wireSize(withCursor)}
			}
			page.Items = page.Items[:len(page.Items)-1]
			consumed--
			page.Partial = hasMarker(page.Items)
			sizer, err = sizerForItems(page, page.Items)
			if err != nil {
				return Page{}, fmt.Errorf("account page size: %w", err)
			}
		}
	} else {
		page.Complete = true
	}
	if !fitsWire(page) {
		return Page{}, &MetadataOverflowError{Bytes: wireSize(page)}
	}
	return page, nil
}

// NewToolResult constructs the exact SDK result wire object and returns its
// encoded byte count. MCP handlers can reuse this result and enforce the same
// budget as Pack.
func NewToolResult(page Page) (*mcp.CallToolResult, int, error) {
	pageJSON, err := json.Marshal(page)
	if err != nil {
		return nil, 0, err
	}
	result := mcp.NewToolResultStructured(page, string(pageJSON))
	wire, err := json.Marshal(result)
	if err != nil {
		return nil, 0, err
	}
	return result, len(wire), nil
}

func fitsWire(page Page) bool { return wireSize(page) <= MaxResultBytes }

func wireSize(page Page) int {
	_, size, err := NewToolResult(page)
	if err != nil {
		return MaxResultBytes + 1
	}
	return size
}

// pageWireSizer tracks both structured JSON and its escaped text representation
// incrementally. This matches the SDK wire shape without serializing each full
// page prefix; Pack still performs one exact final SDK serialization check.
type pageWireSizer struct {
	baseJSONLen    int
	baseEscapedLen int
	baseOuterLen   int
	itemJSONLen    int
	itemEscapedLen int
	count          int
}

func newPageWireSizer(page Page) (pageWireSizer, error) {
	page.Items = []Summary{}
	pageJSON, err := json.Marshal(page)
	if err != nil {
		return pageWireSizer{}, err
	}
	result, err := json.Marshal(mcp.NewToolResultStructured(page, string(pageJSON)))
	if err != nil {
		return pageWireSizer{}, err
	}
	return pageWireSizer{
		baseJSONLen:    len(pageJSON),
		baseEscapedLen: escapedJSONLen(string(pageJSON)),
		baseOuterLen:   len(result) - len(pageJSON) - escapedJSONLen(string(pageJSON)),
	}, nil
}

func sizerForItems(page Page, items []Summary) (pageWireSizer, error) {
	sizer, err := newPageWireSizer(page)
	if err != nil {
		return pageWireSizer{}, err
	}
	for _, item := range items {
		if err := sizer.add(item); err != nil {
			return pageWireSizer{}, err
		}
	}
	return sizer, nil
}

func (s *pageWireSizer) add(item Summary) error {
	encoded, err := json.Marshal(item)
	if err != nil {
		return err
	}
	s.itemJSONLen += len(encoded)
	s.itemEscapedLen += escapedJSONLen(string(encoded))
	s.count++
	return nil
}

func (s pageWireSizer) size() int {
	commas := max(0, s.count-1)
	return s.baseOuterLen + s.baseJSONLen + s.itemJSONLen + commas + s.baseEscapedLen + s.itemEscapedLen + commas
}

func escapedJSONLen(value string) int {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) < 2 {
		return MaxResultBytes + 1
	}
	return len(encoded) - 2
}

func rowMarker(summary Summary) bool {
	kind := strings.ToLower(summary.Kind)
	return kind == "marker" || kind == "oversized" || kind == "raw_source" || kind == "source_marker"
}

func hasMarker(items []Summary) bool {
	for _, item := range items {
		if rowMarker(item) {
			return true
		}
	}
	return false
}
