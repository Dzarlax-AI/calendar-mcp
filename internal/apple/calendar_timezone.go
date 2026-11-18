package apple

import (
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

func calendarTimezone(value string) string {
	data, err := ical.NewDecoder(strings.NewReader(strings.TrimSpace(value))).Decode()
	if err != nil {
		return ""
	}

	var component *ical.Component
	for _, child := range data.Children {
		if child.Name == ical.CompTimezone {
			if component != nil {
				return ""
			}
			component = child
		}
	}
	if component == nil || len(component.Props[ical.PropTimezoneID]) != 1 {
		return ""
	}
	tzid, err := component.Props.Text(ical.PropTimezoneID)
	if err != nil || tzid == "" || tzid == "Local" {
		return ""
	}

	location, _ := time.LoadLocation(tzid)
	namedMatches := location != nil
	var firstTransition, lastTransition time.Time
	transitions := make(map[time.Time]struct{})
	var firstOffset int
	fixed, hasObservance := true, false
	for _, child := range component.Children {
		if child.Name != ical.CompTimezoneStandard && child.Name != ical.CompTimezoneDaylight {
			continue
		}
		dates, from, to, valid := calendarTimezoneObservance(child.Props)
		if !valid {
			return ""
		}
		if !hasObservance {
			firstOffset = from
			hasObservance = true
		}
		fixed = fixed && from == firstOffset && to == firstOffset
		if namedMatches {
			for _, date := range dates {
				at := date.Add(-time.Duration(from) * time.Second)
				_, before := at.Add(-time.Second).In(location).Zone()
				_, after := at.In(location).Zone()
				if from != before || to != after {
					namedMatches = false
					break
				}
				if from != to {
					transitions[at] = struct{}{}
					if firstTransition.IsZero() || at.Before(firstTransition) {
						firstTransition = at
					}
					if at.After(lastTransition) {
						lastTransition = at
					}
				}
			}
		}
	}
	if !hasObservance {
		return ""
	}
	if fixed {
		// A named TZID can label a fixed definition even if the IANA zone
		// observes DST. Preserve the definition's offset in that case.
		if firstOffset == 0 {
			return "UTC"
		}
		if firstOffset%3600 == 0 {
			name := "Etc/GMT" + strconv.Itoa(-firstOffset/3600)
			if _, err := time.LoadLocation(name); err == nil {
				return name
			}
		}
		return ""
	}
	if namedMatches && calendarTimezoneTransitionsMatch(location, firstTransition, lastTransition, transitions) {
		return tzid
	}
	return ""
}

// Explicit dates and sampled annual recurrence dates need to agree in both
// directions: the IANA zone must not add offset changes between observations.
func calendarTimezoneTransitionsMatch(location *time.Location, first, last time.Time, observed map[time.Time]struct{}) bool {
	for at := first; at.Before(last); {
		_, offset := at.In(location).Zone()
		_, next := at.In(location).ZoneBounds()
		if next.IsZero() || next.After(last) {
			break
		}
		if !next.After(at) {
			return false
		}
		_, nextOffset := next.In(location).Zone()
		if nextOffset != offset {
			if _, ok := observed[next.UTC()]; !ok {
				return false
			}
		}
		at = next
	}
	return true
}

func calendarTimezoneObservance(props ical.Props) (dates []time.Time, from, to int, valid bool) {
	// Observance starts and recurrence dates are local date-times. Treat
	// them as civil values and apply TZOFFSETFROM when checking the instant.
	if len(props[ical.PropDateTimeStart]) != 1 || len(props[ical.PropTimezoneOffsetFrom]) != 1 ||
		len(props[ical.PropTimezoneOffsetTo]) != 1 || len(props[ical.PropRecurrenceRule]) > 1 {
		return nil, 0, 0, false
	}
	const layout = "20060102T150405"
	start, err := time.Parse(layout, props.Get(ical.PropDateTimeStart).Value)
	from, fromOK := calendarUTCOffset(props.Get(ical.PropTimezoneOffsetFrom).Value)
	to, toOK := calendarUTCOffset(props.Get(ical.PropTimezoneOffsetTo).Value)
	if err != nil || !fromOK || !toOK {
		return nil, 0, 0, false
	}
	dates = []time.Time{start}
	for _, prop := range props[ical.PropRecurrenceDates] {
		for _, value := range strings.Split(prop.Value, ",") {
			date, err := time.Parse(layout, value)
			if err != nil {
				return nil, 0, 0, false
			}
			dates = append(dates, date)
		}
	}
	rule, err := props.RecurrenceRule()
	if err != nil {
		return nil, 0, 0, false
	}
	if rule != nil && from != to {
		// Check annual DST rules beyond DTSTART as well. The bounded sample
		// catches contradicting recurrences without expanding arbitrary rules.
		if rule.Freq != rrule.YEARLY {
			return nil, 0, 0, false
		}
		rule.Dtstart = start
		if !rule.Until.IsZero() {
			rule.Until = rule.Until.Add(time.Duration(from) * time.Second)
		}
		recur, err := rrule.NewRRule(*rule)
		if err != nil {
			return nil, 0, 0, false
		}
		next := recur.Iterator()
		for range 8 {
			date, ok := next()
			if !ok {
				break
			}
			dates = append(dates, date)
		}
	}
	return dates, from, to, true
}

func calendarUTCOffset(value string) (int, bool) {
	if (len(value) != 5 && len(value) != 7) || (value[0] != '+' && value[0] != '-') {
		return 0, false
	}
	for _, digit := range value[1:] {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	hours, _ := strconv.Atoi(value[1:3])
	minutes, _ := strconv.Atoi(value[3:5])
	seconds, _ := strconv.Atoi(value[5:])
	if hours > 23 || minutes > 59 || seconds > 59 {
		return 0, false
	}
	offset := hours*3600 + minutes*60 + seconds
	if value[0] == '-' {
		return -offset, offset != 0
	}
	return offset, true
}
