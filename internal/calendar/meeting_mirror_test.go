package calendar

import (
	"html"
	"strings"
	"testing"
)

func TestMirrorMeetingURLsAndMerge(t *testing.T) {
	for _, raw := range []string{"https://meet.google.com/abc-defg-hij?authuser=1&x=2", "https://teams.microsoft.com/l/meetup-join/a?context=a%2Fb", "https://us02web.zoom.us/j/123?pwd=a%2Bb"} {
		t.Run(raw, func(t *testing.T) {
			link := MirrorMeetingURL(&ConferenceData{EntryPoints: []ConferenceEntryPoint{{Type: "video", URI: raw}}})
			if link != raw {
				t.Fatal("video link lost or rejected")
			}
			first, err := MergeMirrorMeeting("manual <b>text</b>", link)
			if err != nil || !strings.Contains(html.UnescapeString(first), raw) {
				t.Fatal("create failed")
			}
			second, err := MergeMirrorMeeting(first, link)
			if err != nil || second != first {
				t.Fatal("merge not idempotent")
			}
			removed, err := MergeMirrorMeeting(first, "")
			if err != nil || removed != "manual <b>text</b>" {
				t.Fatal("removal changed other text")
			}
		})
	}
	for _, raw := range []string{"http://meet.google.com/a", "https://meet.google.com.evil.invalid/a", "https://x@meet.google.com/a", "https://zoom.us:444/a"} {
		if MirrorMeetingURL(&ConferenceData{EntryPoints: []ConferenceEntryPoint{{Type: "video", URI: raw}}}) != "" {
			t.Fatal("accepted invalid URL")
		}
	}
	if _, err := MergeMirrorMeeting(meetingStart+"x", ""); err == nil || strings.Contains(err.Error(), "x") {
		t.Fatal("ambiguous marker must fail safely")
	}
}

func TestMeetingURLDedupRequiresExactURL(t *testing.T) {
	link := "https://meet.google.com/abc?x=1"
	for _, tc := range []struct {
		text string
		want bool
	}{
		{`Join <a href="https://meet.google.com/abc?x=1">meeting</a>`, true},
		{`https://meet.google.com/abc?x=1&amp;other=2`, false},
		{`https://meet.google.com/abc?x=10`, false},
	} {
		if got := ContainsMirrorMeetingURL(tc.text, link); got != tc.want {
			t.Errorf("exact match = %v, want %v", got, tc.want)
		}
	}
	merged, err := MergeMirrorMeeting(`https://meet.google.com/abc?x=10`, link)
	if err != nil || !HasMirrorMeetingMarker(merged) {
		t.Fatal("URL prefix incorrectly suppressed managed link")
	}
}
