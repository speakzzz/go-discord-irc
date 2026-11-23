package bridge

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// IRC formatting control codes. Each one toggles its style.
const (
	ircBold      = "\x02"
	ircItalic    = "\x1D"
	ircUnderline = "\x1F"
	ircStrike    = "\x1E"
	ircMono      = "\x11"
)

// markerCodes maps Discord emphasis markers to the IRC codes they become.
var markerCodes = map[string]string{
	"*":   ircItalic,
	"_":   ircItalic,
	"**":  ircBold,
	"__":  ircUnderline,
	"***": ircBold + ircItalic,
	"___": ircUnderline + ircItalic,
	"~~":  ircStrike,
}

// Things that must pass through untouched: Discord mentions, custom emoji
// and timestamps (ParseText rewrites those later), and URLs, which often
// contain underscores or asterisks. <https://...> is Discord's "no embed"
// link syntax; its brackets are dropped.
var (
	verbatimToken = regexp.MustCompile(`^(?:<a?:\w+:\d+>|<[@#][!&]?\d+>|<t:-?\d+(?::\w)?>|https?://\S+)`)
	autolinkToken = regexp.MustCompile(`^<(https?://[^>\s]+)>`)
	maskedLink    = regexp.MustCompile(`^\[([^\]\n]+)\]\(<?(https?://[^)>\s]+)>?\)`)
	headerLine    = regexp.MustCompile(`^#{1,3} (.+)$`)
)

// discordMarkdownToIRC converts Discord markdown in a message to IRC
// formatting codes: **bold**, *italic*/_italic_, __underline__,
// ~~strikethrough~~, `code` and ``` blocks (monospace), # headers (bold),
// -# subtext, [masked](links) and \escapes. Formatting is closed at the end
// of every line, because each line becomes a separate IRC message.
func discordMarkdownToIRC(s string) string {
	if !strings.ContainsAny(s, "*_~`#[\\<-") {
		return s
	}

	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	inBlock := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if inBlock {
			if strings.HasSuffix(trimmed, "```") {
				inBlock = false
				if code := strings.TrimSuffix(strings.TrimRight(line, " \t"), "```"); strings.TrimSpace(code) != "" {
					out = append(out, ircMono+code+ircMono)
				}
				continue
			}
			if line != "" {
				out = append(out, ircMono+line+ircMono)
			}
			continue
		}

		// Opening fence: ```lang, or ``` followed by code on the same line.
		// A fence that also closes on this line is an inline code span.
		if strings.HasPrefix(trimmed, "```") && !strings.Contains(trimmed[3:], "```") {
			inBlock = true
			if rest := trimmed[3:]; strings.ContainsAny(rest, " \t") {
				out = append(out, ircMono+rest+ircMono)
			}
			continue
		}

		out = append(out, convertMarkdownLine(line))
	}

	return strings.Join(out, "\n")
}

func convertMarkdownLine(line string) string {
	switch {
	case strings.HasPrefix(line, "-# "):
		return convertInlineMarkdown(line[3:])
	case strings.HasPrefix(line, ">>> "):
		return "> " + convertInlineMarkdown(line[4:])
	case strings.HasPrefix(line, "> "):
		return "> " + convertInlineMarkdown(line[2:])
	}
	if m := headerLine.FindStringSubmatch(line); m != nil {
		return ircBold + convertInlineMarkdown(m[1]) + ircBold
	}
	return convertInlineMarkdown(line)
}

type mdToken struct {
	text     string // literal text, or the marker itself
	marker   string // non-empty for emphasis markers
	canOpen  bool
	canClose bool
	paired   bool
}

func isMarkdownPunct(b byte) bool {
	return strings.IndexByte("*_~`|>#-[]()\\<:", b) >= 0
}

func runeBefore(s string, i int) (rune, bool) {
	if i <= 0 {
		return 0, false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return r, true
}

func runeAt(s string, i int) (rune, bool) {
	if i >= len(s) {
		return 0, false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return r, true
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func convertInlineMarkdown(s string) string {
	var tokens []mdToken
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			tokens = append(tokens, mdToken{text: lit.String()})
			lit.Reset()
		}
	}

	for i := 0; i < len(s); {
		c := s[i]

		// \* and friends: literal punctuation
		if c == '\\' && i+1 < len(s) && isMarkdownPunct(s[i+1]) {
			lit.WriteByte(s[i+1])
			i += 2
			continue
		}

		rest := s[i:]

		if loc := autolinkToken.FindStringSubmatchIndex(rest); loc != nil {
			lit.WriteString(rest[loc[2]:loc[3]])
			i += loc[1]
			continue
		}
		if loc := verbatimToken.FindStringIndex(rest); loc != nil {
			lit.WriteString(rest[:loc[1]])
			i += loc[1]
			continue
		}
		if m := maskedLink.FindStringSubmatch(rest); m != nil {
			lit.WriteString(convertInlineMarkdown(m[1]) + " (" + m[2] + ")")
			i += len(m[0])
			continue
		}

		switch c {
		case '`':
			n := 0
			for i+n < len(s) && s[i+n] == '`' {
				n++
			}
			fence := strings.Repeat("`", n)
			if end := strings.Index(s[i+n:], fence); end >= 0 {
				code := s[i+n : i+n+end]
				if len(code) > 1 && code[0] == ' ' && code[len(code)-1] == ' ' {
					code = code[1 : len(code)-1]
				}
				lit.WriteString(ircMono + code + ircMono)
				i += n + end + n
			} else {
				lit.WriteString(fence)
				i += n
			}
			continue

		case '*', '_', '~':
			n := 0
			for i+n < len(s) && s[i+n] == c {
				n++
			}
			marker := s[i : i+n]
			if _, ok := markerCodes[marker]; !ok {
				// "~", "****", etc.: not emphasis
				lit.WriteString(marker)
				i += n
				continue
			}

			prev, hasPrev := runeBefore(s, i)
			next, hasNext := runeAt(s, i+n)
			tok := mdToken{
				text:     marker,
				marker:   marker,
				canOpen:  hasNext && !unicode.IsSpace(next),
				canClose: hasPrev && !unicode.IsSpace(prev),
			}
			// Underscores only work at word boundaries (snake_case stays).
			if c == '_' {
				tok.canOpen = tok.canOpen && !(hasPrev && isWordRune(prev))
				tok.canClose = tok.canClose && !(hasNext && isWordRune(next))
			}

			flush()
			tokens = append(tokens, tok)
			i += n
			continue
		}

		_, size := utf8.DecodeRuneInString(rest)
		lit.WriteString(rest[:size])
		i += size
	}
	flush()

	// Pair closers with the nearest matching opener; anything unpaired
	// (e.g. the * in "5 * 3") stays literal.
	var stack []int
	for idx := range tokens {
		tok := &tokens[idx]
		if tok.marker == "" {
			continue
		}
		if tok.canClose {
			found := -1
			for j := len(stack) - 1; j >= 0; j-- {
				if tokens[stack[j]].marker == tok.marker {
					found = j
					break
				}
			}
			if found >= 0 {
				tokens[stack[found]].paired = true
				tok.paired = true
				stack = stack[:found]
				continue
			}
		}
		if tok.canOpen {
			stack = append(stack, idx)
		}
	}

	var out strings.Builder
	for _, tok := range tokens {
		if tok.paired {
			out.WriteString(markerCodes[tok.marker])
		} else {
			out.WriteString(tok.text)
		}
	}
	return out.String()
}
