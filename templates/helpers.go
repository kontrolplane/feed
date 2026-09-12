package templates

import (
	"fmt"
	"math"
	"strings"
	"time"
)

func FormatDate(iso string) string {
	d, err := time.Parse("2006-01-02", iso)
	if err != nil {
		return iso
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	diff := int(math.Floor(today.Sub(d).Hours() / 24))
	if diff <= 0 {
		return "today"
	}
	if diff == 1 {
		return "1d"
	}
	if diff < 7 {
		return fmt.Sprintf("%dd", diff)
	}
	// Safe to slice by byte: time.Parse succeeded, so iso is an ASCII
	// "YYYY-MM-DD" and offsets 5..10 land on rune boundaries.
	return iso[5:10]
}

func Pad2(n int) string {
	return fmt.Sprintf("%02d", n)
}

// truncateRunes cuts s to at most n runes, appending an ellipsis when it
// actually truncated.
//
// This used to slice by byte (`s[:max]`), which cuts multi-byte runes in half
// and emits invalid UTF-8 — any Japanese, emoji or accented-Latin feed title
// rendered as mojibake, and the same class of bug on the storage path made
// Postgres reject the insert outright. Converting to []rune first makes the
// cut land on a rune boundary by construction.
//
// Implemented here rather than calling internal/sanitize.TruncateRunes on
// purpose: templates is the presentation layer and should not grow a
// dependency on an internal service package for four lines of string work.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	// Fast path: a string of n bytes or fewer can never hold more than n
	// runes, so the common ASCII case skips the allocation entirely.
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Truncate shortens s to at most max runes. Note that max counts runes, not
// bytes, so a Japanese title is cut after max characters rather than after
// max/3 of them.
func Truncate(s string, max int) string {
	return truncateRunes(s, max)
}

// TruncateURL strips the scheme and shortens a URL for display, counting
// runes so that internationalised domains and percent-free unicode paths
// survive the cut intact.
func TruncateURL(u string) string {
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	return truncateRunes(u, 40)
}
