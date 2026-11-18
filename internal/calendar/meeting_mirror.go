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
	if meetingURL != "" && strings.Contains(html.UnescapeString(before+after), meetingURL) {
		fragment = ""
	}
	return before + fragment + after, nil
}
