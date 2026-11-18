package apple

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFindCalendarsReadsOptionalTimezone(t *testing.T) {
	const zone = "Europe/Belgrade"
	ical := "BEGIN:VCALENDAR\r\nPRODID:-//Synthetic//Calendar Discovery//EN\r\nVERSION:2.0\r\nBEGIN:VTIMEZONE\r\nTZID:" + zone + "\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:20261025T030000\r\nTZOFFSETFROM:+0200\r\nTZOFFSETTO:+0100\r\nEND:STANDARD\r\n" +
		"END:VTIMEZONE\r\nEND:VCALENDAR\r\n"
	propstat := func(status, props string) string {
		return "<D:propstat><D:prop>" + props + "</D:prop><D:status>HTTP/1.1 " + status + "</D:status></D:propstat>"
	}
	entry := func(path, props string) string {
		return "<D:response><D:href>" + path + "</D:href>" + props + "</D:response>"
	}
	base := propstat("200 OK", "<D:resourcetype><C:calendar/></D:resourcetype><D:displayname>Synthetic</D:displayname>")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || r.Method != "PROPFIND" || r.Header.Get("Depth") != "1" || !strings.Contains(string(body), "<C:calendar-timezone/>") {
			t.Error("calendar enumeration must request calendar-timezone in the Depth:1 PROPFIND")
		}
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprint(w, `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`)
		fmt.Fprint(w, entry("/home/", propstat("200 OK", "<D:resourcetype><D:collection/></D:resourcetype>")))
		fmt.Fprint(w, entry("http://"+r.Host+"/home/has%20zone/", base+propstat("200 OK", "<C:calendar-timezone>"+ical+"</C:calendar-timezone>")))
		fmt.Fprint(w, entry("/home/missing/", base+propstat("404 Not Found", "<C:calendar-timezone/>")))
		fmt.Fprint(w, entry("/home/forbidden/", base+propstat("403 Forbidden", "<C:calendar-timezone/>")))
		fmt.Fprint(w, entry("/home/invalid/", base+propstat("200 OK", "<C:calendar-timezone>not an iCalendar value</C:calendar-timezone>")))
		fmt.Fprint(w, entry("/home/omitted/", base))
		fmt.Fprint(w, entry("/home/ambiguous/", base+propstat("200 OK", "<C:calendar-timezone>invalid</C:calendar-timezone><C:calendar-timezone>"+ical+"</C:calendar-timezone>")))
		fmt.Fprint(w, "</D:multistatus>")
	}))
	t.Cleanup(server.Close)

	p, err := New("synthetic", "synthetic", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	calendars, _, err := p.findCalendars(t.Context(), "/home/")
	if err != nil || len(calendars) != 6 {
		t.Fatalf("findCalendars = %v, %v", calendars, err)
	}
	if calendars[0].ID != "/home/has zone/" || calendars[0].Timezone != zone || calendars[0].Name != "Synthetic" {
		t.Fatalf("calendar with timezone = %+v", calendars[0])
	}
	for _, cal := range calendars[1:] {
		if cal.Timezone != "" {
			t.Errorf("expected missing timezone for %s, got %q", cal.ID, cal.Timezone)
		}
	}
}
