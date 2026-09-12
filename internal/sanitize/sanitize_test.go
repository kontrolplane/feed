package sanitize

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The article body is rendered with templ.Raw, so anything that survives
// ArticleHTML executes in the reader's origin. These cases are the attacks a
// hostile feed would actually ship, not smoke tests.
func TestArticleHTMLRemovesDangerousMarkup(t *testing.T) {
	tests := []struct {
		name string
		in   string
		// mustNotContain is matched case-insensitively against the output.
		mustNotContain []string
		mustContain    []string
	}{
		{
			name:           "script element",
			in:             `<p>before</p><script>alert(1)</script><p>after</p>`,
			mustNotContain: []string{"<script", "alert(1)"},
			mustContain:    []string{"before", "after"},
		},
		{
			name:           "img onerror handler",
			in:             `<img src="x" onerror="alert(1)">`,
			mustNotContain: []string{"onerror", "alert"},
		},
		{
			name:           "img onerror handler, obfuscated case and spacing",
			in:             `<IMG SRC=x OnErRoR = alert(1) >`,
			mustNotContain: []string{"onerror", "alert"},
		},
		{
			name:           "body-level event handler on a kept element",
			in:             `<p onmouseover="alert(1)" onclick="alert(2)">hover</p>`,
			mustNotContain: []string{"onmouseover", "onclick", "alert"},
			mustContain:    []string{"hover"},
		},
		{
			name:           "javascript href",
			in:             `<a href="javascript:alert(1)">click</a>`,
			mustNotContain: []string{"javascript", "alert"},
			mustContain:    []string{"click"},
		},
		{
			name:           "javascript href with leading whitespace",
			in:             `<a href=" javascript:alert(1)">click</a>`,
			mustNotContain: []string{"javascript", "alert"},
		},
		{
			name:           "javascript href with mixed case scheme",
			in:             `<a href="JaVaScRiPt:alert(1)">click</a>`,
			mustNotContain: []string{"javascript", "alert"},
		},
		{
			name:           "data text/html href",
			in:             `<a href="data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==">x</a>`,
			mustNotContain: []string{"data:", "base64"},
		},
		{
			name:           "data text/html img src",
			in:             `<img src="data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==">`,
			mustNotContain: []string{"data:", "base64", "<img"},
		},
		{
			name:           "style attribute overlaying the page",
			in:             `<div style="position:fixed;inset:0;z-index:9999">gotcha</div>`,
			mustNotContain: []string{"style", "position:fixed", "inset"},
			mustContain:    []string{"gotcha"},
		},
		{
			name:           "style element",
			in:             `<style>body{display:none}</style><p>text</p>`,
			mustNotContain: []string{"<style", "display:none"},
			mustContain:    []string{"text"},
		},
		{
			name:           "svg onload",
			in:             `<svg onload="alert(1)"><circle r="10"/></svg>`,
			mustNotContain: []string{"<svg", "onload", "alert"},
		},
		{
			name:           "iframe",
			in:             `<iframe src="https://evil.example/x"></iframe>`,
			mustNotContain: []string{"<iframe", "evil.example"},
		},
		{
			name:           "object",
			in:             `<object data="https://evil.example/x.swf"><param name="a" value="b"></object>`,
			mustNotContain: []string{"<object", "<param", "evil.example"},
		},
		{
			name:           "embed",
			in:             `<embed src="https://evil.example/x.swf">`,
			mustNotContain: []string{"<embed", "evil.example"},
		},
		{
			name:           "form and inputs",
			in:             `<form action="https://evil.example/steal"><input name="p" type="password"><button>go</button></form>`,
			mustNotContain: []string{"<form", "<input", "<button", "evil.example"},
		},
		{
			name:           "base tag repointing relative URLs",
			in:             `<base href="https://evil.example/"><p>text</p>`,
			mustNotContain: []string{"<base", "evil.example"},
			mustContain:    []string{"text"},
		},
		{
			name:           "meta refresh",
			in:             `<meta http-equiv="refresh" content="0;url=https://evil.example">`,
			mustNotContain: []string{"<meta", "refresh", "evil.example"},
		},
		{
			name:           "srcset smuggling a javascript candidate",
			in:             `<img src="https://ok.example/a.png" srcset="javascript:alert(1) 2x">`,
			mustNotContain: []string{"srcset", "javascript", "alert"},
			mustContain:    []string{"https://ok.example/a.png"},
		},
		{
			name:           "relative href resolving against our own origin",
			in:             `<a href="/feeds/delete?id=1">read more</a>`,
			mustNotContain: []string{"href"},
			mustContain:    []string{"read more"},
		},
		{
			name:           "malformed nesting used to hide a handler",
			in:             `<div><p>a<img src="x" onerror=alert(1)</p></div>`,
			mustNotContain: []string{"onerror", "alert"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ArticleHTML(tc.in)
			lower := strings.ToLower(got)
			for _, bad := range tc.mustNotContain {
				if strings.Contains(lower, strings.ToLower(bad)) {
					t.Errorf("output still contains %q\ninput:  %s\noutput: %s", bad, tc.in, got)
				}
			}
			for _, want := range tc.mustContain {
				if !strings.Contains(got, want) {
					t.Errorf("output lost %q\ninput:  %s\noutput: %s", want, tc.in, got)
				}
			}
		})
	}
}

// Sanitising is worthless if it leaves articles unreadable, so the formatting a
// feed legitimately uses has to come through untouched.
func TestArticleHTMLKeepsLegitimateContent(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		mustContain []string
	}{
		{
			name:        "headings",
			in:          `<h1>Title</h1><h2>Sub</h2><h3>Sub sub</h3>`,
			mustContain: []string{"<h1>Title</h1>", "<h2>Sub</h2>", "<h3>Sub sub</h3>"},
		},
		{
			name:        "unordered list",
			in:          `<ul><li>one</li><li>two</li></ul>`,
			mustContain: []string{"<ul>", "<li>one</li>", "<li>two</li>", "</ul>"},
		},
		{
			name:        "ordered list",
			in:          `<ol><li>one</li></ol>`,
			mustContain: []string{"<ol>", "<li>one</li>"},
		},
		{
			name:        "definition list",
			in:          `<dl><dt>term</dt><dd>meaning</dd></dl>`,
			mustContain: []string{"<dl>", "<dt>term</dt>", "<dd>meaning</dd>"},
		},
		{
			name:        "code block with highlight class",
			in:          `<pre class="language-go"><code class="language-go">fmt.Println("hi")</code></pre>`,
			mustContain: []string{`<pre class="language-go">`, `<code class="language-go">`, "fmt.Println"},
		},
		{
			name:        "blockquote with cite",
			in:          `<blockquote cite="https://example.com/src"><p>quoted</p></blockquote>`,
			mustContain: []string{"<blockquote", `cite="https://example.com/src"`, "quoted"},
		},
		{
			name:        "figure and caption",
			in:          `<figure><img src="https://example.com/a.png" alt="a"><figcaption>caption</figcaption></figure>`,
			mustContain: []string{"<figure>", "<figcaption>caption</figcaption>", "https://example.com/a.png"},
		},
		{
			name:        "table",
			in:          `<table><thead><tr><th>h</th></tr></thead><tbody><tr><td>c</td></tr></tbody></table>`,
			mustContain: []string{"<table>", "<thead>", "<th>h</th>", "<tbody>", "<td>c</td>"},
		},
		{
			name:        "inline phrasing",
			in:          `<p><strong>s</strong><em>e</em><b>b</b><i>i</i><sub>2</sub><sup>3</sup><mark>m</mark><small>sm</small><code>c</code></p>`,
			mustContain: []string{"<strong>s</strong>", "<em>e</em>", "<b>b</b>", "<i>i</i>", "<sub>2</sub>", "<sup>3</sup>", "<mark>m</mark>", "<small>sm</small>", "<code>c</code>"},
		},
		{
			name:        "breaks and rules",
			in:          `<p>a<br>b</p><hr>`,
			mustContain: []string{"<br", "<hr"},
		},
		{
			name:        "image with dimensions and title",
			in:          `<img src="https://example.com/a.png" alt="alt text" title="a title" width="640" height="480">`,
			mustContain: []string{`src="https://example.com/a.png"`, `alt="alt text"`, `title="a title"`, `width="640"`, `height="480"`},
		},
		{
			name:        "image with a well formed srcset",
			in:          `<img src="https://example.com/a.png" srcset="https://example.com/a.png 1x, https://example.com/a@2x.png 2x">`,
			mustContain: []string{"srcset=", "a@2x.png"},
		},
		{
			name:        "mailto link",
			in:          `<a href="mailto:someone@example.com">mail</a>`,
			mustContain: []string{`href="mailto:someone@example.com"`},
		},
		{
			name:        "unicode content is preserved",
			in:          `<p>日本語のテキスト — café 🎉</p>`,
			mustContain: []string{"日本語のテキスト", "café", "🎉"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ArticleHTML(tc.in)
			for _, want := range tc.mustContain {
				if !strings.Contains(got, want) {
					t.Errorf("output lost %q\ninput:  %s\noutput: %s", want, tc.in, got)
				}
			}
		})
	}
}

// External links must not hand the destination a window.opener handle back into
// the reader, nor leak what the user is reading via the referrer.
func TestArticleHTMLLinkHardening(t *testing.T) {
	got := ArticleHTML(`<a href="https://example.com/post">read</a>`)

	for _, want := range []string{`href="https://example.com/post"`, `target="_blank"`, "nofollow", "noopener", "noreferrer"} {
		if !strings.Contains(got, want) {
			t.Errorf("link output missing %q: %s", want, got)
		}
	}
}

func TestText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "plain text unchanged", in: "just words", want: "just words"},
		{name: "tags stripped", in: `<p>hello <strong>world</strong></p>`, want: "hello world"},
		{name: "block boundaries become spaces", in: `<p>one</p><p>two</p>`, want: "one two"},
		{name: "entities decoded", in: `caf&eacute; &amp; cr&egrave;me &lt;3`, want: "café & crème <3"},
		{name: "nbsp collapses", in: "a&nbsp;&nbsp;b", want: "a b"},
		{name: "whitespace collapsed", in: "  a \n\t  b  ", want: "a b"},
		{name: "script content removed entirely", in: `<script>alert(1)</script>text`, want: "text"},
		{name: "style content removed entirely", in: `<style>p{color:red}</style>text`, want: "text"},
		{name: "attributes never leak into the text", in: `<img src="x" onerror="alert(1)" alt="pic">caption`, want: "caption"},
		{name: "unicode survives", in: `<p>日本語 — 🎉</p>`, want: "日本語 — 🎉"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Text(tc.in)
			if got != tc.want {
				t.Errorf("Text(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("Text(%q) produced invalid UTF-8: %q", tc.in, got)
			}
		})
	}
}

// Postgres rejects the whole insert with `invalid byte sequence for encoding
// "UTF8"` if a string is cut mid-rune, so every result here must stay valid
// UTF-8. That is the regression this guards.
func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{name: "ascii under limit", in: "hello", n: 10, want: "hello"},
		{name: "ascii at limit", in: "hello", n: 5, want: "hello"},
		{name: "ascii over limit", in: "hello world", n: 5, want: "hello…"},
		{name: "empty string", in: "", n: 5, want: ""},
		{name: "empty string with zero n", in: "", n: 0, want: ""},
		{name: "zero n", in: "hello", n: 0, want: ""},
		{name: "negative n", in: "hello", n: -3, want: ""},

		// Three bytes per rune: a byte slice at n would land mid-rune.
		{name: "japanese truncated", in: "日本語のテキスト", n: 3, want: "日本語…"},
		{name: "japanese at exact boundary", in: "日本語", n: 3, want: "日本語"},
		{name: "japanese under limit", in: "日本語", n: 10, want: "日本語"},
		{name: "japanese cut at one rune", in: "日本語", n: 1, want: "日…"},

		// Four bytes per rune.
		{name: "emoji truncated", in: "🎉🎊🎈🎁", n: 2, want: "🎉🎊…"},
		{name: "emoji at exact boundary", in: "🎉🎊", n: 2, want: "🎉🎊"},

		// Two bytes per rune: len(s) > n while the rune count is not.
		{name: "accented latin under rune limit but over byte limit", in: "éééé", n: 4, want: "éééé"},
		{name: "accented latin truncated", in: "éééé", n: 2, want: "éé…"},
		{name: "mixed scripts", in: "abc日本🎉", n: 4, want: "abc日…"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateRunes(tc.in, tc.n)
			if got != tc.want {
				t.Errorf("TruncateRunes(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("TruncateRunes(%q, %d) produced invalid UTF-8: %q", tc.in, tc.n, got)
			}
		})
	}
}

// Every prefix length of a multi-byte string must come back as valid UTF-8;
// this is the property the byte-slicing call sites violated.
func TestTruncateRunesAlwaysValidUTF8(t *testing.T) {
	inputs := []string{
		"日本語のテキストです",
		"🎉🎊🎈🎁🎀",
		"café crème brûlée",
		"Ñandú — ñoño",
		"mixed abc 日本 🎉 déjà",
	}

	for _, in := range inputs {
		for n := -2; n <= utf8.RuneCountInString(in)+2; n++ {
			got := TruncateRunes(in, n)
			if !utf8.ValidString(got) {
				t.Fatalf("TruncateRunes(%q, %d) produced invalid UTF-8: %q", in, n, got)
			}
			if n > 0 && utf8.RuneCountInString(got) > n+1 {
				t.Fatalf("TruncateRunes(%q, %d) kept %d runes, want at most %d (+ ellipsis)",
					in, n, utf8.RuneCountInString(got), n)
			}
		}
	}
}

// ArticleHTML and Text are called from the fetcher, which processes feeds
// concurrently; the shared policies must be safe to use from many goroutines.
func TestPoliciesAreConcurrentSafe(t *testing.T) {
	const goroutines = 16
	done := make(chan struct{}, goroutines)

	for range goroutines {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 100 {
				if strings.Contains(ArticleHTML(`<p onclick="alert(1)">x</p>`), "onclick") {
					t.Error("event handler survived sanitisation")
					return
				}
				if Text(`<b>a</b> b`) != "a b" {
					t.Error("unexpected Text output")
					return
				}
			}
		}()
	}

	for range goroutines {
		<-done
	}
}
