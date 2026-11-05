package google

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode"
)

// ResolvePhysicalCalendarKey returns a stable, provider-scoped key for the ID
// returned by Google CalendarList.Get. It performs only a metadata read; no
// mutation may be dispatched solely on the strength of this identity. The
// caller must still establish ownership and fence every writer to this calendar.
// Equal keys identify matching returned IDs; differing keys do not prove that
// aliases refer to different calendars. The key is not anonymized data.
func (p *Provider) ResolvePhysicalCalendarKey(ctx context.Context, calendarID string) (string, error) {
	if calendarID == "" || strings.TrimSpace(calendarID) != calendarID {
		return "", errors.New("calendar identity unavailable")
	}
	entry, err := p.svc.CalendarList.Get(calendarID).Context(ctx).Do()
	if err != nil {
		return "", errors.New("calendar identity unavailable")
	}
	id := entry.Id
	if id == "" || strings.EqualFold(id, "primary") || strings.TrimSpace(id) != id || strings.IndexFunc(id, unicode.IsControl) >= 0 || (calendarID == "primary" && !entry.Primary) || entry.Deleted {
		return "", errors.New("calendar identity unavailable")
	}
	digest := sha256.Sum256([]byte("google\x00" + id))
	return "google:" + hex.EncodeToString(digest[:]), nil
}
