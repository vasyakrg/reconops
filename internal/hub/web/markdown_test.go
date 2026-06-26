package web

import (
	"strings"
	"testing"
)

func TestRenderMarkdownInline_Formatting(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string // substrings that MUST appear
		deny []string // substrings that must NOT appear
	}{
		{"code span", "check `bond0` now", []string{"<code>bond0</code>"}, nil},
		{"bold", "this is **critical** here", []string{"<strong>critical</strong>"}, nil},
		{"italic", "a *stressed* word", []string{"<em>stressed</em>"}, nil},
		{"safe http link", "see [docs](https://example.com/x)",
			[]string{`<a href="https://example.com/x" rel="noopener noreferrer nofollow" target="_blank">docs</a>`}, nil},
		{"safe mailto link", "mail [me](mailto:a@b.co)", []string{`href="mailto:a@b.co"`}, nil},
		// snake_case identifiers must not be italicised (single underscores).
		{"snake_case untouched", "field ad_select changed", []string{"ad_select"}, []string{"<em>"}},
		// spaced asterisks (multiplication / globs) must not become italics.
		{"spaced asterisks", "min_links 2 * 3 * 4", nil, []string{"<em>"}},
		// a `*` INSIDE a code span stays literal, never bolded.
		{"no format inside code", "`a*b*c`", []string{"<code>a*b*c</code>"}, []string{"<em>", "<strong>"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(renderMarkdownInline(tc.in))
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("want %q in %q", w, got)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(got, d) {
					t.Errorf("did NOT want %q in %q", d, got)
				}
			}
		})
	}
}

// The renderer is a trust boundary: model text can be steered by injected tool
// data, so any HTML/script/unsafe-scheme in the input must be neutralised.
func TestRenderMarkdown_XSSSafety(t *testing.T) {
	cases := []struct {
		name string
		in   string
		deny []string // markup that must never reach the browser
		want []string // optional: proof the dangerous text was escaped to inert form
	}{
		{"script tag", "<script>alert(1)</script>",
			[]string{"<script>", "</script>"}, []string{"&lt;script&gt;"}},
		{"img onerror", `<img src=x onerror="alert(1)">`,
			[]string{"<img", `onerror="alert`}, []string{"&lt;img"}},
		{"javascript link", "[x](javascript:alert(1))",
			[]string{"<a ", `href="javascript`}, []string{"[x](javascript:alert(1))"}},
		{"data link", "[x](data:text/html,<script>)",
			[]string{"<a ", "<script>"}, nil},
		{"attr break-out via quote", `[x](https://a" onmouseover="alert(1))`,
			// the space splits the url so it is not a safe-scheme single token;
			// either way no raw double-quote/handler may escape the attribute.
			[]string{`onmouseover="alert`}, nil},
		{"raw quote in text", `say "<b>hi</b>"`,
			[]string{"<b>", "</b>"}, []string{"&lt;b&gt;"}},
	}
	for _, tc := range cases {
		t.Run("inline/"+tc.name, func(t *testing.T) {
			got := string(renderMarkdownInline(tc.in))
			assertSafe(t, got, tc.want, tc.deny)
		})
		t.Run("block/"+tc.name, func(t *testing.T) {
			got := string(renderMarkdownBlock(tc.in))
			assertSafe(t, got, tc.want, tc.deny)
		})
	}
}

func assertSafe(t *testing.T, got string, want, deny []string) {
	t.Helper()
	for _, d := range deny {
		if strings.Contains(got, d) {
			t.Errorf("UNSAFE: %q leaked into %q", d, got)
		}
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("want escaped form %q in %q", w, got)
		}
	}
}

func TestRenderMarkdownBlock_Structure(t *testing.T) {
	t.Run("paragraph with line break", func(t *testing.T) {
		got := string(renderMarkdownBlock("line one\nline two"))
		if !strings.Contains(got, "<p>line one<br>line two</p>") {
			t.Fatalf("paragraph/br wrong: %q", got)
		}
	})
	t.Run("two paragraphs", func(t *testing.T) {
		got := string(renderMarkdownBlock("para one\n\npara two"))
		if strings.Count(got, "<p>") != 2 {
			t.Fatalf("want 2 paragraphs: %q", got)
		}
	})
	t.Run("bullet list", func(t *testing.T) {
		got := string(renderMarkdownBlock("- first\n- second"))
		if !strings.Contains(got, "<ul><li>first</li><li>second</li></ul>") {
			t.Fatalf("bullet list wrong: %q", got)
		}
	})
	t.Run("ordered list", func(t *testing.T) {
		got := string(renderMarkdownBlock("1. a\n2. b"))
		if !strings.Contains(got, "<ol><li>a</li><li>b</li></ol>") {
			t.Fatalf("ordered list wrong: %q", got)
		}
	})
	t.Run("fenced code escapes and skips inline", func(t *testing.T) {
		got := string(renderMarkdownBlock("```\n<script> **x**\n```"))
		if !strings.Contains(got, "<pre><code>&lt;script&gt; **x**</code></pre>") {
			t.Fatalf("fenced code wrong: %q", got)
		}
		if strings.Contains(got, "<strong>") {
			t.Fatalf("inline formatting must not run inside a code fence: %q", got)
		}
	})
	t.Run("heading", func(t *testing.T) {
		got := string(renderMarkdownBlock("# Title"))
		if !strings.Contains(got, "<h4>Title</h4>") {
			t.Fatalf("heading wrong: %q", got)
		}
	})
	t.Run("inline formatting inside paragraph", func(t *testing.T) {
		got := string(renderMarkdownBlock("the `bond0` is **down**"))
		if !strings.Contains(got, "<code>bond0</code>") || !strings.Contains(got, "<strong>down</strong>") {
			t.Fatalf("inline-in-block wrong: %q", got)
		}
	})
}

func TestRenderMarkdownInline_CollapsesNewlines(t *testing.T) {
	got := string(renderMarkdownInline("line one\nline two"))
	if strings.Contains(got, "\n") || strings.Contains(got, "<br>") {
		t.Fatalf("inline render must collapse newlines: %q", got)
	}
	if !strings.Contains(got, "line one line two") {
		t.Fatalf("collapsed text wrong: %q", got)
	}
}
