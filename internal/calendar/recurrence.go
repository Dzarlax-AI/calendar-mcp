package calendar

import (
	"fmt"
	"strings"

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
		default:
			return fmt.Errorf("unsupported recurrence property %q", property)
		}
	}
	return nil
}
