package storage

import (
	"encoding/hex"
	"encoding/json"

	"calendar-mcp/internal/calendar"
)

// projectionStagedPage is a private, untrusted persisted record. This check
// establishes internal consistency only; it proves neither provider provenance
// nor the writer/rollout barriers needed for verified publication.
type projectionStagedPage struct {
	ordinal                int
	digest, previous, next string
	payload                []byte
}

// validateProjectionPageChain checks an entire candidate inside the caller's
// source-locking transaction. It must never itself authorize publication.
func validateProjectionPageChain(source, timezone, finalCursor, lastHash string, total int64, pages []projectionStagedPage) ([]calendar.EventSyncPage, error) {
	if source == "" || timezone == "" || finalCursor == "" || len(finalCursor) > 4096 || len(pages) == 0 || len(pages) > projectionPageLimit || total < 0 || total > projectionAttemptSizeLimit {
		return nil, ErrProjectionPageStage
	}
	expected := projectionPageHash(nil)
	empty := expected
	seen := map[string]bool{expected: true}
	var size int64
	out := make([]calendar.EventSyncPage, 0, len(pages))
	for i, record := range pages {
		if record.ordinal != i || record.previous != expected || len(record.payload) > projectionPageSizeLimit {
			return nil, ErrProjectionPageStage
		}
		raw, err := hex.DecodeString(record.next)
		if err != nil || len(raw) != 32 || record.next != hex.EncodeToString(raw) {
			return nil, ErrProjectionPageStage
		}
		page, err := decodeProjectionPagePayload(record.payload, source)
		if err != nil || page.SourceTimeZone != timezone || page.Complete != (i == len(pages)-1) {
			return nil, ErrProjectionPageStage
		}
		cursor := ""
		if page.Complete {
			if record.next != empty {
				return nil, ErrProjectionPageStage
			}
			cursor = finalCursor
			page.NextCursor = calendar.EventSyncCursor(finalCursor)
		} else {
			if record.next == empty || seen[record.next] {
				return nil, ErrProjectionPageStage
			}
			seen[record.next] = true
			// Actual page tokens are deliberately not persisted; validate structural
			// properties using a nonempty placeholder without revealing provider data.
			page.NextPageToken = "validated-token-placeholder"
		}
		digestInput, err := json.Marshal(struct {
			Payload                json.RawMessage
			Previous, Next, Cursor string
		}{record.payload, record.previous, record.next, cursor})
		if err != nil || projectionPageHash(digestInput) != record.digest || validateProjectionGooglePage(source, page) != nil {
			return nil, ErrProjectionPageStage
		}
		page.NextPageToken = ""
		page.NextCursor = ""
		size += int64(len(record.payload))
		if size > projectionAttemptSizeLimit {
			return nil, ErrProjectionPageStage
		}
		expected = record.next
		out = append(out, page)
	}
	if expected != lastHash || size != total {
		return nil, ErrProjectionPageStage
	}
	return out, nil
}
