package bridge

import (
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/gobwas/glob"
	irc "github.com/qaisjp/go-ircevent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJoinTracker(t *testing.T) {
	tr := newJoinTracker()
	mappings := []Mapping{{IRCChannel: "#evilnet"}, {IRCChannel: "#norge"}}

	assert.Len(t, tr.missing(mappings), 2)

	tr.markJoined("#EvilNET") // server may echo a different case
	assert.Equal(t, []Mapping{{IRCChannel: "#norge"}}, tr.missing(mappings))

	tr.markParted("#evilnet")
	assert.Len(t, tr.missing(mappings), 2)

	assert.True(t, tr.noteError("#norge", "474 banned"))
	assert.False(t, tr.noteError("#norge", "474 banned"), "same error twice is not news")
	assert.True(t, tr.noteError("#norge", "473 invite only"))
	tr.markJoined("#norge")
	assert.True(t, tr.noteError("#norge", "473 invite only"), "a join clears the last error")

	tr.markJoined("#evilnet")
	tr.reset()
	assert.Len(t, tr.missing(mappings), 2)
}

// The JOIN/PART/KICK/numeric callbacks keep the tracker in step with what
// the server says, and only for our own nick and mapped channels.
func TestJoinTrackingCallbacks(t *testing.T) {
	b := newTestBridge(t)
	require.NoError(t, b.SetChannelMappings(map[string]string{"#evilnet": "1", "#norge": "2"}))

	conn := irc.IRC("bridge", "discord")
	tr := newJoinTracker()
	cbs := b.joinTrackingCallbacks(tr, func() string { return "test" })

	fire := func(code, nick string, args ...string) {
		cbs[code](&irc.Event{Code: code, Nick: nick, Arguments: args, Connection: conn})
	}

	fire("JOIN", "someoneelse", "#evilnet")
	assert.Len(t, tr.missing(b.Mappings()), 2, "other people's joins don't count")

	fire("JOIN", "bridge", "#evilnet")
	fire("JOIN", "bridge", "#norge")
	assert.Empty(t, tr.missing(b.Mappings()))

	fire("KICK", "op", "#norge", "bridge", "bye")
	assert.Equal(t, "#norge", tr.missing(b.Mappings())[0].IRCChannel)

	fire("PART", "bridge", "#evilnet")
	assert.Len(t, tr.missing(b.Mappings()), 2)

	fire("474", "", "bridge", "#evilnet", "Cannot join channel (+b)")
	assert.False(t, tr.noteError("#evilnet", "474 Cannot join channel (+b)"), "refusal was recorded")

	fire("474", "", "bridge", "#notmapped", "Cannot join channel (+b)")
	assert.True(t, tr.noteError("#notmapped", "474 Cannot join channel (+b)"), "unmapped channels are ignored")

	fire("474", "", "bridge") // malformed: must not panic
}

func TestRejoinMissingChannels(t *testing.T) {
	b := newTestBridge(t)
	require.NoError(t, b.SetChannelMappings(map[string]string{"#evilnet": "1", "#norge key": "2"}))

	tr := newJoinTracker()
	var sent []string
	send := func(s string) { sent = append(sent, s) }

	b.rejoinMissingChannels(tr, "test", send)
	assert.Equal(t, []string{"JOIN #norge,#evilnet key"}, sent)

	sent = nil
	tr.markJoined("#norge")
	b.rejoinMissingChannels(tr, "test", send)
	assert.Equal(t, []string{"JOIN #evilnet"}, sent)

	sent = nil
	tr.markJoined("#evilnet")
	b.rejoinMissingChannels(tr, "test", send)
	assert.Empty(t, sent, "nothing to do when we're in every channel")
}

func TestDeafModeAndPrejoin(t *testing.T) {
	b := newTestBridge(t)

	// ircu / Nefarious don't advertise DEAF: use d (their +D is privdeaf).
	assert.Equal(t, "d", b.deafMode())
	assert.Equal(t, []string{"MODE ${NICK} +d", "PRIVMSG X :hi"},
		b.expandPrejoin([]string{"MODE ${NICK} +${DEAF}", "PRIVMSG X :hi"}))

	// charybdis-style servers advertise DEAF=D.
	b.ircListener.SetupFeatureDetect()
	b.ircListener.RunCallbacks(&irc.Event{Code: "005", Arguments: []string{"bridge", "DEAF=D", "are supported by this server"}})
	assert.Equal(t, "D", b.deafMode())

	assert.Empty(t, b.expandPrejoin(nil))
}

func TestDisplayNames(t *testing.T) {
	legacy := &discordgo.User{Username: "bob", Discriminator: "1234"}
	migrated := &discordgo.User{Username: "bob.smith", GlobalName: "Bob Smith", Discriminator: "0"}

	assert.Equal(t, "bob", userDisplayName(legacy))
	assert.Equal(t, "Bob Smith", userDisplayName(migrated))

	assert.Equal(t, "Bobby", GetMemberNick(&discordgo.Member{Nick: "Bobby", User: migrated}))
	assert.Equal(t, "Bob Smith", GetMemberNick(&discordgo.Member{User: migrated}))
	assert.Equal(t, "bob", GetMemberNick(&discordgo.Member{User: legacy}))

	assert.Equal(t, "Bobby", messageAuthorName(&discordgo.Message{Author: migrated, Member: &discordgo.Member{Nick: "Bobby"}}))
	assert.Equal(t, "Bob Smith", messageAuthorName(&discordgo.Message{Author: migrated}))
	assert.Equal(t, "", messageAuthorName(&discordgo.Message{}))

	m := &discordgo.Member{Nick: "Bobby", User: migrated}
	for _, name := range []string{"Bobby", "Bob Smith", "bob.smith"} {
		assert.True(t, memberHasName(m, name, false), name)
	}
	assert.False(t, memberHasName(m, "bob smith", false))
	assert.True(t, memberHasName(m, "bob smith", true))
	assert.False(t, memberHasName(&discordgo.Member{User: legacy}, "", true), "empty names never match")
}

// Simple mode shows the Discord display name, and only adds the legacy
// #discriminator when the shown name is the real username.
func TestSimpleModePrefixUsesDisplayName(t *testing.T) {
	prefix := func(u *discordgo.User, member *discordgo.Member) string {
		return relayPrefix(&DiscordMessage{Message: &discordgo.Message{Author: u, Member: member}})
	}

	migrated := &discordgo.User{Username: "bob.smith", GlobalName: "Bob Smith", Discriminator: "0"}
	legacy := &discordgo.User{Username: "alice", Discriminator: "4321"}
	plain := &discordgo.User{Username: "carol", Discriminator: "0"}

	assert.Equal(t, "<B​ob Smith> ", prefix(migrated, nil))
	assert.Equal(t, "<B​obby> ", prefix(migrated, &discordgo.Member{Nick: "Bobby"}))
	assert.Equal(t, "<a​lice#4321> ", prefix(legacy, nil))
	assert.Equal(t, "<c​arol> ", prefix(plain, nil))
}

// Config changes from the file watcher race with readers on other goroutines.
// Run with -race.
func TestConfigReloadIsSafe(t *testing.T) {
	b := newTestBridge(t)
	m := b.ircManager

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				m.isIgnoredHostmask("nick!user@host")
				m.isFilteredIRCMessage("hello")
				m.isFilteredDiscordMessage("hello")
				m.ircIgnoredDiscord("1")
				m.discordAllowed("1")
				_ = b.avatarURL()
			}
		}()
	}

	for i := 0; i < 200; i++ {
		ignores := []glob.Glob{glob.MustCompile("*!*@host")}
		b.UpdateConfig(func(c *Config) {
			c.IRCIgnores = ignores
			c.IRCFilteredMessages = ignores
			c.DiscordFilteredMessages = ignores
			c.DiscordIgnores = map[string]struct{}{"1": {}}
			c.DiscordAllowed = map[string]struct{}{"2": {}}
			c.AvatarURL = "https://example.invalid/${USERNAME}"
		})
	}
	close(stop)
	wg.Wait()

	assert.True(t, m.isIgnoredHostmask("nick!user@host"))
	assert.True(t, m.ircIgnoredDiscord("1"))
	assert.False(t, m.discordAllowed("1"))
	assert.True(t, m.discordAllowed("2"))

	b.UpdateConfig(func(c *Config) { c.DiscordAllowed = nil })
	assert.True(t, m.discordAllowed("anyone"), "nil allow list allows everyone")
}
