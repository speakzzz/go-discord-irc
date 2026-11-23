package bridge

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	irc "github.com/qaisjp/go-ircevent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Readable stand-ins for the control codes in expectations.
func vis(s string) string {
	return strings.NewReplacer(ircBold, "<B>", ircItalic, "<I>", ircUnderline, "<U>",
		ircStrike, "<S>", ircMono, "<M>").Replace(s)
}

func TestDiscordMarkdownToIRC(t *testing.T) {
	cases := []struct{ in, want string }{
		// emphasis
		{"**bold**", "<B>bold<B>"},
		{"*italic* and _italic_", "<I>italic<I> and <I>italic<I>"},
		{"__under__ ~~gone~~", "<U>under<U> <S>gone<S>"},
		{"***both***", "<B><I>both<B><I>"},
		{"**bold *nested* bold**", "<B>bold <I>nested<I> bold<B>"},
		// things that must stay literal
		{"5 * 3 = 15", "5 * 3 = 15"},
		{"snake_case_name and __init__.py", "snake_case_name and <U>init<U>.py"},
		{"a ~ b", "a ~ b"},
		{"**unclosed", "**unclosed"},
		{`\*not italic\*`, "*not italic*"},
		{"* list item", "* list item"},
		{"no markdown here", "no markdown here"},
		// code
		{"use `go_build *now*`", "use <M>go_build *now*<M>"},
		{"``a `tick` b``", "<M>a `tick` b<M>"},
		{"```inline```", "<M>inline<M>"},
		{"```go\nfmt.Println(\"*x*\")\n\nreturn\n```", "<M>fmt.Println(\"*x*\")<M>\n<M>return<M>"},
		{"```\nunterminated", "<M>unterminated<M>"},
		// tokens and links pass through
		{"hi <@123456789012345678> <:pepe_hands:1234>", "hi <@123456789012345678> <:pepe_hands:1234>"},
		{"see https://example.com/a_b_c*d*", "see https://example.com/a_b_c*d*"},
		{"see <https://example.com/x_y_>", "see https://example.com/x_y_"},
		{"[the **docs**](https://example.com/d)", "the <B>docs<B> (https://example.com/d)"},
		// line-level
		{"# Title", "<B>Title<B>"},
		{"## Sub *title*", "<B>Sub <I>title<I><B>"},
		{"-# small print", "small print"},
		{"> quoted **text**", "> quoted <B>text<B>"},
		{"#channel stays", "#channel stays"},
		// per-line: formatting never runs across lines
		{"**a\nb**", "**a\nb**"},
		// spoilers are handled later, leave them alone
		{"||secret||", "||secret||"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, vis(discordMarkdownToIRC(c.in)), "input: %q", c.in)
	}
}

func TestReplaceNickMentions(t *testing.T) {
	mentions := map[string]string{
		ircLower("bob_d2"):    "<@!1>",
		ircLower("[al]_d2"):   "<@!2>",
		ircLower("Speakz_d2"): "<@!3>",
	}
	cases := []struct{ in, want string }{
		{"bob_d2: hi", "<@!1>: hi"},
		{"hi BOB_D2, how are you", "hi <@!1>, how are you"}, // IRC nicks are case-insensitive
		{"{AL}_d2 and [al]_d2", "<@!2> and <@!2>"},          // RFC 1459: {} == []
		{"xbob_d2 bob_d2s bob_d2", "xbob_d2 bob_d2s <@!1>"}, // whole nicks only
		{"see https://x.com/bob_d2 ok", "see https://x.com/bob_d2 ok"},
		{"speakz_d2!", "<@!3>!"},
		{"nobody here", "nobody here"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, replaceNickMentions(c.in, mentions), c.in)
	}
	assert.Equal(t, "bob_d2", replaceNickMentions("bob_d2", nil))
}

func TestCompactEdit(t *testing.T) {
	cases := []struct{ old, new, want string }{
		{"i like teh cats", "i like the cats", "s/teh/the/"},
		{"see you tomorow at 5", "see you tomorrow at 6", "s/tomorow at 5/tomorrow at 6/"},
		{"hello world", "hello big world", "s/hello/hello big/"},
		{"hello world", "big hello world", "s/hello/big hello/"},
		{"remove this word please", "remove word please", "s/remove this/remove/"},
		{"oops oops", "oops", "s/oops oops/oops/"},
		{"path is a/b", "path is a/c", "s|a/b|a/c|"},
		// too big, ambiguous or multi-line: fall back to a full edit
		{"one two three four five", "uno dos tres cuatro cinco", ""},
		{"the cat and the dog", "the cat and a dog", ""}, // "the" occurs twice
		{"line one\nline two", "line one\nline 2", ""},
		{"same", "same", ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, compactEdit(c.old, c.new), "%q -> %q", c.old, c.new)
	}
}

func TestEditToRelay(t *testing.T) {
	b := newTestBridge(t)
	d := b.discord

	d.relayed.put("m1", "i like teh cats")

	// no-op update (e.g. link preview added): nothing to relay
	b.UpdateConfig(func(c *Config) { c.DiscordEdits = EditsFull })
	_, _, relay := d.editToRelay("m1", "i like teh cats", false)
	assert.False(t, relay)

	// compact
	b.UpdateConfig(func(c *Config) { c.DiscordEdits = EditsCompact })
	content, action, relay := d.editToRelay("m1", "i like the cats", true)
	assert.True(t, relay)
	assert.Equal(t, "s/teh/the/", content)
	assert.False(t, action, "a correction is not itself an action")

	// compact falls back to full for big changes and unknown messages
	content, action, relay = d.editToRelay("m1", "completely different text now here", true)
	assert.True(t, relay)
	assert.Equal(t, "[edit] completely different text now here", content)
	assert.True(t, action)
	content, _, _ = d.editToRelay("unknown", "edited", false)
	assert.Equal(t, "[edit] edited", content)

	// off
	b.UpdateConfig(func(c *Config) { c.DiscordEdits = EditsOff })
	_, _, relay = d.editToRelay("m1", "anything", false)
	assert.False(t, relay)
}

func TestMessageCacheEvicts(t *testing.T) {
	c := newMessageCache(3)
	for i := 0; i < 5; i++ {
		c.put(fmt.Sprint(i), fmt.Sprint("msg", i))
	}
	_, ok := c.get("0")
	assert.False(t, ok)
	_, ok = c.get("1")
	assert.False(t, ok)
	v, ok := c.get("4")
	assert.True(t, ok)
	assert.Equal(t, "msg4", v)

	c.put("4", "updated") // updating doesn't grow the cache
	v, _ = c.get("4")
	assert.Equal(t, "updated", v)
	_, ok = c.get("2")
	assert.True(t, ok)
}

func TestTopicChangeMessage(t *testing.T) {
	b := newTestBridge(t)
	require.NoError(t, b.SetChannelMappings(map[string]string{"#evilnet": "1"}))
	l := b.ircListener

	topic := func(nick, channel string, args ...string) {
		l.onTopicChange(&irc.Event{Code: "TOPIC", Nick: nick, Source: nick + "!u@h",
			Arguments: append([]string{channel}, args...)})
	}
	next := func() string {
		select {
		case m := <-b.discordMessagesChan:
			return m.IRCChannel + " " + m.Message
		default:
			return ""
		}
	}
	waitNext := func() string {
		for i := 0; i < 100; i++ {
			if m := next(); m != "" {
				return m
			}
			waitABit()
		}
		return ""
	}

	// off by default in the test bridge
	topic("speakz", "#evilnet", "hello")
	waitABit()
	assert.Equal(t, "", next())

	b.UpdateConfig(func(c *Config) { c.IRCTopicToDiscord = TopicMessage })
	topic("speakz", "#evilnet", "\x02Welcome\x02 to EvilNET")
	assert.Equal(t, "#evilnet _speakz changed the topic to:_ **Welcome** to EvilNET", waitNext())

	topic("speakz", "#evilnet", "")
	assert.Equal(t, "#evilnet _speakz cleared the topic_", waitNext())

	topic("speakz", "#unmapped", "x")
	waitABit()
	assert.Equal(t, "", next())
}

func TestSyncTopicSkipsNoops(t *testing.T) {
	b := newTestBridge(t)
	d := b.discord
	require.NoError(t, d.Session.State.ChannelAdd(&discordgo.Channel{ID: "1", GuildID: "guild", Topic: "same"}))

	// Unchanged or empty topics never reach the Discord API (which this
	// test has no connection to).
	d.syncTopic("1", "same")
	d.syncTopic("1", "")
	d.topics.mu.Lock()
	assert.Empty(t, d.topics.pending)
	assert.Empty(t, d.topics.running)
	d.topics.mu.Unlock()

	assert.Equal(t, strings.Repeat("é", discordTopicMax), truncateRunes(strings.Repeat("é", discordTopicMax+5), discordTopicMax))
	assert.Equal(t, "short", truncateRunes("short", discordTopicMax))
}

func waitABit() { time.Sleep(5 * time.Millisecond) }
