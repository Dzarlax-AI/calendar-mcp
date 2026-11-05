package calendar

import "strings"

// SelfResponseStatus returns only a verified account response, never attendees.
func SelfResponseStatus(event EventV2, googlePrimary bool) string {
	if !googlePrimary || !strings.HasPrefix(event.CalendarID, "google:") || (event.Provider != "" && event.Provider != "google") {
		return ""
	}
	var status string
	count := 0
	for _, attendee := range event.Attendees {
		if attendee.Self {
			count++
			status = attendee.Status
		}
	}
	if count != 1 {
		return ""
	}
	switch status {
	case "accepted", "declined", "tentative", "needsAction":
		return status
	}
	return ""
}
