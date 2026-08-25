package apple

import (
	"fmt"
	"strconv"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"
)

const appleUnsupportedCustomTimezoneReason = "unsupported_custom_timezone"

// normalizeAppleFixedOffsetTimezone replaces an Apple-defined fixed-offset
// TZID with an equivalent IANA Etc/GMT name in one decoded resource. go-ical
// otherwise attempts time.LoadLocation on the custom TZID and ignores its
// VTIMEZONE definition. Only whole-hour definitions with one observance and a
// constant offset are accepted. Apple may add an RDATE to that observance; it
// is safe because it cannot change the offset. Guessing across daylight
// transitions would change event instants.
func normalizeAppleFixedOffsetTimezone(object *caldav.CalendarObject) error {
	if object == nil || object.Data == nil {
		return fmt.Errorf("%s: missing calendar data", appleUnsupportedCustomTimezoneReason)
	}
	for _, event := range object.Data.Events() {
		for _, prop := range event.Props {
			for _, value := range prop {
				tzid := value.Params.Get(ical.PropTimezoneID)
				if tzid == "" {
					continue
				}
				if _, err := appleFixedOffsetTimezoneName(object.Data, tzid); err != nil {
					return err
				}
			}
		}
	}

	for _, event := range object.Data.Events() {
		for _, prop := range event.Props {
			for _, value := range prop {
				tzid := value.Params.Get(ical.PropTimezoneID)
				if tzid == "" {
					continue
				}
				name, err := appleFixedOffsetTimezoneName(object.Data, tzid)
				if err != nil {
					return err
				}
				value.Params.Set(ical.PropTimezoneID, name)
			}
		}
	}
	return nil
}

func appleFixedOffsetTimezoneName(container *ical.Calendar, tzid string) (string, error) {
	if _, err := time.LoadLocation(tzid); err == nil {
		return tzid, nil
	}

	var matches []*ical.Component
	for _, child := range container.Children {
		if child.Name != ical.CompTimezone {
			continue
		}
		value, _ := child.Props.Text(ical.PropTimezoneID)
		if value == tzid {
			matches = append(matches, child)
		}
	}
	if len(matches) != 1 || len(matches[0].Children) != 1 {
		return "", fmt.Errorf("%s: %q is not one unambiguous fixed definition", appleUnsupportedCustomTimezoneReason, tzid)
	}
	standard := matches[0].Children[0]
	if standard.Name != ical.CompTimezoneStandard && standard.Name != ical.CompTimezoneDaylight || len(standard.Children) != 0 || len(standard.Props[ical.PropTimezoneOffsetFrom]) != 1 || len(standard.Props[ical.PropTimezoneOffsetTo]) != 1 || len(standard.Props["RRULE"]) != 0 || len(standard.Props["RDATE"]) > 1 {
		return "", fmt.Errorf("%s: %q has transitions", appleUnsupportedCustomTimezoneReason, tzid)
	}
	from := standard.Props.Get(ical.PropTimezoneOffsetFrom)
	to := standard.Props.Get(ical.PropTimezoneOffsetTo)
	if from == nil || to == nil || from.Value != to.Value {
		return "", fmt.Errorf("%s: %q is not fixed", appleUnsupportedCustomTimezoneReason, tzid)
	}
	offset, err := appleTimezoneWholeHourOffset(to.Value)
	if err != nil {
		return "", fmt.Errorf("%s: %q: %w", appleUnsupportedCustomTimezoneReason, tzid, err)
	}
	if offset == 0 {
		return "UTC", nil
	}
	// IANA's Etc/GMT signs are deliberately reversed: Etc/GMT-1 is UTC+01.
	name := "Etc/GMT" + strconv.Itoa(-offset)
	if _, err := time.LoadLocation(name); err != nil {
		return "", fmt.Errorf("%s: fixed offset %q is unavailable", appleUnsupportedCustomTimezoneReason, to.Value)
	}
	return name, nil
}

func appleTimezoneWholeHourOffset(value string) (int, error) {
	if len(value) != 5 || (value[0] != '+' && value[0] != '-') {
		return 0, fmt.Errorf("invalid UTC offset %q", value)
	}
	hours, errH := strconv.Atoi(value[1:3])
	minutes, errM := strconv.Atoi(value[3:5])
	if errH != nil || errM != nil || hours > 23 || minutes != 0 {
		return 0, fmt.Errorf("invalid fixed-hour UTC offset %q", value)
	}
	if value[0] == '-' {
		return -hours, nil
	}
	return hours, nil
}
