package calendar

import "testing"

func TestValidateRecurrenceValidatesDateValuesAndParameters(t *testing.T) {
	valid := []string{"RRULE:FREQ=DAILY;COUNT=3", "EXDATE;TZID=Europe/Belgrade:20260811T090000", "RDATE;VALUE=DATE:20260812"}
	if err := ValidateRecurrence(valid); err != nil {
		t.Fatalf("ValidateRecurrence() error = %v", err)
	}
	for _, line := range []string{"EXDATE:tomorrow", "RDATE;VALUE=DATE:20260230", "EXDATE;TZID=Local:20260811T090000"} {
		if err := ValidateRecurrence([]string{line}); err == nil {
			t.Fatalf("ValidateRecurrence(%q) returned nil", line)
		}
	}
}
