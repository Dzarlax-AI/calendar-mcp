package calendar

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// FormatCollectionOffset encodes a validated fixed CalDAV collection offset.
func FormatCollectionOffset(offset int) string {
	sign := '+'
	if offset < 0 {
		sign, offset = '-', -offset
	}
	return fmt.Sprintf("UTC%c%02d:%02d:%02d", sign, offset/3600, offset/60%60, offset%60)
}

// CollectionLocation resolves internal collection metadata. Only Apple
// accepts the fixed offset notation obtained from CalDAV discovery.
func CollectionLocation(provider, zone string) *time.Location {
	if zone == "" || zone == "Local" {
		return nil
	}
	if strings.HasPrefix(zone, "UTC+") || strings.HasPrefix(zone, "UTC-") {
		if provider != "apple" || len(zone) != 12 || zone[6] != ':' || zone[9] != ':' {
			return nil
		}
		offset, ok := ParseICalendarOffset(zone[3:6] + zone[7:9] + zone[10:12])
		if !ok || offset == 0 {
			return nil
		}
		return time.FixedZone(zone, offset)
	}
	loc, _ := time.LoadLocation(zone)
	return loc
}

// ParseICalendarOffset parses an RFC 5545 UTC-OFFSET, with optional seconds.
func ParseICalendarOffset(value string) (int, bool) {
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
