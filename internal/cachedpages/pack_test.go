package cachedpages

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/credentials"
)

func testSummary(id, title string) Summary {
	return Summary{
		Kind: "event", EventID: id, CalendarID: "google:primary", Title: title,
		Start: calendar.EventTime{DateTime: "2026-11-17T10:00:00+01:00", TimeZone: "Europe/Belgrade"},
		End:   calendar.EventTime{DateTime: "2026-11-17T10:30:00+01:00", TimeZone: "Europe/Belgrade"},
	}
}

func testPosition(id string) Position {
	return Position{StartAt: "2026-11-17T09:00:00Z", EventID: id, CalendarID: "google:primary"}
}

func TestPackKeepsWholeNextSummaryAndUsesLastEmittedCursor(t *testing.T) {
	large := testSummary("large", "")
	found := false
	for n := 1; n < MaxResultBytes; n++ {
		large.Title = strings.Repeat("я🧭", n)
		alone := Page{Items: []Summary{large}, Sources: []Source{}, Consistency: "cached_weak"}
		withFirst := Page{Items: []Summary{testSummary("first", "first"), large}, Sources: []Source{}, Consistency: "cached_weak"}
		_, aloneSize, err := NewToolResult(alone)
		if err != nil {
			t.Fatal(err)
		}
		_, combinedSize, err := NewToolResult(withFirst)
		if err != nil {
			t.Fatal(err)
		}
		if aloneSize <= MaxResultBytes && combinedSize > MaxResultBytes {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("could not construct summary that fits alone but not after the first item")
	}
	rows := []Row{
		{Summary: testSummary("first", "first"), Position: testPosition("first")},
		{Summary: large, Position: testPosition("large")},
		{Summary: testSummary("lookahead", "lookahead"), Position: testPosition("lookahead")},
	}
	var minted []Position
	page, err := Pack(ReadPage{Rows: rows}, 2, func(p Position) (string, error) {
		minted = append(minted, p)
		return "cursor:" + p.EventID, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].EventID != "first" || page.NextPageToken != "cursor:first" || page.Complete {
		t.Fatalf("unexpected first page: %#v", page)
	}
	if len(minted) != 1 || minted[0] != testPosition("first") {
		t.Fatalf("cursor did not use last emitted position: %#v", minted)
	}
	second, err := Pack(ReadPage{Rows: rows[1:]}, 1, func(p Position) (string, error) { return "cursor:" + p.EventID, nil })
	if err != nil {
		t.Fatal(err)
	}
	if second.Items[0] != large || second.NextPageToken != "cursor:large" || second.Complete {
		t.Fatalf("fitting summary was not preserved whole: %#v", second)
	}
}

func TestPackTerminalPageIsCompleteAndDoesNotMintCursor(t *testing.T) {
	called := false
	page, err := Pack(ReadPage{Rows: []Row{{Summary: testSummary("only", "only"), Position: testPosition("only")}}}, 25, func(Position) (string, error) {
		called = true
		return "unexpected", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Complete || page.NextPageToken != "" || called {
		t.Fatalf("terminal page completeness/cursor mismatch: %#v called=%v", page, called)
	}
}

func TestPackOversizedSummaryBecomesProgressMarker(t *testing.T) {
	jumbo := testSummary("jumbo", strings.Repeat("x", 12000))
	var minted Position
	page, err := Pack(ReadPage{Rows: []Row{
		{Summary: jumbo, Position: testPosition("jumbo")},
		{Summary: testSummary("after", "after"), Position: testPosition("after")},
	}}, 1, func(p Position) (string, error) { minted = p; return "next", nil })
	if err != nil {
		t.Fatal(err)
	}
	item := page.Items[0]
	if item.Kind != "oversized" || item.EventID != "jumbo" || item.Title != "" || item.Start != jumbo.Start || item.End != jumbo.End || item.Status != jumbo.Status || item.Reason != "result_size_limit" {
		t.Fatalf("marker did not retain required identity/range fields: %#v", item)
	}
	if !page.Partial || page.Complete || page.NextPageToken != "next" || minted != testPosition("jumbo") {
		t.Fatalf("marker continuation mismatch: page=%#v position=%#v", page, minted)
	}
}

func TestPackMarkerWhenMandatoryCursorPushesSoleItemOverBudget(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	cipher, err := credentials.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	codec := NewCodec(cipher, func() time.Time { return now })
	var binding [32]byte
	binding[0] = 9
	expiry := now.Add(5 * time.Minute)
	position := testPosition("boundary")
	token, err := codec.Encode(position, binding, expiry)
	if err != nil {
		t.Fatal(err)
	}
	observed := now
	var summary Summary
	found := false
	for n := 1; n < MaxResultBytes; n++ {
		summary = testSummary("boundary", strings.Repeat("я", n))
		without := Page{Items: []Summary{summary}, Sources: []Source{}, Consistency: "cached_weak", ObservedAt: observed}
		with := without
		with.NextPageToken = token
		_, withoutSize, err := NewToolResult(without)
		if err != nil {
			t.Fatal(err)
		}
		_, withSize, err := NewToolResult(with)
		if err != nil {
			t.Fatal(err)
		}
		if withoutSize <= MaxResultBytes && withSize > MaxResultBytes && withoutSize > MaxResultBytes-512 {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("could not find mandatory cursor budget boundary")
	}
	mints := 0
	page, err := Pack(ReadPage{Rows: []Row{
		{Summary: summary, Position: position},
		{Summary: testSummary("lookahead", "next"), Position: testPosition("lookahead")},
	}, ObservedAt: observed}, 1, func(p Position) (string, error) {
		mints++
		return codec.Encode(p, binding, expiry)
	})
	if err != nil {
		t.Fatal(err)
	}
	if mints != 1 || page.Items[0].Kind != "oversized" || !page.Partial || page.Complete || page.NextPageToken == "" {
		t.Fatalf("expected progressing marker: mints=%d page=%#v", mints, page)
	}
	decoded, _, err := codec.Decode(page.NextPageToken, binding)
	if err != nil || decoded != position {
		t.Fatalf("marker cursor lost original row position: %#v %v", decoded, err)
	}
}

func TestNewToolResultCountsSDKTextStructuredAndEscaping(t *testing.T) {
	page := Page{Items: []Summary{}, Sources: []Source{}, Consistency: "cached_weak", ObservedAt: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)}
	sizer, err := newPageWireSizer(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"plain", `quote " slash \\ newline\n`, "<tag>&", "Встреча 🧭", "\u2028\u2029"} {
		item := testSummary(value, value)
		page.Items = append(page.Items, item)
		result, size, err := NewToolResult(page)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if size != len(wire) {
			t.Fatalf("wire byte count mismatch: got %d, serialized %d", size, len(wire))
		}
		if err := sizer.add(item); err != nil {
			t.Fatal(err)
		}
		if got := sizer.size(); got != size {
			t.Fatalf("incremental wire accounting mismatch for %q: got %d, SDK result %d", value, got, size)
		}
	}
}

func TestPackBacksOffWhenCursorOverflowsBudget(t *testing.T) {
	rows := []Row{
		{Summary: testSummary("first", strings.Repeat("x", 7000)), Position: testPosition("first")},
		{Summary: testSummary("second", "second"), Position: testPosition("second")},
		{Summary: testSummary("lookahead", "lookahead"), Position: testPosition("lookahead")},
	}
	var calls []Position
	page, err := Pack(ReadPage{Rows: rows}, 2, func(p Position) (string, error) {
		calls = append(calls, p)
		if p.EventID == "second" {
			return strings.Repeat("x", MaxTokenBytes), nil
		}
		return "cursor", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].EventID != "first" || page.NextPageToken != "cursor" || page.Complete {
		t.Fatalf("unexpected result after cursor backoff: %#v", page)
	}
	if len(calls) != 2 || calls[0].EventID != "second" || calls[1].EventID != "first" {
		t.Fatalf("expected cursor remint at backed-off position: %#v", calls)
	}
}

func TestPackReturnsTypedOverflowForUnfitMetadataAndCursor(t *testing.T) {
	sources := []Source{{CalendarID: "google:primary", Provider: "google", Status: strings.Repeat("s", 10000)}}
	_, err := Pack(ReadPage{Sources: sources}, 10, nil)
	var overflow *MetadataOverflowError
	if !errors.As(err, &overflow) {
		t.Fatalf("expected metadata overflow, got %v", err)
	}
	rows := []Row{{Summary: testSummary("one", strings.Repeat("x", 7000)), Position: testPosition("one")}, {Summary: testSummary("next", "next"), Position: testPosition("next")}}
	_, err = Pack(ReadPage{Rows: rows}, 1, func(Position) (string, error) { return strings.Repeat("x", MaxResultBytes), nil })
	if !errors.As(err, &overflow) {
		t.Fatalf("expected cursor overflow, got %v", err)
	}
}

func TestPackCompletesFullWalkWithoutSkippingRows(t *testing.T) {
	rows := make([]Row, 7)
	for i := range rows {
		id := string(rune('a' + i))
		rows[i] = Row{Summary: testSummary(id, id), Position: testPosition(id)}
	}
	var walked []string
	start := 0
	for start < len(rows) {
		end := min(start+3, len(rows))
		if end < len(rows) {
			end++ // one lookahead row
		}
		page, err := Pack(ReadPage{Rows: rows[start:end]}, 3, func(p Position) (string, error) { return p.EventID, nil })
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			walked = append(walked, item.EventID)
		}
		if page.Complete {
			if page.NextPageToken != "" {
				t.Fatalf("complete page has continuation: %#v", page)
			}
			break
		}
		if page.NextPageToken != page.Items[len(page.Items)-1].EventID {
			t.Fatalf("continuation does not identify last returned row: %#v", page)
		}
		start += len(page.Items)
	}
	if got, want := strings.Join(walked, ""), "abcdefg"; got != want {
		t.Fatalf("walk skipped or repeated rows: got %q want %q", got, want)
	}
}
