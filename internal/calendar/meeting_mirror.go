package calendar

import (
	"errors"
	"html"
	"net/url"
	"strings"
)

var ErrMeetingMarkerAmbiguous = errors.New("sync_meeting_marker_ambiguous")

const meetingStart = "<!-- calendar-mcp:meeting:start -->"
const meetingEnd = "<!-- calendar-mcp:meeting:end -->"

// MirrorMeetingURL accepts only HTTPS video entry points for known providers.
// Keep the raw URL intact because paths and query parameters can authorize entry.
func MirrorMeetingURL(conference *ConferenceData) string {
	if conference == nil {
		return ""
	}
	for _, entry := range conference.EntryPoints {
		if !strings.EqualFold(entry.Type, "video") {
			continue
		}
		u, err := url.Parse(entry.URI)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Hostname() == "" {
			continue
		}
		host := strings.ToLower(u.Hostname())
		if host == "meet.google.com" || host == "teams.microsoft.com" || host == "teams.live.com" || host == "zoom.us" || strings.HasSuffix(host, ".zoom.us") {
			return entry.URI
		}
	}
	return ""
}

func HasMirrorMeetingMarker(description string) bool {
	return strings.Contains(description, meetingStart) || strings.Contains(description, meetingEnd)
}

// ContainsMirrorMeetingURL matches a complete URL, rather than a URL prefix.
// HTML entities are decoded so a URL in an href is recognized as well.
func ContainsMirrorMeetingURL(text, meetingURL string) bool {
	if meetingURL == "" {
		return false
	}
	text = html.UnescapeString(text)
	for offset := 0; offset < len(text); {
		i := strings.Index(text[offset:], meetingURL)
		if i < 0 {
			return false
		}
		i += offset
		end := i + len(meetingURL)
		if (i == 0 || !isURLByte(text[i-1])) && (end == len(text) || !isURLByte(text[end])) {
			return true
		}
		offset = i + 1
	}
	return false
}

func isURLByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~:/?#[]@!$&()*+,;=%", rune(c))
}

// MergeMirrorMeeting modifies only the managed fragment. Ambiguous markers are
// rejected without returning the description or URL in the error.
func MergeMirrorMeeting(description, meetingURL string) (string, error) {
	starts, ends := strings.Count(description, meetingStart), strings.Count(description, meetingEnd)
	if starts != ends || starts > 1 {
		return "", ErrMeetingMarkerAmbiguous
	}
	fragment := ""
	if meetingURL != "" {
		fragment = meetingStart + `<br><strong>Join meeting:</strong> <a href="` + html.EscapeString(meetingURL) + `">` + html.EscapeString(meetingURL) + `</a>` + meetingEnd
	}
	before, after := description, ""
	if starts != 0 {
		start, end := strings.Index(description, meetingStart), strings.Index(description, meetingEnd)
		if end < start {
			return "", ErrMeetingMarkerAmbiguous
		}
		before, after = description[:start], description[end+len(meetingEnd):]
	}
	if meetingURL != "" && ContainsMirrorMeetingURL(before+after, meetingURL) {
		fragment = ""
	}
	return before + fragment + after, nil
}

// PreserveMirrorMeeting carries the existing managed fragment into a new body
// when the source conference state is unknown. Both bodies must be unambiguous.
func PreserveMirrorMeeting(description, existing string) (string, error) {
	if HasMirrorMeetingMarker(description) {
		return "", ErrMeetingMarkerAmbiguous
	}
	if _, err := MergeMirrorMeeting(existing, ""); err != nil {
		return "", err
	}
	start := strings.Index(existing, meetingStart)
	if start < 0 {
		return description, nil
	}
	end := strings.Index(existing, meetingEnd) + len(meetingEnd)
	return description + existing[start:end], nil
}
