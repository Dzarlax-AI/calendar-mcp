package storage

import (
	"encoding/json"
	"testing"

	"calendar-mcp/internal/calendar"
)

func TestValidateProjectionPageChain(t *testing.T) {
	const source, zone, cursor = "source", "UTC", "terminal-cursor"
	makeRecord := func(t *testing.T, ordinal int, page calendar.EventSyncPage, previous, next, terminal string) projectionStagedPage {
		t.Helper()
		payload, err := encodeProjectionPagePayload(page, source)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(struct {
			Payload                json.RawMessage
			Previous, Next, Cursor string
		}{payload, previous, next, terminal})
		if err != nil {
			t.Fatal(err)
		}
		return projectionStagedPage{ordinal: ordinal, digest: projectionPageHash(b), previous: previous, next: next, payload: payload}
	}
	empty, next := projectionPageHash(nil), projectionPageHash([]byte("page-token"))
	first := makeRecord(t, 0, calendar.EventSyncPage{SourceTimeZone: zone}, empty, next, "")
	last := makeRecord(t, 1, calendar.EventSyncPage{SourceTimeZone: zone, Complete: true}, next, empty, cursor)
	pages := []projectionStagedPage{first, last}
	total := int64(len(first.payload) + len(last.payload))
	if out, err := validateProjectionPageChain(source, zone, cursor, empty, total, pages); err != nil || len(out) != 2 {
		t.Fatalf("valid chain: %v", err)
	}
	tests := []struct {
		name         string
		mutate       func([]projectionStagedPage)
		cursor, hash string
		total        int64
	}{
		{"missing page", func(p []projectionStagedPage) { p[0].ordinal = 1 }, cursor, empty, total},
		{"payload changed", func(p []projectionStagedPage) {
			p[0].payload = append([]byte(nil), p[0].payload...)
			p[0].payload[len(p[0].payload)-2] ^= 1
		}, cursor, empty, total},
		{"token chain changed", func(p []projectionStagedPage) { p[1].previous = empty }, cursor, empty, total},
		{"invalid terminal token", func(p []projectionStagedPage) { p[1].next = next }, cursor, empty, total},
		{"wrong cursor", func([]projectionStagedPage) {}, "different", empty, total},
		{"wrong total", func([]projectionStagedPage) {}, cursor, empty, total + 1},
		{"wrong last hash", func([]projectionStagedPage) {}, cursor, next, total},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := append([]projectionStagedPage(nil), pages...)
			tt.mutate(p)
			if _, err := validateProjectionPageChain(source, zone, tt.cursor, tt.hash, tt.total, p); err == nil {
				t.Fatal("accepted invalid chain")
			}
		})
	}
}
