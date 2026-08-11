package calendar

import (
	"fmt"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

func ValidateRecurrence(lines []string) error {
	for _, line := range lines {
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("invalid recurrence line %q", line)
		}
		property := strings.SplitN(strings.ToUpper(name), ";", 2)[0]
		switch property {
		case "RRULE":
			if _, err := rrule.StrToROption(value); err != nil {
				return fmt.Errorf("invalid RRULE %q: %w", line, err)
			}
		case "RDATE", "EXDATE":
			if err := validateRecurrenceDates(name, value, property == "RDATE"); err != nil {
				return fmt.Errorf("invalid %s %q: %w", property, line, err)
			}
		default:
			return fmt.Errorf("unsupported recurrence property %q", property)
		}
	}
	return nil
}

func validateRecurrenceDates(name, value string, allowPeriod bool) error {
	params := make(map[string]string)
	parts := strings.Split(name, ";")
	for _, raw := range parts[1:] {
		key, val, ok := strings.Cut(raw, "=")
		if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(val) == "" {
			return fmt.Errorf("malformed parameter %q", raw)
		}
		params[strings.ToUpper(strings.TrimSpace(key))] = strings.TrimSpace(val)
	}
	valueType := strings.ToUpper(params["VALUE"])
	if valueType == "" {
		valueType = "DATE-TIME"
	}
	if valueType != "DATE" && valueType != "DATE-TIME" && !(allowPeriod && valueType == "PERIOD") {
		return fmt.Errorf("unsupported VALUE=%s", valueType)
	}
	if zone := params["TZID"]; zone != "" {
		if valueType == "DATE" {
			return fmt.Errorf("TZID is not valid with VALUE=DATE")
		}
		if zone == "Local" {
			return fmt.Errorf("TZID must be an explicit IANA name")
		}
		if _, err := time.LoadLocation(zone); err != nil {
			return fmt.Errorf("invalid TZID %q", zone)
		}
	}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return fmt.Errorf("empty date value")
		}
		if valueType == "PERIOD" {
			start, end, ok := strings.Cut(item, "/")
			if !ok || strings.HasPrefix(strings.ToUpper(end), "P") {
				return fmt.Errorf("period must use explicit start/end date-times")
			}
			if err := validateICalDateTime(start); err != nil {
				return err
			}
			if err := validateICalDateTime(end); err != nil {
				return err
			}
			continue
		}
		if valueType == "DATE" {
			if _, err := time.Parse("20060102", item); err != nil {
				return fmt.Errorf("invalid date %q", item)
			}
			continue
		}
		if err := validateICalDateTime(item); err != nil {
			return err
		}
	}
	return nil
}

func validateICalDateTime(value string) error {
	layout := "20060102T150405"
	if strings.HasSuffix(value, "Z") {
		layout += "Z"
	}
	if _, err := time.Parse(layout, value); err != nil {
		return fmt.Errorf("invalid date-time %q", value)
	}
	return nil
}
