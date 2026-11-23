package bridge

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	irc "github.com/qaisjp/go-ircevent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestBridge(t *testing.T) *Bridge {
	t.Helper()
	b := &Bridge{
		Config: &Config{
			GuildID:          "guild",
			Suffix:           "_d2",
			Separator:        "_",
			MaxNickLength:    30,
			CooldownDuration: time.Hour,
		},
		updateUserChan:           make(chan DiscordUser, 16),
		discordMessagesChan:      make(chan IRCMessage, 16),
		discordMessageEventsChan: make(chan *DiscordMessage, 16),
		cooldownExpiredChan:      make(chan cooldownExpiry, 16),
		emoji:                    map[string]*discordgo.Emoji{},
	}

	state := discordgo.NewState()
	require.NoError(t, state.GuildAdd(&discordgo.Guild{ID: "guild"}))
	b.discord = &discordBot{
		Session: &discordgo.Session{State: state},
		bridge:  b,
		guildID: "guild",
		relayed: newMessageCache(100),
		topics:  newTopicSyncer(),
	}
	b.ircListener = &ircListener{
		Connection:          irc.IRC("listener", "discord"),
		bridge:              b,
		listenerCallbackIDs: map[string]int{},
	}

	m, err := newIRCManager(b)
	require.NoError(t, err)
	b.ircManager = m
	return b
}

// Bug 1: action detection used to index the raw content with the parsed
// content's length and could panic.
func TestSplitAction(t *testing.T) {
	cases := []struct {
		raw, want string
		action    bool
	}{
		{"_waves_", "waves", true},
		{"_<@123456789012345678>_", "<@123456789012345678>", true},
		{"_hi <@123456789012345678>_", "hi <@123456789012345678>", true},
		{"hello", "hello", false},
		{"__", "__", false},
		{"_", "_", false},
		{"", "", false},
		{"_not closed", "_not closed", false},
	}
	for _, c := range cases {
		got, action := splitAction(c.raw)
		assert.Equal(t, c.want, got, c.raw)
		assert.Equal(t, c.action, action, c.raw)
	}
}

// Bug 5: long Discord messages must be split into IRC-sized chunks.
func TestSplitIRCMessage(t *testing.T) {
	assert.Equal(t, []string{"short"}, splitIRCMessage("short", 100))
	assert.Equal(t, []string{""}, splitIRCMessage("", 100))

	words := strings.Repeat("lorem ipsum dolor ", 100)
	chunks := splitIRCMessage(words, 120)
	require.Greater(t, len(chunks), 1)
	for _, c := range chunks {
		assert.LessOrEqual(t, len(c), 120)
	}
	assert.Equal(t, words, strings.Join(chunks, " "))

	// No spaces, multi-byte characters: never split a rune.
	accents := strings.Repeat("é", 300) // 600 bytes
	chunks = splitIRCMessage(accents, 101)
	require.Greater(t, len(chunks), 1)
	for _, c := range chunks {
		assert.True(t, utf8.ValidString(c))
		assert.LessOrEqual(t, len(c), 101)
	}
	assert.Equal(t, accents, strings.Join(chunks, ""))
}

func TestIRCMessageBudgetFitsLine(t *testing.T) {
	nick := strings.Repeat("n", 30)
	target := "#some-channel"
	for _, action := range []bool{false, true} {
		budget := ircMessageBudget(target, len(nick), action)
		text := strings.Repeat("x", budget)
		if action {
			text = "\x01ACTION " + text + "\x01"
		}
		line := fmt.Sprintf(":%s!%s@%s PRIVMSG %s :%s",
			nick, strings.Repeat("u", ircMaxUserLen), strings.Repeat("h", ircMaxHostLen), target, text)
		assert.LessOrEqual(t, len(line)+2, 512, "action=%v", action)
	}
}

// Bug 7: highlight-breaking must not split a multi-byte character.
func TestBreakHighlight(t *testing.T) {
	got := breakHighlight("Émile")
	assert.True(t, utf8.ValidString(got))
	assert.Equal(t, "É​mile", got)
	assert.Equal(t, "", breakHighlight(""))
	assert.Equal(t, "a​bc", breakHighlight("abc"))
}

// Bug 6: migrated Discord accounts have discriminator "0".
func TestDiscordTagAndDisambiguator(t *testing.T) {
	assert.Equal(t, "bob#1234", discordTag("bob", "1234"))
	assert.Equal(t, "bob", discordTag("bob", "0"))
	assert.Equal(t, "bob", discordTag("bob", ""))

	assert.Equal(t, "1234", nickDisambiguator(DiscordUser{ID: "998877665544332211", Discriminator: "1234"}))
	assert.Equal(t, "2211", nickDisambiguator(DiscordUser{ID: "998877665544332211", Discriminator: "0"}))
}

func TestGenerateNicknameFallback(t *testing.T) {
	b := newTestBridge(t)
	m := b.ircManager

	// Another member whose name sanitises to the same nick forces a fallback.
	require.NoError(t, b.discord.Session.State.MemberAdd(&discordgo.Member{
		GuildID: "guild",
		User:    &discordgo.User{ID: "1", Username: "someoneelse"},
		Nick:    "Bob",
	}))

	migrated := DiscordUser{ID: "998877665544332211", Username: "bob.smith", Discriminator: "0", Nick: "bob"}
	assert.Equal(t, "bob_smith_2211_d2", m.generateNickname(migrated, false))

	legacy := DiscordUser{ID: "998877665544332211", Username: "bobsmith", Discriminator: "4321", Nick: "bob"}
	assert.Equal(t, "bobsmith_4321_d2", m.generateNickname(legacy, false))

	// The fallback respects max_nick_length rather than a hardcoded 30.
	b.Config.MaxNickLength = 12
	long := DiscordUser{ID: "998877665544332211", Username: "averyverylongusername", Discriminator: "0", Nick: "averyverylongnickname"}
	got := m.generateNickname(long, false)
	assert.LessOrEqual(t, len(got), 12)
	assert.True(t, strings.HasSuffix(got, "_2211_d2"), got)
}

// Bug 7: no bare JOIN/PART when there is nothing to join.
func TestGetJoinCommand(t *testing.T) {
	b := newTestBridge(t)
	assert.Equal(t, "", b.GetJoinCommand(nil))

	require.NoError(t, b.SetChannelMappings(map[string]string{
		"#open":       "1",
		"#locked key": "2",
	}))
	assert.Equal(t, "JOIN #locked,#open key", b.GetJoinCommand(b.Mappings()))

	require.NoError(t, b.SetChannelMappingsForTest(map[string]string{"#open": "1"}))
	assert.Equal(t, "JOIN #open", b.GetJoinCommand(b.Mappings()))
}

// SetChannelMappingsForTest replaces mappings without the live part/join
// side effects, which need real IRC connections.
func (b *Bridge) SetChannelMappingsForTest(in map[string]string) error {
	b.mappingsMu.Lock()
	b.mappings = nil
	b.mappingsMu.Unlock()
	return b.SetChannelMappings(in)
}

// Bug 3: enqueueing after (or while) a connection closes must not panic.
func TestEnqueueAfterClose(t *testing.T) {
	b := newTestBridge(t)
	con := newIRCConnection(b.ircManager, DiscordUser{ID: "1"}, "bob_d2")

	// Fill the queue so later sends take the background path.
	for i := 0; i < messageQueueSize+10; i++ {
		con.enqueue(IRCMessage{IRCChannel: "#c", Message: "hi"})
	}
	con.close()
	con.close() // idempotent
	for i := 0; i < 10; i++ {
		con.enqueue(IRCMessage{IRCChannel: "#c", Message: "late"})
	}
	time.Sleep(50 * time.Millisecond) // let background senders observe done
}

// Bug 2: a cooldown that was renewed or cancelled after its timer fired
// must not close the connection.
func TestCooldownExpiryGenerations(t *testing.T) {
	b := newTestBridge(t)
	m := b.ircManager
	con := newIRCConnection(m, DiscordUser{ID: "1"}, "bob_d2")
	m.mu.Lock()
	m.ircConnections["1"] = con
	m.puppetNicks["bob_d2"] = con
	m.mu.Unlock()

	m.SetConnectionCooldown(con)
	stale := cooldownExpiry{con: con, gen: con.cooldownGen}
	m.SetConnectionCooldown(con) // renewed

	m.expireCooldown(stale)
	_, ok := m.connByUID("1")
	assert.True(t, ok, "stale expiry must be ignored")

	current := cooldownExpiry{con: con, gen: con.cooldownGen}
	m.stopCooldown(con) // user came back online
	m.expireCooldown(current)
	_, ok = m.connByUID("1")
	assert.True(t, ok, "expiry after cancel must be ignored")

	m.SetConnectionCooldown(con)
	m.expireCooldown(cooldownExpiry{con: con, gen: con.cooldownGen})
	_, ok = m.connByUID("1")
	assert.False(t, ok, "live expiry closes the connection")
	assert.False(t, m.isPuppetNick("bob_d2"))
}

// Bug 2: run with -race. Readers on other goroutines while the "loop"
// goroutine creates, renames and closes connections.
func TestManagerConcurrentAccess(t *testing.T) {
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
				_ = m.mentionMap()
				_ = m.isPuppetNick("user5_d2")
				_, _ = m.nickForUID("5")
				_, _ = m.puppetNickOwner("USER5_D2")
				m.renamePuppetNick("user7_d2", "user7_d2_")
				m.renamePuppetNick("user7_d2_", "user7_d2")
				for _, con := range m.allConnections() {
					_, _ = con.snapshot()
				}
				_ = b.Mappings()
				_, _ = b.lookupEmoji("x")
			}
		}()
	}

	for round := 0; round < 50; round++ {
		var cons []*ircConnection
		for i := 0; i < 10; i++ {
			id := fmt.Sprint(i)
			con := newIRCConnection(m, DiscordUser{ID: id, Nick: "user" + id}, "user"+id+"_d2")
			m.mu.Lock()
			m.ircConnections[id] = con
			m.puppetNicks[con.nick] = con
			m.mu.Unlock()
			m.setConnectionNick(con, "user"+id+"_d2")
			con.enqueue(IRCMessage{IRCChannel: "#c", Message: "hi"})
			cons = append(cons, con)
		}
		b.setEmoji(map[string]*discordgo.Emoji{"x": {Name: "x"}})
		for _, con := range cons {
			m.CloseConnection(con)
		}
	}

	close(stop)
	wg.Wait()
	assert.Equal(t, 0, m.connCount())
}
