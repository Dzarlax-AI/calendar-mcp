package apple

import (
	"strings"
	"testing"
)

func TestCalendarTimezoneValidatesObservances(t *testing.T) {
	const standard = "BEGIN:STANDARD\nDTSTART:20261025T030000\nTZOFFSETFROM:+0200\nTZOFFSETTO:+0100\nRRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU\nEND:STANDARD\n"
	const daylight = "BEGIN:DAYLIGHT\nDTSTART:20260329T020000\nTZOFFSETFROM:+0100\nTZOFFSETTO:+0200\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU\nEND:DAYLIGHT\n"
	const fixed = "BEGIN:STANDARD\nDTSTART:20260101T000000\nTZOFFSETFROM:+0200\nTZOFFSETTO:+0200\nEND:STANDARD\n"
	wrapZone := func(tzid, observances string) string {
		return "BEGIN:VTIMEZONE\nTZID:" + tzid + "\n" + observances + "END:VTIMEZONE\n"
	}
	named := wrapZone("Europe/Belgrade", standard+daylight)

	for _, tc := range []struct{ name, components, want string }{
		{"named zone", named, "Europe/Belgrade"},
		{"extension alongside timezone", named + "BEGIN:X-VENDOR\nX-INFO:value\nEND:X-VENDOR\n", "Europe/Belgrade"},
		{"duplicate timezone", named + named, ""},
		{"no observances", wrapZone("Europe/Belgrade", ""), ""},
		{"missing required offset", wrapZone("Europe/Belgrade", strings.ReplaceAll(standard, "TZOFFSETFROM:+0200\n", "")), ""},
		{"invalid offset", wrapZone("Europe/Belgrade", strings.ReplaceAll(standard, "+0200", "+0260")), ""},
		{"named zone with contradicting transition", wrapZone("America/New_York", standard+daylight), ""},
		{"named zone with contradicting recurrence", wrapZone("Europe/Belgrade", strings.ReplaceAll(standard, "BYDAY=-1SU", "BYDAY=4SU")+daylight), ""},
		{"extra IANA transitions between observances", wrapZone("Europe/Belgrade", strings.ReplaceAll(standard, "FREQ=YEARLY;", "FREQ=YEARLY;INTERVAL=2;")+strings.ReplaceAll(daylight, "FREQ=YEARLY;", "FREQ=YEARLY;INTERVAL=2;")), ""},
		{"named zone with fixed definition", wrapZone("America/New_York", fixed), "Etc/GMT-2"},
		{"fixed definition with unsupported fractional offset", wrapZone("America/New_York", strings.ReplaceAll(fixed, "+0200", "+0230")), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := "BEGIN:VCALENDAR\nPRODID:-//Synthetic//Timezone//EN\nVERSION:2.0\n" + tc.components + "END:VCALENDAR\n"
			if got := calendarTimezone(data); got != tc.want {
				t.Errorf("calendarTimezone = %q, want %q", got, tc.want)
			}
		})
	}
}
