package syncengine

import (
	"html"
	"sort"
	"strings"

	"calendar-mcp/internal/calendar"
)

// mirrorDescription renders provider description and available participants as
// HTML text. It only uses source data and is deterministic across retries.
// Participants are text, never calendar attendees or notification recipients.
func mirrorDescription(event calendar.EventV2) string {
	description := event.Description
	if !strings.EqualFold(event.DescriptionFormat, "html") {
		description = html.EscapeString(description)
		description = strings.ReplaceAll(strings.ReplaceAll(description, "\r\n", "\n"), "\n", "<br>")
	}
	people := make(map[string]string, len(event.Attendees)+1)
	addPerson := func(person calendar.PersonV2) {
		name := strings.Join(strings.Fields(person.Name), " ")
		email := strings.Join(strings.Fields(person.Email), " ")
		if name == "" && email == "" {
			return
		}
		key := strings.ToLower(email)
		if key == "" {
			key = "name:" + strings.ToLower(name)
		} else {
			key = "email:" + key
		}
		label := name
		if email != "" {
			if label != "" {
				label += " <" + email + ">"
			} else {
				label = email
			}
		}
		if previous, exists := people[key]; !exists || label < previous {
			people[key] = label
		}
	}
	if event.Organizer != nil {
		addPerson(*event.Organizer)
	}
	for _, attendee := range event.Attendees {
		addPerson(attendee.PersonV2)
	}
	if len(people) == 0 {
		return description
	}
	keys := make([]string, 0, len(people))
	for key := range people {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(description)
	if description != "" {
		b.WriteString("<br><br>")
	}
	if event.SourceAttendeesComplete {
		b.WriteString("<strong>Known participants</strong>")
	} else {
		b.WriteString("<strong>Known participants (list may be incomplete)</strong>")
	}
	for _, key := range keys {
		b.WriteString("<br>• ")
		b.WriteString(html.EscapeString(people[key]))
	}
	return b.String()
}
