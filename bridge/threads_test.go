package bridge

import (
	"testing"

	"github.com/42wim/matterbridge/bridge/discord/transmitter"
	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newThreadTestBridge maps #evilnet to Discord channel 100, which has a
// public thread 200 called "Server move plan".
func newThreadTestBridge(t *testing.T) *Bridge {
	t.Helper()
	b := newTestBridge(t)
	d := b.discord
	d.transmitter = transmitter.New(d.Session, "guild", "irc-bridge", false)
	d.Session.State.User = &discordgo.User{ID: "bot"}

	state := d.Session.State
	require.NoError(t, state.ChannelAdd(&discordgo.Channel{ID: "100", GuildID: "guild", Type: discordgo.ChannelTypeGuildText, Name: "evilnet"}))
	require.NoError(t, state.ChannelAdd(&discordgo.Channel{ID: "200", GuildID: "guild", Type: discordgo.ChannelTypeGuildPublicThread, ParentID: "100", Name: "Server move plan"}))
	require.NoError(t, state.ChannelAdd(&discordgo.Channel{ID: "900", GuildID: "guild", Type: discordgo.ChannelTypeGuildText, Name: "unmapped"}))
	require.NoError(t, b.SetChannelMappings(map[string]string{"#evilnet": "100"}))

	b.UpdateConfig(func(c *Config) {
		c.DiscordThreads = true
		c.DiscordMarkdownToIRC = true
		c.DiscordEdits = EditsCompact
	})
	return b
}

func drain(b *Bridge) []*DiscordMessage {
	var out []*DiscordMessage
	for {
		select {
		case m := <-b.discordMessageEventsChan:
			out = append(out, m)
		default:
			return out
		}
	}
}

var bob = &discordgo.User{ID: "u1", Username: "bob"}

func post(b *Bridge, m *discordgo.Message, edit bool) []*DiscordMessage {
	if m.Author == nil {
		m.Author = bob
	}
	if m.GuildID == "" {
		m.GuildID = "guild"
	}
	b.discord.publishMessage(b.discord.Session, m, edit)
	return drain(b)
}

func TestThreadMessagesGoToParent(t *testing.T) {
	b := newThreadTestBridge(t)

	out := post(b, &discordgo.Message{ID: "m1", ChannelID: "200", Content: "hello\nsecond **line**"}, false)
	require.Len(t, out, 1)
	assert.Equal(t, "100", out[0].ChannelID, "relayed to the parent's mapping")
	assert.Equal(t, "[Server move plan] hello\n[Server move plan] second \x02line\x02", out[0].Content)

	// actions keep the tag after the nick: "* bob [Server move plan] waves"
	out = post(b, &discordgo.Message{ID: "m2", ChannelID: "200", Content: "_waves_"}, false)
	require.Len(t, out, 1)
	assert.True(t, out[0].IsAction)
	assert.Equal(t, "[Server move plan] waves", out[0].Content)

	// edits in threads are tagged too, and compact still works
	post(b, &discordgo.Message{ID: "m3", ChannelID: "200", Content: "i like teh cats"}, false)
	out = post(b, &discordgo.Message{ID: "m3", ChannelID: "200", Content: "i like the cats"}, true)
	require.Len(t, out, 1)
	assert.Equal(t, "[Server move plan] s/teh/the/", out[0].Content)

	// messages in the parent channel itself are untagged
	out = post(b, &discordgo.Message{ID: "m4", ChannelID: "100", Content: "plain"}, false)
	require.Len(t, out, 1)
	assert.Equal(t, "plain", out[0].Content)

	// threads off: thread messages are not relayed, the channel still is
	b.UpdateConfig(func(c *Config) { c.DiscordThreads = false })
	assert.Empty(t, post(b, &discordgo.Message{ID: "m5", ChannelID: "200", Content: "hidden"}, false))
	assert.Len(t, post(b, &discordgo.Message{ID: "m6", ChannelID: "100", Content: "shown"}, false), 1)
}

func TestThreadSystemMessagesSkipped(t *testing.T) {
	b := newThreadTestBridge(t)
	assert.Empty(t, post(b, &discordgo.Message{ID: "s1", ChannelID: "100", Type: discordgo.MessageTypeThreadCreated, Content: "Server move plan"}, false))
	assert.Empty(t, post(b, &discordgo.Message{ID: "s2", ChannelID: "200", Type: discordgo.MessageTypeThreadStarterMessage}, false))
}

func TestMessageExtras(t *testing.T) {
	b := newThreadTestBridge(t)

	// sticker-only: no empty line, just the sticker
	out := post(b, &discordgo.Message{ID: "x1", ChannelID: "100",
		StickerItems: []*discordgo.StickerItem{{Name: "wave"}}}, false)
	require.Len(t, out, 1)
	assert.Equal(t, "[sticker: wave]", out[0].Content)
	assert.False(t, out[0].IsAction)

	// rich embeds (bots) are described; link previews are not
	out = post(b, &discordgo.Message{ID: "x2", ChannelID: "100", Content: "see https://example.com",
		Embeds: []*discordgo.MessageEmbed{
			{Type: discordgo.EmbedTypeLink, URL: "https://example.com", Title: "Example"},
			{Type: discordgo.EmbedTypeRich, Title: "Level up!", Description: "bob reached\nlevel 5", URL: "https://mee6.xyz/x"},
		}}, false)
	require.Len(t, out, 2)
	assert.Equal(t, "see https://example.com", out[0].Content)
	assert.Equal(t, "[embed] Level up! — bob reached level 5 https://mee6.xyz/x", out[1].Content)

	// polls, in a thread: tagged
	out = post(b, &discordgo.Message{ID: "x3", ChannelID: "200", Poll: &discordgo.Poll{
		Question: discordgo.PollMedia{Text: "Pizza?"},
		Answers:  []discordgo.PollAnswer{{Media: &discordgo.PollMedia{Text: "yes"}}, {Media: &discordgo.PollMedia{Text: "no"}}},
	}}, false)
	require.Len(t, out, 1)
	assert.Equal(t, "[Server move plan] [poll] Pizza? (yes / no)", out[0].Content)

	// attachment-only: just the URL, no empty line before it
	out = post(b, &discordgo.Message{ID: "x4", ChannelID: "100",
		Attachments: []*discordgo.MessageAttachment{{URL: "https://cdn.discordapp.com/a.png"}}}, false)
	require.Len(t, out, 1)
	assert.Equal(t, "https://cdn.discordapp.com/a.png", out[0].Content)

	// edits never re-send extras or attachments
	out = post(b, &discordgo.Message{ID: "x1", ChannelID: "100",
		StickerItems: []*discordgo.StickerItem{{Name: "wave"}}}, true)
	assert.Empty(t, out)
}

func TestThreadCreateAnnounced(t *testing.T) {
	b := newThreadTestBridge(t)
	d := b.discord
	require.NoError(t, d.Session.State.MemberAdd(&discordgo.Member{GuildID: "guild", User: bob, Nick: "Bobby"}))

	create := func(id, parent, owner, name string, newly bool) []*DiscordMessage {
		d.onThreadCreate(d.Session, &discordgo.ThreadCreate{
			Channel:      &discordgo.Channel{ID: id, GuildID: "guild", ParentID: parent, OwnerID: owner, Name: name, Type: discordgo.ChannelTypeGuildPublicThread},
			NewlyCreated: newly,
		})
		return drain(b)
	}

	out := create("300", "100", "u1", "  New   idea ", true)
	require.Len(t, out, 1)
	assert.Equal(t, "started a thread: New idea", out[0].Content)
	assert.True(t, out[0].IsAction)
	assert.Equal(t, "100", out[0].ChannelID)
	assert.Equal(t, "u1", out[0].Author.ID)

	assert.Empty(t, create("301", "100", "u1", "old thread", false), "only newly created threads")
	assert.Empty(t, create("302", "900", "u1", "elsewhere", true), "only threads in mapped channels")
	assert.Empty(t, create("303", "100", "bot", "ours", true), "not threads the bot made")

	b.UpdateConfig(func(c *Config) { c.DiscordThreads = false })
	assert.Empty(t, create("304", "100", "u1", "disabled", true))
}

func TestPrefixLinesAndTag(t *testing.T) {
	assert.Equal(t, "[a b] x\n[a b] y", prefixLines("x\ny", threadTag(" a \n b ")))
	assert.Equal(t, "x", prefixLines("x", ""))
}
