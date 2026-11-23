package bridge

import (
	"regexp"
	"strings"

	ircnick "github.com/qaisjp/go-discord-irc/irc/nick"
)

// ircLower folds a nick using RFC 1459 casemapping (the default for
// charybdis/Solanum and ircu): A-Z, and []\~ are the uppercase of {}|^.
func ircLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z':
			b[i] = c + ('a' - 'A')
		case c == '[':
			b[i] = '{'
		case c == ']':
			b[i] = '}'
		case c == '\\':
			b[i] = '|'
		case c == '~':
			b[i] = '^'
		}
	}
	return string(b)
}

var urlToken = regexp.MustCompile(`^https?://\S+`)

// replaceNickMentions turns puppet nicks in an IRC message into Discord
// mentions. Only whole nicks match (so "al_d2" inside "real_d2s" or a URL
// doesn't ping anyone), compared case-insensitively as IRC does.
// mentions maps ircLower(nick) to the Discord mention.
func replaceNickMentions(msg string, mentions map[string]string) string {
	if len(mentions) == 0 {
		return msg
	}

	var out strings.Builder
	for i := 0; i < len(msg); {
		if loc := urlToken.FindStringIndex(msg[i:]); loc != nil && (i == 0 || !ircnick.IsNickChar(msg[i-1])) {
			out.WriteString(msg[i : i+loc[1]])
			i += loc[1]
			continue
		}

		if !ircnick.IsNickChar(msg[i]) {
			out.WriteByte(msg[i])
			i++
			continue
		}

		j := i
		for j < len(msg) && ircnick.IsNickChar(msg[j]) {
			j++
		}
		word := msg[i:j]
		if mention, ok := mentions[ircLower(word)]; ok {
			out.WriteString(mention)
		} else {
			out.WriteString(word)
		}
		i = j
	}
	return out.String()
}
