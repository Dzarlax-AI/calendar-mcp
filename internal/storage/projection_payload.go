package storage

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"calendar-mcp/internal/calendar"
)

// These envelopes are private storage formats. Completeness is an adapter
// assertion; decoding it does not establish provider provenance or integrity.
type projectionAttendeeEvidence struct {
	EventID  string `json:"event_id"`
	Complete *bool  `json:"complete"`
}
type projectionPagePayload struct {
	Version  int                          `json:"version"`
	Page     *calendar.EventSyncPage      `json:"page"`
	Evidence []projectionAttendeeEvidence `json:"attendee_evidence"`
}
type projectionSnapshotPayload struct {
	Version  int                          `json:"version"`
	Events   *[]calendar.EventV2          `json:"events"`
	Evidence []projectionAttendeeEvidence `json:"attendee_evidence"`
}

func projectionEvidence(events []calendar.EventV2, source string) ([]projectionAttendeeEvidence, error) {
	if source == "" {
		return nil, ErrProjectionPageStage
	}
	evidence := make([]projectionAttendeeEvidence, 0, len(events))
	seen := make(map[string]bool, len(events))
	for _, event := range events {
		if event.ID == "" || event.CalendarID != source || seen[event.ID] {
			return nil, ErrProjectionPageStage
		}
		seen[event.ID] = true
		complete := event.SourceAttendeesComplete
		evidence = append(evidence, projectionAttendeeEvidence{EventID: event.ID, Complete: &complete})
	}
	return evidence, nil
}
func restoreProjectionEvidence(events []calendar.EventV2, evidence []projectionAttendeeEvidence, source string) error {
	if evidence == nil || len(evidence) != len(events) {
		return ErrProjectionPageStage
	}
	if _, err := projectionEvidence(events, source); err != nil {
		return err
	}
	byID := make(map[string]bool, len(evidence))
	for _, item := range evidence {
		if item.EventID == "" || item.Complete == nil {
			return ErrProjectionPageStage
		}
		if _, exists := byID[item.EventID]; exists {
			return ErrProjectionPageStage
		}
		byID[item.EventID] = *item.Complete
	}
	for i := range events {
		complete, ok := byID[events[i].ID]
		if !ok {
			return ErrProjectionPageStage
		}
		events[i].SourceAttendeesComplete = complete
	}
	return nil
}
func decodeProjectionPayload(data []byte, max int, value any) error {
	if len(data) == 0 || len(data) > max {
		return ErrProjectionPageStage
	}
	if err := rejectProjectionDuplicateKeys(data); err != nil {
		return ErrProjectionPageStage
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return ErrProjectionPageStage
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrProjectionPageStage
	}
	return nil
}

// Reject duplicate object members, including duplicates nested in provider data.
func rejectProjectionDuplicateKeys(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var value func() error
	value = func() error {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				k, ok := key.(string)
				k = strings.ToLower(k)
				if !ok || seen[k] {
					return ErrProjectionPageStage
				}
				seen[k] = true
				if err := value(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		case '[':
			for d.More() {
				if err := value(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		default:
			return ErrProjectionPageStage
		}
	}
	return value()
}
func encodeProjectionPagePayload(page calendar.EventSyncPage, source string) ([]byte, error) {
	events := make([]calendar.EventV2, len(page.Upserts))
	for i := range page.Upserts {
		events[i] = page.Upserts[i].Event
	}
	evidence, err := projectionEvidence(events, source)
	if err != nil {
		return nil, err
	}
	return json.Marshal(projectionPagePayload{Version: 1, Page: &page, Evidence: evidence})
}
func decodeProjectionPagePayload(data []byte, source string) (calendar.EventSyncPage, error) {
	var envelope projectionPagePayload
	if err := decodeProjectionPayload(data, projectionPageSizeLimit, &envelope); err != nil || envelope.Version != 1 || envelope.Page == nil {
		return calendar.EventSyncPage{}, ErrProjectionPageStage
	}
	page := *envelope.Page
	if page.NextPageToken != "" || page.NextCursor != "" || page.ResetRequired || len(page.Warnings) != 0 {
		return calendar.EventSyncPage{}, ErrProjectionPageStage
	}
	events := make([]calendar.EventV2, len(page.Upserts))
	for i := range page.Upserts {
		events[i] = page.Upserts[i].Event
		if page.Upserts[i].Object.ObjectID != events[i].ID {
			return calendar.EventSyncPage{}, ErrProjectionPageStage
		}
	}
	if err := restoreProjectionEvidence(events, envelope.Evidence, source); err != nil {
		return calendar.EventSyncPage{}, err
	}
	for i := range page.Upserts {
		page.Upserts[i].Event = events[i]
	}
	return page, nil
}
func encodeProjectionSnapshotPayload(events []calendar.EventV2, source string) ([]byte, error) {
	evidence, err := projectionEvidence(events, source)
	if err != nil {
		return nil, err
	}
	if events == nil {
		events = []calendar.EventV2{}
	}
	return json.Marshal(projectionSnapshotPayload{Version: 1, Events: &events, Evidence: evidence})
}
func decodeProjectionSnapshotPayload(data []byte, source string) ([]calendar.EventV2, error) {
	var envelope projectionSnapshotPayload
	if err := decodeProjectionPayload(data, maxProjectionSnapshotBytes, &envelope); err != nil || envelope.Version != 1 || envelope.Events == nil || *envelope.Events == nil {
		return nil, ErrProjectionPageStage
	}
	events := *envelope.Events
	if err := restoreProjectionEvidence(events, envelope.Evidence, source); err != nil {
		return nil, err
	}
	return events, nil
}
