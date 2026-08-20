package apple

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"
	"github.com/teambition/rrule-go"

	"calendar-mcp/internal/calendar"
)

const appleInstanceMarker = "#calendar-mcp-instance="

func appleInstanceID(path string, original calendar.EventTime) string {
	data, _ := json.Marshal(original)
	return path + appleInstanceMarker + base64.RawURLEncoding.EncodeToString(data)
}

func splitAppleInstanceID(value string) (string, *calendar.EventTime, error) {
	path, token, found := strings.Cut(value, appleInstanceMarker)
	if !found {
		return value, nil, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", nil, fmt.Errorf("invalid Apple recurrence instance id")
	}
	var original calendar.EventTime
	if err := json.Unmarshal(data, &original); err != nil || original.Validate() != nil {
		return "", nil, fmt.Errorf("invalid Apple recurrence instance id")
	}
	return path, &original, nil
}

func appleEventsFromObject(object caldav.CalendarObject, request calendar.ListEventsRequestV2) ([]calendar.EventV2, error) {
	events := object.Data.Events()
	var master *ical.Event
	for i := range events {
		if events[i].Props.Get(ical.PropRecurrenceID) == nil {
			copy := events[i]
			master = &copy
			break
		}
	}
	if master == nil {
		return nil, nil
	}
	base := appleEventV2(*master, request.CalendarID, object.Path, object.ETag)
	if len(base.Recurrence) == 0 {
		start, startErr := base.Start.Instant()
		end, endErr := base.End.Instant()
		if startErr != nil || endErr != nil || !end.After(request.Start) || !start.Before(request.End) {
			return nil, nil
		}
		return []calendar.EventV2{base}, nil
	}
	base.InstanceKind = "seriesMaster"
	result := []calendar.EventV2{}
	if request.View != calendar.RecurrenceExpanded {
		result = append(result, base)
	}
	if request.View == calendar.RecurrenceSeries {
		return result, nil
	}

	start, err := master.DateTimeStart(nil)
	if err != nil {
		return nil, err
	}
	end, err := master.DateTimeEnd(nil)
	if err != nil {
		return nil, err
	}
	set := &rrule.Set{}
	set.DTStart(start)
	for _, prop := range master.Props.Values(ical.PropRecurrenceRule) {
		option, err := rrule.StrToROptionInLocation(prop.Value, start.Location())
		if err != nil {
			return nil, fmt.Errorf("parse Apple RRULE: %w", err)
		}
		option.Dtstart = start
		rule, err := rrule.NewRRule(*option)
		if err != nil {
			return nil, err
		}
		set.RRule(rule)
	}
	for _, prop := range master.Props.Values(ical.PropRecurrenceDates) {
		values, err := appleRecurrenceDates(prop, start.Location())
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			set.RDate(value)
		}
	}
	excluded := map[string]calendar.EventTime{}
	for _, prop := range master.Props.Values(ical.PropExceptionDates) {
		values, err := appleRecurrenceDates(prop, start.Location())
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			set.ExDate(value)
			original := appleTimeLikeMaster(value, base.Start)
			excluded[appleTimeKey(original)] = original
		}
	}
	exceptions := map[string]calendar.EventV2{}
	for _, event := range events {
		recurrenceID := event.Props.Get(ical.PropRecurrenceID)
		if recurrenceID == nil {
			continue
		}
		original := appleEventTimeV2(recurrenceID)
		item := appleEventV2(event, request.CalendarID, appleInstanceID(object.Path, original), object.ETag)
		item.RecurringEventID = object.Path
		item.OriginalStart = &original
		item.InstanceKind = "exception"
		if strings.EqualFold(item.Status, "CANCELLED") {
			item.Status, item.InstanceKind = "cancelled", "cancelled"
		}
		exceptions[appleTimeKey(original)] = item
	}
	duration := end.Sub(start)
	for _, occurrence := range set.Between(request.Start, request.End, true) {
		original := appleTimeLikeMaster(occurrence, base.Start)
		key := appleTimeKey(original)
		if exception, ok := exceptions[key]; ok {
			result = append(result, exception)
			delete(exceptions, key)
			continue
		}
		item := base
		item.ID = appleInstanceID(object.Path, original)
		item.RecurringEventID = object.Path
		item.OriginalStart = &original
		item.InstanceKind = "occurrence"
		item.Recurrence = nil
		item.Start = original
		item.End = appleTimeLikeMaster(occurrence.Add(duration), base.End)
		result = append(result, item)
	}
	for key, original := range excluded {
		if _, explicit := exceptions[key]; explicit {
			continue
		}
		instant, _ := original.Instant()
		if !instant.Before(request.Start) && instant.Before(request.End) {
			copy := original
			result = append(result, calendar.EventV2{ID: appleInstanceID(object.Path, original), CalendarID: request.CalendarID, RecurringEventID: object.Path, OriginalStart: &copy, InstanceKind: "cancelled", Status: "cancelled"})
		}
	}
	for _, exception := range exceptions {
		instant, err := exception.OriginalStart.Instant()
		if err == nil && !instant.Before(request.Start) && instant.Before(request.End) {
			result = append(result, exception)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, _ := result[i].Start.Instant()
		right, _ := result[j].Start.Instant()
		return left.Before(right)
	})
	return result, nil
}

func appleRecurrenceDates(prop ical.Prop, location *time.Location) ([]time.Time, error) {
	values := make([]time.Time, 0)
	for _, raw := range strings.Split(prop.Value, ",") {
		copy := prop
		copy.Value = raw
		value, err := copy.DateTime(location)
		if err != nil {
			return nil, fmt.Errorf("parse Apple recurrence date: %w", err)
		}
		values = append(values, value)
	}
	return values, nil
}

func appleTimeLikeMaster(value time.Time, model calendar.EventTime) calendar.EventTime {
	if model.IsAllDay() {
		return calendar.EventTime{Date: value.Format(calendar.DateLayout)}
	}
	location, err := time.LoadLocation(model.TimeZone)
	if err != nil {
		location = time.UTC
	}
	return calendar.EventTime{DateTime: value.In(location).Format(time.RFC3339), TimeZone: model.TimeZone}
}

func appleTimeKey(value calendar.EventTime) string {
	instant, err := value.Instant()
	if err != nil {
		return value.Date + "\x00" + value.DateTime
	}
	return instant.UTC().Format(time.RFC3339)
}

func cloneAppleEvent(source *ical.Event) *ical.Event {
	result := ical.NewEvent()
	for name, values := range source.Props {
		for _, value := range values {
			copy := value
			copy.Params = make(ical.Params, len(value.Params))
			for key, items := range value.Params {
				copy.Params[key] = append([]string(nil), items...)
			}
			result.Props.Add(&copy)
		}
		_ = name
	}
	return result
}

func sameAppleEventTime(left, right calendar.EventTime) bool {
	return appleTimeKey(left) == appleTimeKey(right)
}
