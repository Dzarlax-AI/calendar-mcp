package cachedpages

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/credentials"
)

func newTestCodec(t *testing.T, now *time.Time) *Codec {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	cipher, err := credentials.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	return NewCodec(cipher, func() time.Time { return *now })
}

func TestCodecRoundTripsBoundPosition(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 123, time.UTC)
	codec := newTestCodec(t, &now)
	position := Position{AllDay: false, StartAt: "2026-11-17T09:00:00.123456789+01:00", EventID: "встреча 🧭", CalendarID: "google:primary"}
	var binding [32]byte
	binding[0] = 7
	expiry := now.Add(5*time.Minute + 123*time.Nanosecond)
	token, err := codec.Encode(position, binding, expiry)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) > MaxTokenBytes || !isASCII(token) {
		t.Fatalf("token bound violated: bytes=%d", len(token))
	}
	got, gotExpiry, err := codec.Decode(token, binding)
	if err != nil {
		t.Fatal(err)
	}
	if got != position || !gotExpiry.Equal(expiry) {
		t.Fatalf("round trip mismatch: position=%#v expiry=%s", got, gotExpiry)
	}
}

func TestCodecRejectsTamperingBindingExpiryAndMalformedToken(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	codec := newTestCodec(t, &now)
	position := Position{StartAt: "2026-11-17T10:00:00Z", EventID: "event", CalendarID: "google:primary"}
	var binding, wrongBinding [32]byte
	binding[0], wrongBinding[0] = 1, 2
	token, err := codec.Encode(position, binding, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	index := len(token) / 2
	replacement := "A"
	if token[index] == 'A' {
		replacement = "B"
	}
	tampered := token[:index] + replacement + token[index+1:]
	if tampered == token {
		t.Fatal("tamper fixture did not change the encoded token")
	}
	for name, candidate := range map[string]struct {
		token   string
		binding [32]byte
	}{
		"tampered":      {tampered, binding},
		"wrong binding": {token, wrongBinding},
		"malformed":     {"not a token", binding},
		"non ascii":     {"é", binding},
		"oversized":     {strings.Repeat("A", MaxTokenBytes+1), binding},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := codec.Decode(candidate.token, candidate.binding); err == nil {
				t.Fatal("Decode accepted invalid token")
			}
		})
	}
	now = now.Add(time.Minute)
	if _, _, err := codec.Decode(token, binding); err == nil {
		t.Fatal("Decode accepted expired token")
	}
	otherKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x24}, 32))
	otherCipher, err := credentials.NewCipher(otherKey)
	if err != nil {
		t.Fatal(err)
	}
	otherCodec := NewCodec(otherCipher, func() time.Time { return now.Add(-time.Minute) })
	if _, _, err := otherCodec.Decode(token, binding); err == nil {
		t.Fatal("different cipher accepted cursor")
	}
}

func TestCodecRequiresConsistentTimedOrAllDayPosition(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	codec := newTestCodec(t, &now)
	var binding [32]byte
	for _, position := range []Position{
		{EventID: "missing-calendar"},
		{StartAt: "2026-11-17T10:00:00Z", StartDate: "2026-11-17", EventID: "mixed", CalendarID: "google:primary"},
		{AllDay: true, StartAt: "2026-11-17T10:00:00Z", StartDate: "2026-11-17", EventID: "mixed-all-day", CalendarID: "google:primary"},
		{AllDay: true, StartDate: "not-date", EventID: "bad-date", CalendarID: "google:primary"},
		{StartAt: strings.Repeat("x", maxCursorTimeBytes+1), EventID: "too-long-time", CalendarID: "google:primary"},
		{StartAt: "2026-11-17T10:00:00Z", EventID: strings.Repeat("x", maxCursorIDBytes+1), CalendarID: "google:primary"},
	} {
		if _, err := codec.Encode(position, binding, now.Add(time.Minute)); err == nil {
			t.Fatalf("Encode accepted invalid position: %#v", position)
		}
	}
	allDay := Position{AllDay: true, StartDate: "2026-11-17", EventID: "all-day", CalendarID: "apple:/family"}
	token, err := codec.Encode(allDay, binding, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := codec.Decode(token, binding)
	if err != nil || got != allDay {
		t.Fatalf("all-day position round trip failed: %#v %v", got, err)
	}
	sqliteKey := Position{StartAt: "2026-11-17 12:00:00 +0000 UTC", EventID: "sqlite", CalendarID: "google:primary"}
	token, err = codec.Encode(sqliteKey, binding, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	got, _, err = codec.Decode(token, binding)
	if err != nil || got != sqliteKey {
		t.Fatalf("raw SQLite seek key round trip failed: %#v %v", got, err)
	}
}
