package microsoft

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
	"github.com/thommeo/winianatz"

	"calendar-mcp/internal/calendar"
)

func portableToGraphRecurrence(lines []string, start calendar.EventTime) (*graphRecurrence, error) {
	if len(lines) == 0 {
		return nil, nil
	}
	if len(lines) != 1 || !strings.HasPrefix(strings.ToUpper(lines[0]), "RRULE:") {
		return nil, fmt.Errorf("Microsoft recurrence supports one RRULE and cannot losslessly represent RDATE or EXDATE")
	}
	if err := calendar.ValidateRecurrence(lines); err != nil {
		return nil, err
	}
	option, err := rrule.StrToROption(strings.TrimPrefix(lines[0], "RRULE:"))
	if err != nil {
		return nil, err
	}
	if len(option.Bysetpos) == 1 && len(option.Byweekday) == 1 && option.Byweekday[0].N() == 0 && (option.Freq == rrule.MONTHLY || option.Freq == rrule.YEARLY) {
		option.Byweekday[0] = option.Byweekday[0].Nth(option.Bysetpos[0])
		option.Bysetpos = nil
	}
	if len(option.Bysetpos) > 0 || len(option.Byyearday) > 0 || len(option.Byweekno) > 0 || len(option.Byhour) > 0 || len(option.Byminute) > 0 || len(option.Bysecond) > 0 || len(option.Byeaster) > 0 {
		return nil, fmt.Errorf("Microsoft recurrence cannot losslessly represent this RRULE selector")
	}
	startInstant, err := start.Instant()
	if err != nil {
		return nil, err
	}
	zone := "UTC"
	if !start.IsAllDay() {
		zone = start.TimeZone
	}
	msZone := zone
	if zone != "UTC" {
		entry, mapErr := winianatz.FromIANA(zone)
		if mapErr != nil {
			return nil, fmt.Errorf("map recurrence time zone %q to Microsoft: %w", zone, mapErr)
		}
		msZone = entry.MicrosoftAlias
	}
	location, _ := time.LoadLocation(zone)
	localStart := startInstant.In(location)
	pattern := graphRecurrencePattern{Interval: option.Interval}
	if pattern.Interval <= 0 {
		pattern.Interval = 1
	}
	weekdayNames := []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}
	toDays := func(values []rrule.Weekday, requirePlain bool) ([]string, string, error) {
		days := make([]string, 0, len(values))
		index := ""
		indices := map[int]string{1: "first", 2: "second", 3: "third", 4: "fourth", -1: "last"}
		for _, value := range values {
			if value.Day() < 0 || value.Day() >= len(weekdayNames) {
				return nil, "", fmt.Errorf("invalid recurrence weekday")
			}
			if value.N() != 0 {
				if requirePlain || len(values) != 1 {
					return nil, "", fmt.Errorf("Microsoft recurrence cannot represent multiple ordinal weekdays")
				}
				var ok bool
				index, ok = indices[value.N()]
				if !ok {
					return nil, "", fmt.Errorf("Microsoft recurrence supports only first through fourth or last weekday")
				}
			}
			days = append(days, weekdayNames[value.Day()])
		}
		return days, index, nil
	}
	switch option.Freq {
	case rrule.DAILY:
		if len(option.Byweekday) > 0 || len(option.Bymonth) > 0 || len(option.Bymonthday) > 0 {
			return nil, fmt.Errorf("Microsoft daily recurrence cannot represent RRULE selectors")
		}
		pattern.Type = "daily"
	case rrule.WEEKLY:
		if len(option.Bymonth) > 0 || len(option.Bymonthday) > 0 {
			return nil, fmt.Errorf("Microsoft weekly recurrence cannot represent month selectors")
		}
		days := option.Byweekday
		if len(days) == 0 {
			days = []rrule.Weekday{{}}
			days[0] = []rrule.Weekday{rrule.MO, rrule.TU, rrule.WE, rrule.TH, rrule.FR, rrule.SA, rrule.SU}[(int(localStart.Weekday())+6)%7]
		}
		mapped, _, err := toDays(days, true)
		if err != nil {
			return nil, err
		}
		pattern.Type, pattern.DaysOfWeek = "weekly", mapped
		pattern.FirstDayOfWeek = weekdayNames[option.Wkst.Day()]
	case rrule.MONTHLY:
		if len(option.Bymonth) > 0 {
			return nil, fmt.Errorf("Microsoft monthly recurrence cannot represent BYMONTH")
		}
		if len(option.Byweekday) > 0 {
			mapped, index, err := toDays(option.Byweekday, false)
			if err != nil {
				return nil, err
			}
			if index == "" {
				return nil, fmt.Errorf("Microsoft monthly recurrence requires one ordinal BYDAY")
			}
			pattern.Type, pattern.DaysOfWeek, pattern.Index = "relativeMonthly", mapped, index
		} else {
			day := localStart.Day()
			if len(option.Bymonthday) == 1 {
				day = option.Bymonthday[0]
			} else if len(option.Bymonthday) > 1 {
				return nil, fmt.Errorf("Microsoft monthly recurrence supports one positive BYMONTHDAY")
			}
			if day < 1 || day > 31 {
				return nil, fmt.Errorf("Microsoft monthly recurrence supports one positive BYMONTHDAY")
			}
			pattern.Type, pattern.DayOfMonth = "absoluteMonthly", day
		}
	case rrule.YEARLY:
		month, day := int(localStart.Month()), localStart.Day()
		if len(option.Bymonth) == 1 {
			month = option.Bymonth[0]
		} else if len(option.Bymonth) > 1 {
			return nil, fmt.Errorf("Microsoft yearly recurrence supports one BYMONTH")
		}
		pattern.Month = month
		if len(option.Byweekday) > 0 {
			mapped, index, err := toDays(option.Byweekday, false)
			if err != nil {
				return nil, err
			}
			if index == "" {
				return nil, fmt.Errorf("Microsoft yearly recurrence requires one ordinal BYDAY")
			}
			pattern.Type, pattern.DaysOfWeek, pattern.Index = "relativeYearly", mapped, index
		} else {
			if len(option.Bymonthday) == 1 {
				day = option.Bymonthday[0]
			} else if len(option.Bymonthday) > 1 {
				return nil, fmt.Errorf("Microsoft yearly recurrence supports one BYMONTHDAY")
			}
			if day < 1 || day > 31 {
				return nil, fmt.Errorf("Microsoft yearly recurrence supports one positive BYMONTHDAY")
			}
			pattern.Type, pattern.DayOfMonth = "absoluteYearly", day
		}
	default:
		return nil, fmt.Errorf("Microsoft recurrence supports daily, weekly, monthly, and yearly frequencies")
	}
	rangeValue := graphRecurrenceRange{Type: "noEnd", StartDate: localStart.Format(calendar.DateLayout), RecurrenceTimeZone: msZone}
	if option.Count > 0 {
		rangeValue.Type, rangeValue.NumberOfOccurrences = "numbered", option.Count
	} else if !option.Until.IsZero() {
		rangeValue.Type, rangeValue.EndDate = "endDate", option.Until.In(location).Format(calendar.DateLayout)
	}
	return &graphRecurrence{Pattern: pattern, Range: rangeValue}, nil
}

func graphRecurrenceLines(value *graphRecurrence, allDay bool) ([]string, string, error) {
	if value == nil {
		return nil, "", nil
	}
	zone, err := graphIANAZone(value.Range.RecurrenceTimeZone)
	if err != nil {
		return nil, "", err
	}
	p := value.Pattern
	interval := p.Interval
	if interval <= 0 {
		interval = 1
	}
	parts := []string{}
	switch p.Type {
	case "daily":
		parts = append(parts, "FREQ=DAILY")
	case "weekly":
		parts = append(parts, "FREQ=WEEKLY")
		days, err := graphDays(p.DaysOfWeek, "")
		if err != nil {
			return nil, "", err
		}
		parts = append(parts, "BYDAY="+strings.Join(days, ","))
		if p.FirstDayOfWeek != "" {
			day, err := graphDay(p.FirstDayOfWeek)
			if err != nil {
				return nil, "", err
			}
			parts = append(parts, "WKST="+day)
		}
	case "absoluteMonthly":
		parts = append(parts, "FREQ=MONTHLY", "BYMONTHDAY="+strconv.Itoa(p.DayOfMonth))
	case "relativeMonthly":
		parts = append(parts, "FREQ=MONTHLY")
		days, err := graphDays(p.DaysOfWeek, p.Index)
		if err != nil {
			return nil, "", err
		}
		parts = append(parts, "BYDAY="+strings.Join(days, ","))
	case "absoluteYearly":
		parts = append(parts, "FREQ=YEARLY", "BYMONTH="+strconv.Itoa(p.Month), "BYMONTHDAY="+strconv.Itoa(p.DayOfMonth))
	case "relativeYearly":
		parts = append(parts, "FREQ=YEARLY", "BYMONTH="+strconv.Itoa(p.Month))
		days, err := graphDays(p.DaysOfWeek, p.Index)
		if err != nil {
			return nil, "", err
		}
		parts = append(parts, "BYDAY="+strings.Join(days, ","))
	default:
		return nil, "", fmt.Errorf("unsupported Microsoft recurrence pattern %q", p.Type)
	}
	if interval != 1 {
		parts = append(parts, "INTERVAL="+strconv.Itoa(interval))
	}
	switch value.Range.Type {
	case "noEnd", "":
	case "numbered":
		if value.Range.NumberOfOccurrences <= 0 {
			return nil, "", fmt.Errorf("invalid Microsoft recurrence count")
		}
		parts = append(parts, "COUNT="+strconv.Itoa(value.Range.NumberOfOccurrences))
	case "endDate":
		date, err := time.Parse(calendar.DateLayout, value.Range.EndDate)
		if err != nil {
			return nil, "", fmt.Errorf("invalid Microsoft recurrence end date: %w", err)
		}
		if allDay {
			parts = append(parts, "UNTIL="+date.Format("20060102"))
		} else {
			location, err := time.LoadLocation(zone)
			if err != nil {
				return nil, "", err
			}
			until := time.Date(date.Year(), date.Month(), date.Day(), 23, 59, 59, 0, location).UTC()
			parts = append(parts, "UNTIL="+until.Format("20060102T150405Z"))
		}
	default:
		return nil, "", fmt.Errorf("unsupported Microsoft recurrence range %q", value.Range.Type)
	}
	line := "RRULE:" + strings.Join(parts, ";")
	if err := calendar.ValidateRecurrence([]string{line}); err != nil {
		return nil, "", err
	}
	return []string{line}, zone, nil
}

func graphIANAZone(value string) (string, error) {
	if value == "" || value == "UTC" {
		return "UTC", nil
	}
	if _, err := time.LoadLocation(value); err == nil {
		return value, nil
	}
	entry, err := winianatz.FromMicrosoftAlias(value)
	if err != nil {
		return "", fmt.Errorf("map Microsoft recurrence time zone %q: %w", value, err)
	}
	if _, err := time.LoadLocation(entry.IANA); err != nil {
		return "", fmt.Errorf("load mapped recurrence time zone %q: %w", entry.IANA, err)
	}
	return entry.IANA, nil
}

func graphDays(values []string, index string) ([]string, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("Microsoft recurrence daysOfWeek is empty")
	}
	if index != "" && len(values) != 1 {
		return nil, fmt.Errorf("relative Microsoft recurrence with multiple days is ambiguous")
	}
	prefix := ""
	if index != "" {
		indices := map[string]string{"first": "1", "second": "2", "third": "3", "fourth": "4", "last": "-1"}
		var ok bool
		prefix, ok = indices[index]
		if !ok {
			return nil, fmt.Errorf("unsupported Microsoft recurrence index %q", index)
		}
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		day, err := graphDay(value)
		if err != nil {
			return nil, err
		}
		result = append(result, prefix+day)
	}
	return result, nil
}
func graphDay(value string) (string, error) {
	days := map[string]string{"monday": "MO", "tuesday": "TU", "wednesday": "WE", "thursday": "TH", "friday": "FR", "saturday": "SA", "sunday": "SU"}
	day, ok := days[value]
	if !ok {
		return "", fmt.Errorf("unsupported Microsoft recurrence weekday %q", value)
	}
	return day, nil
}
