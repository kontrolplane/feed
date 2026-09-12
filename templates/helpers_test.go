package templates

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Finding 13: Truncate and TruncateURL sliced by byte offset, which splits a
// multi-byte rune and emits invalid UTF-8. Every case below asserts
// utf8.ValidString on the result, because that - not the exact cut point - is
// the property that was broken.

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"empty", "", 5, ""},
		{"ascii shorter than max", "hello", 10, "hello"},
		{"ascii exactly max", "hello", 5, "hello"},
		{"ascii one over max", "hello!", 5, "hello…"},
		{"ascii truncated", "the quick brown fox", 9, "the quick…"},

		// Japanese: 3 bytes per rune. The old code cut "日本語のフィード" at
		// byte 5, landing inside the second rune.
		{"japanese under max", "日本語", 5, "日本語"},
		{"japanese exactly max", "日本語のフィード", 8, "日本語のフィード"},
		{"japanese truncated", "日本語のフィード", 3, "日本語…"},
		{"japanese truncated at one", "日本語", 1, "日…"},

		// Emoji: 4 bytes per rune.
		{"emoji under max", "🚀🌍", 5, "🚀🌍"},
		{"emoji truncated", "🚀🌍🔥🎉", 2, "🚀🌍…"},

		// Accented Latin: 2 bytes per rune.
		{"accented under max", "café", 10, "café"},
		{"accented exactly max", "café", 4, "café"},
		{"accented truncated", "crème brûlée", 5, "crème…"},

		// Mixed widths in one string.
		{"mixed truncated", "a£本🚀b", 4, "a£本🚀…"},

		{"max beyond length", "short", 1000, "short"},
		{"max zero", "anything", 0, ""},
		{"max zero empty input", "", 0, ""},
		{"max negative", "anything", -3, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Truncate(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("Truncate(%q, %d) produced invalid UTF-8: %q", tc.in, tc.max, got)
			}
		})
	}
}

func TestTruncateKeepsAtMostMaxRunes(t *testing.T) {
	inputs := []string{
		"plain ascii string",
		"日本語のフィードリーダー",
		"🚀🌍🔥🎉✨🎊",
		"crème brûlée à la mode",
		"mixed a£本🚀 content",
	}

	for _, in := range inputs {
		for n := -1; n <= utf8.RuneCountInString(in)+2; n++ {
			got := Truncate(in, n)
			if !utf8.ValidString(got) {
				t.Fatalf("Truncate(%q, %d) produced invalid UTF-8: %q", in, n, got)
			}
			kept := strings.TrimSuffix(got, "…")
			if n > 0 && utf8.RuneCountInString(kept) > n {
				t.Fatalf("Truncate(%q, %d) kept %d runes, want at most %d",
					in, n, utf8.RuneCountInString(kept), n)
			}
		}
	}
}

func TestTruncateURL(t *testing.T) {
	const long = "example.com/a-very-long-path-segment-that-keeps-going-and-going"

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"strips https", "https://example.com/feed.xml", "example.com/feed.xml"},
		{"strips http", "http://example.com/feed.xml", "example.com/feed.xml"},
		{"no scheme is left alone", "example.com/feed.xml", "example.com/feed.xml"},
		{"other scheme untouched", "ftp://example.com", "ftp://example.com"},

		// Exactly 40 runes after the scheme is stripped: no ellipsis.
		{"exactly forty", "https://" + strings.Repeat("a", 40), strings.Repeat("a", 40)},
		{"forty one", "https://" + strings.Repeat("a", 41), strings.Repeat("a", 40) + "…"},

		{"long ascii truncated", "https://" + long, long[:40] + "…"},

		// The scheme is stripped before counting, so the 40 runes are 40 runes
		// of host+path, not 40 bytes of "https://…".
		{
			"japanese idn truncated",
			"https://" + strings.Repeat("日", 45),
			strings.Repeat("日", 40) + "…",
		},
		{
			"japanese idn under limit",
			"https://" + strings.Repeat("日", 10),
			strings.Repeat("日", 10),
		},
		{
			"emoji path truncated",
			"https://example.com/" + strings.Repeat("🚀", 30),
			"example.com/" + strings.Repeat("🚀", 28) + "…",
		},
		{
			"accented host",
			"https://café-münchen.example/artículos/año-2024/resumen-completo",
			"café-münchen.example/artículos/año-2024/…",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateURL(tc.in)
			if got != tc.want {
				t.Errorf("TruncateURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("TruncateURL(%q) produced invalid UTF-8: %q", tc.in, got)
			}
			if kept := strings.TrimSuffix(got, "…"); utf8.RuneCountInString(kept) > 40 {
				t.Errorf("TruncateURL(%q) kept %d runes, want at most 40",
					tc.in, utf8.RuneCountInString(kept))
			}
		})
	}
}

func TestFormatDate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Unparseable input is returned verbatim; this is the path that must
		// not byte-slice a non-ASCII string.
		{"garbage passthrough", "not-a-date", "not-a-date"},
		{"unicode passthrough", "日本語", "日本語"},
		{"empty passthrough", "", ""},
		{"old date", "2020-03-17", "03-17"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatDate(tc.in)
			if got != tc.want {
				t.Errorf("FormatDate(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("FormatDate(%q) produced invalid UTF-8: %q", tc.in, got)
			}
		})
	}
}

func TestPad2(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		{0, "00"},
		{7, "07"},
		{10, "10"},
		{99, "99"},
		{100, "100"},
	}

	for _, tc := range tests {
		if got := Pad2(tc.in); got != tc.want {
			t.Errorf("Pad2(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
