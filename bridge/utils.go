package bridge

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pkg/errors"
)

// Leftpad is from github.com/douglarek/leftpad
func Leftpad(s string, length int, ch ...rune) string {
	c := ' '
	if len(ch) > 0 {
		c = ch[0]
	}
	l := length - utf8.RuneCountInString(s)
	if l > 0 {
		s = strings.Repeat(string(c), l) + s
	}
	return s
}

// SnowflakeToIP takes a snowflake and the first half of an IP to make an IP suitable for WEBIRC
func SnowflakeToIP(base string, snowflake string) string {
	num, err := strconv.ParseUint(snowflake, 10, 64)
	if err != nil {
		panic(errors.Wrap(err, "could not convert snowflake to uint"))
	}

	for i, c := range Leftpad(strconv.FormatUint(num, 16), 16, '0') {
		if (i % 4) == 0 {
			base += ":"
		}
		base += string(c)
	}

	return base
}

// discordTag returns "username#1234" for legacy Discord accounts, or just
// "username" for accounts migrated to Discord's unique usernames (whose
// discriminator is "0").
func discordTag(username, discriminator string) string {
	if !hasLegacyDiscriminator(discriminator) {
		return username
	}
	return username + "#" + discriminator
}

func hasLegacyDiscriminator(discriminator string) bool {
	return discriminator != "" && discriminator != "0"
}

// nickDisambiguator returns a short, stable tag used to make fallback nicks
// unique: the legacy discriminator if there is one, otherwise the last four
// digits of the user's snowflake ID.
func nickDisambiguator(user DiscordUser) string {
	if hasLegacyDiscriminator(user.Discriminator) {
		return user.Discriminator
	}
	id := user.ID
	if len(id) > 4 {
		id = id[len(id)-4:]
	}
	return id
}

// breakHighlight inserts a zero-width space after the first character of
// name so that relaying it to IRC does not highlight a user with that nick.
// It is UTF-8 aware, so multi-byte first characters are kept intact.
func breakHighlight(name string) string {
	if name == "" {
		return name
	}
	_, size := utf8.DecodeRuneInString(name)
	return name[:size] + "​" + name[size:]
}

// Conservative assumptions about how the server will prefix our messages
// when relaying them (":nick!user@host "), used to work out how much text
// fits into one 512-byte IRC line.
const (
	ircMaxLine     = 510 // 512 minus CRLF
	ircMaxUserLen  = 12
	ircMaxHostLen  = 63
	ircMinMsgBytes = 64
)

// ircMessageBudget returns how many bytes of message text can be sent in a
// single PRIVMSG to target by a client whose nick is nickLen bytes long.
func ircMessageBudget(target string, nickLen int, isAction bool) int {
	prefix := 1 + nickLen + 1 + ircMaxUserLen + 1 + ircMaxHostLen + 1
	command := len("PRIVMSG ") + len(target) + len(" :")
	budget := ircMaxLine - prefix - command
	if isAction {
		budget -= len("\x01ACTION \x01")
	}
	if budget < ircMinMsgBytes {
		budget = ircMinMsgBytes
	}
	return budget
}

// splitIRCMessage splits text into chunks of at most max bytes, never
// splitting a UTF-8 character, and preferring to break on a space.
func splitIRCMessage(text string, max int) []string {
	if max <= 0 || len(text) <= max {
		return []string{text}
	}

	var chunks []string
	for len(text) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if cut == 0 {
			// A single character longer than max; send it whole.
			_, cut = utf8.DecodeRuneInString(text)
		}

		if space := strings.LastIndexByte(text[:cut], ' '); space > max/2 {
			chunks = append(chunks, text[:space])
			text = text[space+1:]
			continue
		}

		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}

// TruncateString is derived from https://github.com/gohugoio/hugo/blob/a03c631c420a03f9d90699abdf9be7e4fca0ff61/tpl/strings/truncate.go#L43
func TruncateString(length int, text string) string {
	ellipsis := " …"

	if utf8.RuneCountInString(text) <= length {
		return text
	}

	var lastWordIndex, lastNonSpace, currentLen, endTextPos int

	for i, r := range text {
		currentLen++
		if unicode.IsSpace(r) {
			lastWordIndex = lastNonSpace
		} else if unicode.In(r, unicode.Han, unicode.Hangul, unicode.Hiragana, unicode.Katakana) {
			lastWordIndex = i
		} else {
			lastNonSpace = i + utf8.RuneLen(r)
		}

		if currentLen > length {
			if lastWordIndex == 0 {
				endTextPos = i
			} else {
				endTextPos = lastWordIndex
			}
			out := text[0:endTextPos]

			return out + ellipsis
		}
	}

	return text
}
