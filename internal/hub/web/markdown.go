package web

import (
	"html"
	"html/template"
	"regexp"
	"strings"
)

// Safe, dependency-free Markdown rendering for investigator free-text
// (per-turn rationale, finding messages, mark_done prose). The model is asked
// to emit Markdown (see investigator/prompt.go) and the operator UI renders it.
//
// SECURITY — this is a trust boundary. LLM free-text can be steered by injected
// tool data (the prompt-injection seam), so it is treated as untrusted. The
// rules below are load-bearing, not stylistic:
//
//   - Every byte of input is HTML-escaped FIRST. Raw HTML in the source can
//     never reach the browser as markup — `<script>` becomes text.
//   - Only a fixed allowlist of tags is ever emitted by this renderer itself
//     (p, br, strong, em, code, pre, ul/ol/li, h4-h6, a). No attributes are
//     taken from input except link hrefs, which are restricted to the
//     http/https/mailto schemes; any other scheme leaves the link literal.
//   - The output is the ONLY thing typed template.HTML. Never wrap un-rendered
//     model/user text as template.HTML.
//
// This is deliberately a small subset, not a CommonMark implementation: a
// general Markdown library would add a dependency and (e.g. goldmark) still
// emit javascript: links unless configured. The subset covers what the model
// actually produces: inline code, bold/italic, links, lists, fenced code
// blocks, headings.

var (
	mdCodeSpanRe = regexp.MustCompile("`([^`]+)`")
	mdLinkRe     = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	mdBoldRe     = regexp.MustCompile(`\*\*([^*]+?)\*\*`)
	// Italic: a single-asterisk pair hugging non-space text, so spaced "*"
	// (multiplication, glob fragments) and snake_case never get mangled.
	mdItalicRe  = regexp.MustCompile(`\*([^\s*][^*]*?[^\s*]|[^\s*])\*`)
	mdHeadingRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	mdULItemRe  = regexp.MustCompile(`^\s*[-*+]\s+(.*)$`)
	mdOLItemRe  = regexp.MustCompile(`^\s*\d+\.\s+(.*)$`)
)

// renderMarkdownInline renders inline Markdown only (no block wrapping), for
// single-line contexts: rationale, finding message, list items, prior
// conclusions. Newlines collapse to spaces.
func renderMarkdownInline(src string) template.HTML {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	src = strings.ReplaceAll(src, "\n", " ")
	return template.HTML(mdInline(src))
}

// renderMarkdownBlock renders block-level Markdown (paragraphs, lists, fenced
// code, headings) reusing the inline core for line content. For multi-line
// free-text: root_cause, recommended_remediation.
func renderMarkdownBlock(src string) template.HTML {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	lines := strings.Split(src, "\n")
	var b strings.Builder
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// Fenced code block: raw escaped text, no inline formatting inside.
		if strings.HasPrefix(trimmed, "```") {
			i++
			var code []string
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				code = append(code, lines[i])
				i++
			}
			if i < len(lines) {
				i++ // consume the closing fence
			}
			b.WriteString("<pre><code>")
			b.WriteString(html.EscapeString(strings.Join(code, "\n")))
			b.WriteString("</code></pre>")
			continue
		}

		if trimmed == "" {
			i++
			continue
		}

		if m := mdHeadingRe.FindStringSubmatch(line); m != nil {
			tag := "h6"
			switch level := len(m[1]); {
			case level <= 2:
				tag = "h4"
			case level <= 4:
				tag = "h5"
			}
			b.WriteString("<" + tag + ">")
			b.WriteString(mdInline(m[2]))
			b.WriteString("</" + tag + ">")
			i++
			continue
		}

		if mdULItemRe.MatchString(line) {
			b.WriteString("<ul>")
			for i < len(lines) && mdULItemRe.MatchString(lines[i]) {
				b.WriteString("<li>" + mdInline(mdULItemRe.FindStringSubmatch(lines[i])[1]) + "</li>")
				i++
			}
			b.WriteString("</ul>")
			continue
		}

		if mdOLItemRe.MatchString(line) {
			b.WriteString("<ol>")
			for i < len(lines) && mdOLItemRe.MatchString(lines[i]) {
				b.WriteString("<li>" + mdInline(mdOLItemRe.FindStringSubmatch(lines[i])[1]) + "</li>")
				i++
			}
			b.WriteString("</ol>")
			continue
		}

		// Paragraph: gather consecutive lines until a blank line or a block start.
		var para []string
		for i < len(lines) {
			if isBlockBoundary(lines[i]) {
				break
			}
			para = append(para, mdInline(strings.TrimSpace(lines[i])))
			i++
		}
		b.WriteString("<p>" + strings.Join(para, "<br>") + "</p>")
	}
	return template.HTML(b.String())
}

func isBlockBoundary(line string) bool {
	t := strings.TrimSpace(line)
	return t == "" || strings.HasPrefix(t, "```") || mdHeadingRe.MatchString(line) ||
		mdULItemRe.MatchString(line) || mdOLItemRe.MatchString(line)
}

// mdInline turns one run of inline Markdown into safe HTML. Code spans are
// isolated first so their contents are escaped but never re-formatted (a `*`
// inside backticks must stay literal).
func mdInline(src string) string {
	var b strings.Builder
	locs := mdCodeSpanRe.FindAllStringSubmatchIndex(src, -1)
	last := 0
	for _, loc := range locs {
		if loc[0] > last {
			b.WriteString(mdFormatText(src[last:loc[0]]))
		}
		b.WriteString("<code>")
		b.WriteString(html.EscapeString(src[loc[2]:loc[3]]))
		b.WriteString("</code>")
		last = loc[1]
	}
	if last < len(src) {
		b.WriteString(mdFormatText(src[last:]))
	}
	return b.String()
}

// mdFormatText escapes a non-code text run, then applies links and emphasis on
// the already-escaped string. Escaping runs before any transform, so input
// markup is inert; the regexes only ever match Markdown delimiters, which
// html.EscapeString leaves untouched.
func mdFormatText(s string) string {
	s = html.EscapeString(s)
	s = mdLinkRe.ReplaceAllStringFunc(s, mdRenderLink)
	s = mdBoldRe.ReplaceAllString(s, "<strong>$1</strong>")
	s = mdItalicRe.ReplaceAllString(s, "<em>$1</em>")
	return s
}

// mdRenderLink renders [text](url) only for safe schemes; anything else is left
// as the literal (already-escaped) source. url/text are post-escape, so neither
// can break out of the href attribute or inject markup.
func mdRenderLink(match string) string {
	m := mdLinkRe.FindStringSubmatch(match)
	if m == nil {
		return match
	}
	text, url := m[1], m[2]
	low := strings.ToLower(url)
	if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") && !strings.HasPrefix(low, "mailto:") {
		return match
	}
	return `<a href="` + url + `" rel="noopener noreferrer nofollow" target="_blank">` + text + `</a>`
}
