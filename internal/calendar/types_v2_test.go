package calendar

import (
	"encoding/json"
	"testing"
)

func TestEventTimeValidate(t *testing.T) {
	tests := []struct {
		name    string
		value   EventTime
		wantErr bool
	}{
		{name: "all day", value: EventTime{Date: "2026-08-10"}},
		{name: "timed", value: EventTime{DateTime: "2026-08-10T09:00:00+02:00", TimeZone: "Europe/Belgrade"}},
		{name: "both forms", value: EventTime{Date: "2026-08-10", DateTime: "2026-08-10T09:00:00Z", TimeZone: "UTC"}, wantErr: true},
		{name: "missing form", value: EventTime{}, wantErr: true},
		{name: "date with timezone", value: EventTime{Date: "2026-08-10", TimeZone: "UTC"}, wantErr: true},
		{name: "datetime without timezone", value: EventTime{DateTime: "2026-08-10T09:00:00Z"}, wantErr: true},
		{name: "invalid timezone", value: EventTime{DateTime: "2026-08-10T09:00:00Z", TimeZone: "Mars/Olympus"}, wantErr: true},
		{name: "invalid date", value: EventTime{Date: "10-08-2026"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.value.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestEventTimeRangeValidation(t *testing.T) {
	start := EventTime{DateTime: "2026-08-10T10:00:00+02:00", TimeZone: "Europe/Belgrade"}
	end := EventTime{DateTime: "2026-08-10T09:00:00+02:00", TimeZone: "Europe/Belgrade"}

	if err := ValidateEventTimeRangeV2(start, end); err == nil {
		t.Fatal("ValidateEventTimeRangeV2() returned nil for reversed range")
	}
}

func TestPatchFieldDistinguishesPresenceAndClearing(t *testing.T) {
	var patch EventPatchV2
	if err := json.Unmarshal([]byte(`{"title":"","description":null,"attendees":[]}`), &patch); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if !patch.Title.Present || patch.Title.Null || patch.Title.Value != "" {
		t.Fatalf("title patch = %#v, want present empty string", patch.Title)
	}
	if !patch.Description.Present || !patch.Description.Null {
		t.Fatalf("description patch = %#v, want explicit null", patch.Description)
	}
	if !patch.Attendees.Present || patch.Attendees.Null || len(patch.Attendees.Value) != 0 {
		t.Fatalf("attendees patch = %#v, want present empty slice", patch.Attendees)
	}
	if patch.Location.Present {
		t.Fatalf("location patch = %#v, want omitted", patch.Location)
	}
}

func TestValidateRecurrence(t *testing.T) {
	if err := ValidateRecurrence([]string{"RRULE:FREQ=WEEKLY;BYDAY=MO,WE", "EXDATE:20260817T070000Z"}); err != nil {
		t.Fatalf("ValidateRecurrence() error = %v", err)
	}
	if err := ValidateRecurrence([]string{"RRULE:FREQ=NOPE"}); err == nil {
		t.Fatal("ValidateRecurrence() returned nil for invalid RRULE")
	}
	if err := ValidateRecurrence([]string{"UNKNOWN:value"}); err == nil {
		t.Fatal("ValidateRecurrence() returned nil for unsupported line")
	}
}
