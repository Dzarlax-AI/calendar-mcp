package calendar

import (
	"fmt"
	"time"
)

const DateLayout = "2006-01-02"

type ParsedEventTime struct {
	Time   time.Time
	AllDay bool
}

func ParseEventTime(value string) (ParsedEventTime, error) {
	if t, err := time.Parse(DateLayout, value); err == nil {
		return ParsedEventTime{Time: t, AllDay: true}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return ParsedEventTime{}, err
	}
	return ParsedEventTime{Time: t}, nil
}

func ParseEventTimeRange(startValue, endValue string) (start time.Time, end time.Time, allDay bool, err error) {
	startParsed, err := ParseEventTime(startValue)
	if err != nil {
		return time.Time{}, time.Time{}, false, fmt.Errorf("invalid start: %w", err)
	}
	endParsed, err := ParseEventTime(endValue)
	if err != nil {
		return time.Time{}, time.Time{}, false, fmt.Errorf("invalid end: %w", err)
	}
	if startParsed.AllDay != endParsed.AllDay {
		return time.Time{}, time.Time{}, false, fmt.Errorf("start and end must both be date-only or both be RFC3339 datetimes")
	}
	return startParsed.Time, endParsed.Time, startParsed.AllDay, nil
}

func MergeOptionalAllDay(current *bool, parsed ParsedEventTime) (*bool, error) {
	if current != nil && *current != parsed.AllDay {
		return nil, fmt.Errorf("start and end must both be date-only or both be RFC3339 datetimes")
	}
	allDay := parsed.AllDay
	return &allDay, nil
}
