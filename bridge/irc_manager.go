package bridge

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mozillazg/go-unidecode"
	"github.com/pkg/errors"

	ircnick "github.com/qaisjp/go-discord-irc/irc/nick"
	"github.com/qaisjp/go-discord-irc/irc/varys"
	irc "github.com/qaisjp/go-ircevent"
	log "github.com/sirupsen/logrus"
)

// DevMode is a hack
var DevMode = false

// How long generateNickname waits for an ISON reply before falling back to
// only checking the channels the listener can see.
const isonTimeout = 2 * time.Second

// IRCManager owns the puppet connections.
//
// Connections are created, updated and closed only from the bridge loop
// goroutine. The maps (and each connection's discord/nick fields) are also
// read, and occasionally updated, from IRC callback and Discord handler
// goroutines, so they are guarded by mu. Never hold mu while doing network
// I/O or calling another method that takes it.
type IRCManager struct {
	mu             sync.RWMutex
	ircConnections map[string]*ircConnection // keyed by Discord user ID
	puppetNicks    map[string]*ircConnection // keyed by IRC nick

	bridge *Bridge
	varys  varys.Client
}

// cooldownExpiry is delivered to the bridge loop when a cooldown timer fires.
type cooldownExpiry struct {
	con *ircConnection
	gen uint64
}

// NewIRCManager creates a new IRCManager
func newIRCManager(bridge *Bridge) (*IRCManager, error) {
	conf := bridge.Config
	m := &IRCManager{
		ircConnections: make(map[string]*ircConnection),
		puppetNicks:    make(map[string]*ircConnection),
		bridge:         bridge,
	}

	// Set up varys
	m.varys = varys.NewMemClient()
	err := m.varys.Setup(varys.SetupParams{
		UseTLS:             !conf.NoTLS,
		InsecureSkipVerify: conf.InsecureSkipVerify,

		Server:         conf.IRCServer,
		ServerPassword: conf.IRCServerPass,
		WebIRCPassword: conf.WebIRCPass,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to set up params: %w", err)
	}

	// Sync back state, if there is any
	discordToNicks, err := m.varys.GetUIDToNicks()
	if err != nil {
		return nil, fmt.Errorf("failed to get discordToNicks: %w", err)
	}
	m.ircConnections = make(map[string]*ircConnection, len(discordToNicks))
	m.puppetNicks = make(map[string]*ircConnection, len(discordToNicks))
	for discord, nick := range discordToNicks {
		con := newIRCConnection(m, DiscordUser{ID: discord}, nick)
		m.ircConnections[discord] = con
		m.puppetNicks[nick] = con
	}

	return m, nil
}

// --- locked accessors -------------------------------------------------------

func (m *IRCManager) connByUID(uid string) (*ircConnection, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	con, ok := m.ircConnections[uid]
	return con, ok
}

func (m *IRCManager) connCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.ircConnections)
}

func (m *IRCManager) allConnections() []*ircConnection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cons := make([]*ircConnection, 0, len(m.ircConnections))
	for _, con := range m.ircConnections {
		cons = append(cons, con)
	}
	return cons
}

// nickForUID returns the IRC nick of the puppet for a Discord user, if any.
func (m *IRCManager) nickForUID(uid string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	con, ok := m.ircConnections[uid]
	if !ok {
		return "", false
	}
	return con.nick, true
}

func (m *IRCManager) isPuppetNick(nick string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.puppetNicks[nick]
	return ok
}

// puppetNickOwner finds the puppet using nick, compared case-insensitively.
func (m *IRCManager) puppetNickOwner(nick string) (*ircConnection, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if con, ok := m.puppetNicks[nick]; ok {
		return con, true
	}
	for n, con := range m.puppetNicks {
		if strings.EqualFold(n, nick) {
			return con, true
		}
	}
	return nil, false
}

// mentionReplacements returns old/new pairs turning puppet nicks into
// Discord mentions, for use with strings.NewReplacer.
func (m *IRCManager) mentionReplacements() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	replacements := make([]string, 0, len(m.ircConnections)*2)
	for _, con := range m.ircConnections {
		replacements = append(replacements, con.nick, "<@!"+con.uid+">")
	}
	return replacements
}

// setConnectionNick records the nick the server actually gave a puppet.
func (m *IRCManager) setConnectionNick(con *ircConnection, nick string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ircConnections[con.uid] != con {
		return // connection was closed in the meantime
	}
	if con.nick != nick && m.puppetNicks[con.nick] == con {
		delete(m.puppetNicks, con.nick)
	}
	con.nick = nick
	m.puppetNicks[nick] = con
}

// renamePuppetNick follows a NICK change seen by the listener.
func (m *IRCManager) renamePuppetNick(oldNick, newNick string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if con, ok := m.puppetNicks[oldNick]; ok {
		m.puppetNicks[newNick] = con
		delete(m.puppetNicks, oldNick)
		con.nick = newNick
	}
}

// forgetPuppetNickIfDisconnected drops nick from the puppet list if its
// connection is really gone (rather than e.g. a CHGHOST fake quit).
func (m *IRCManager) forgetPuppetNickIfDisconnected(nick string) {
	m.mu.RLock()
	con, ok := m.puppetNicks[nick]
	m.mu.RUnlock()
	if !ok || con.Connected() {
		return
	}

	m.mu.Lock()
	if m.puppetNicks[nick] == con {
		delete(m.puppetNicks, nick)
	}
	m.mu.Unlock()
}

// --- connection lifecycle (bridge loop goroutine only) ----------------------

// CloseConnection shuts down a particular connection and its channels.
func (m *IRCManager) CloseConnection(i *ircConnection) {
	_, nick := i.snapshot()
	log.WithField("nick", nick).Println("Closing connection.")
	m.stopCooldown(i)

	m.mu.Lock()
	if m.ircConnections[i.uid] == i {
		delete(m.ircConnections, i.uid)
	}
	for n, con := range m.puppetNicks {
		if con == i {
			delete(m.puppetNicks, n)
		}
	}
	remaining := len(m.ircConnections)
	m.mu.Unlock()

	i.close()

	if DevMode {
		fmt.Println("Decrementing total connections. It's now", remaining)
	}

	if err := m.varys.QuitIfConnected(i.uid, i.quitMessage); err != nil {
		log.WithError(err).WithFields(log.Fields{"discord": i.uid}).Errorln("failed to quit")
	}
}

// Close closes all of an IRCManager's connections.
func (m *IRCManager) Close() {
	for _, con := range m.allConnections() {
		m.CloseConnection(con)
	}
}

func (m *IRCManager) stopCooldown(con *ircConnection) {
	if con.cooldownTimer != nil {
		con.cooldownTimer.Stop()
		con.cooldownTimer = nil
	}
	// Invalidate any expiry that already fired and is waiting in the loop.
	con.cooldownGen++
}

// SetConnectionCooldown renews/starts a timer for expiring a connection.
func (m *IRCManager) SetConnectionCooldown(con *ircConnection) {
	_, nick := con.snapshot()
	if con.cooldownTimer != nil {
		log.WithField("nick", nick).Println("IRC connection cooldownTimer stopped!")
	}
	m.stopCooldown(con)

	gen := con.cooldownGen
	expired := m.bridge.cooldownExpiredChan
	con.cooldownTimer = time.AfterFunc(
		m.bridge.Config.CooldownDuration,
		func() {
			// Hand the expiry to the bridge loop instead of touching
			// the manager from the timer goroutine.
			expired <- cooldownExpiry{con: con, gen: gen}
		},
	)

	log.WithField("nick", nick).Println("IRC connection cooldownTimer created...")
}

// expireCooldown closes a connection whose cooldown ran out, unless the
// cooldown was cancelled or renewed after the timer fired.
func (m *IRCManager) expireCooldown(ev cooldownExpiry) {
	con := ev.con
	if con.cooldownTimer == nil || ev.gen != con.cooldownGen {
		return
	}
	if current, ok := m.connByUID(con.uid); !ok || current != con {
		return
	}

	_, nick := con.snapshot()
	log.WithField("nick", nick).Println("IRC connection expired by cooldownTimer...")
	m.CloseConnection(con)
}

// DisconnectUser immediately disconnects a Discord user if it exists
func (m *IRCManager) DisconnectUser(userID string) {
	con, ok := m.connByUID(userID)
	if !ok {
		return
	}
	m.CloseConnection(con)
}

var connectionsIgnored = 0

func (m *IRCManager) ircIgnoredDiscord(user string) bool {
	_, ret := m.bridge.Config.DiscordIgnores[user]
	return ret
}

// HandleUser deals with messages sent from a DiscordUser
//
// When `user.Online == false`, we make `user.ID` the only other data present in discord.handlePresenceUpdate
func (m *IRCManager) HandleUser(user DiscordUser) {
	if m.ircIgnoredDiscord(user.ID) {
		return
	}

	// If we have an allowed list of users at all
	if allowed := m.bridge.Config.DiscordAllowed; allowed != nil {
		// Short-circuit if they aren't in the list
		if _, ok := allowed[user.ID]; !ok {
			return
		}
	}

	// Does the user exist on the IRC side?
	if con, ok := m.connByUID(user.ID); ok {
		// Close the connection if they are not
		// online on Discord anymore (after cooldown)
		if !user.Online {
			m.SetConnectionCooldown(con)
			con.SetAway("offline on discord")
		} else {
			// The user is online, destroy any connection cooldown.
			if con.cooldownTimer != nil {
				log.WithField("nick", user.Nick).Println("Destroying connection cooldown.")
				m.stopCooldown(con)
				con.SetAway("")
			}
		}

		// If user.Nick is empty then we probably just had a status change
		if user.Nick == "" {
			return
		}

		// Update their nickname / username
		// Note: this event is still called when their status is changed
		//       from `online` to `dnd` (online related states)
		//       In UpdateDetails we handle nickname changes so it is
		//       OK to call the below potentially redundant function
		con.UpdateDetails(user)
		return
	}

	// Migrated (unique-username) accounts have discriminator "0", which is
	// fine. Only a missing username means we got a partial user.
	if user.Username == "" {
		// If they are not online, we don't care, because this was likely an offline event
		if !user.Online {
			return
		}

		log.WithFields(log.Fields{
			"err":                errors.WithStack(errors.New("Username is empty")).Error(),
			"user.Username":      user.Username,
			"user.Discriminator": user.Discriminator,
			"user.ID":            user.ID,
		}).Println("ignoring a HandleUser (in irc_manager.go)")
		return
	}

	count := m.connCount()

	// DEV MODE: Only create a connection if it sounds like qaisjp or if we have 10 connections
	if DevMode {
		if count > 4 && !strings.Contains(user.Username, "qais") {
			connectionsIgnored++
			return
		}
	}

	// Don't connect them if we're over our configured connection limit! (Includes our listener)
	if m.bridge.Config.ConnectionLimit > 0 && count+1 >= m.bridge.Config.ConnectionLimit {
		return
	}

	nick := m.generateNickname(user, true)
	username := m.generateUsername(user)

	var ip string
	{
		baseip := "fd75:f5f5:226f:"
		if user.Bot {
			baseip += "2"
		} else {
			baseip += "1"
		}
		ip = SnowflakeToIP(baseip, user.ID)
	}

	hostname := user.ID
	if user.Bot {
		hostname += ".bot.discord"
	} else {
		hostname += ".user.discord"
	}

	con := newIRCConnection(m, user, nick)
	con.quitMessage = fmt.Sprintf("Offline for %s", m.bridge.Config.CooldownDuration)

	m.mu.Lock()
	m.ircConnections[user.ID] = con
	m.puppetNicks[nick] = con
	total := len(m.ircConnections)
	m.mu.Unlock()

	if DevMode {
		fmt.Println("Incrementing total connections. It's now", total)
	}

	err := m.varys.Connect(varys.ConnectParams{
		UID: user.ID,

		Nick:     nick,
		Username: username,
		RealName: user.Username,

		WebIRCSuffix: fmt.Sprintf("discord %s %s", hostname, ip),

		Callbacks: map[string]func(*irc.Event){
			"001":     safeCallback("puppet 001", con.OnWelcome),
			"PRIVMSG": safeCallback("puppet PRIVMSG", con.OnPrivateMessage),
		},
	})
	if err != nil {
		log.WithError(err).WithField("nick", nick).Errorln("error opening irc connection")
		// Don't leave a dead entry behind, or this user could never
		// get a puppet again until the bridge restarts.
		m.CloseConnection(con)
		return
	}
}

// Converts a nickname to a sanitised form.
// Does not check IRC or Discord existence, so don't use this method
// unless you're also checking IRC and Discord.
func sanitiseNickname(nick string) string {
	if nick == "" {
		log.Warnln(errors.WithStack(errors.New("trying to sanitise an empty nick")))
		return "_"
	}

	// Unidecode the nickname — we make sure it's not empty to prevent "🔴🔴" becoming ""
	if newnick := unidecode.Unidecode(nick); newnick != "" {
		nick = newnick
	}

	// https://github.com/lp0/charybdis/blob/9ced2a7932dddd069636fe6fe8e9faa6db904703/ircd/client.c#L854-L884
	if nick[0] == '-' {
		nick = "_" + nick
	}
	if ircnick.IsDigit(nick[0]) {
		nick = "_" + nick
	}

	newNick := []byte(nick)

	// Replace bad characters with underscores
	for i, c := range []byte(nick) {
		if !ircnick.IsNickChar(c) || ircnick.IsFakeNickChar(c) {
			newNick[i] = ' '
		}
	}

	// Now every invalid character has been replaced with a space (just some invalid character)
	// Lets replace each sequence of invalid characters with a single underscore
	newNick = invalidNickRun.ReplaceAllLiteral(newNick, []byte{'_'})

	return string(newNick)
}

var invalidNickRun = regexp.MustCompile(` +`)

func (m *IRCManager) maxNickLength() int {
	if n := m.bridge.Config.MaxNickLength; n > 0 {
		return n
	}
	return ircnick.MAXLENGTH
}

// nickInUseByOther reports whether nick is taken by anyone other than the
// Discord user uid's own puppet.
//
// When checkNetwork is set, the whole network is asked with ISON, so users
// who are not in any of our channels are found too. That costs a round trip,
// so it is only used when creating or renaming puppets.
func (m *IRCManager) nickInUseByOther(nick, uid string, checkNetwork bool) bool {
	if owner, ok := m.puppetNickOwner(nick); ok {
		return owner.uid != uid
	}

	if checkNetwork {
		online, err := m.bridge.ircListener.IsOnIRC(nick, isonTimeout)
		if err == nil {
			return online
		}
		log.WithError(err).WithField("nick", nick).Debugln("ISON failed, falling back to channel check")
	}

	return m.bridge.ircListener.DoesUserExist(nick)
}

func (m *IRCManager) generateNickname(discord DiscordUser, checkNetwork bool) string {
	nick := sanitiseNickname(discord.Nick)
	suffix := m.bridge.Config.Suffix
	newNick := nick + suffix
	maxLength := m.maxNickLength()

	useFallback := len(newNick) > maxLength || m.nickInUseByOther(newNick, discord.ID, checkNetwork)

	if !useFallback && m.bridge.discord.nameClashesWithMember(discord.ID, nick) {
		useFallback = true
	}

	if useFallback {
		username := sanitiseNickname(discord.Username)
		suffix = m.bridge.Config.Separator + nickDisambiguator(discord) + suffix

		// Maximum length of a username but without the suffix
		length := maxLength - len(suffix)
		if length < 1 {
			length = 1
		}
		if length > len(username) {
			length = len(username)
		}

		newNick = username[:length] + suffix
		if len(newNick) > maxLength {
			newNick = newNick[:maxLength]
		}
	}

	return newNick
}

// SendMessage sends a broken down Discord Message to a particular IRC channel.
func (m *IRCManager) SendMessage(channel string, msg *DiscordMessage) {
	if msg.Author == nil || m.ircIgnoredDiscord(msg.Author.ID) {
		return
	}

	con, ok := m.connByUID(msg.Author.ID)

	content := msg.Content

	channel = strings.Split(channel, " ")[0]

	// Person is appearing offline (or the bridge is running in Simple Mode)
	if !ok {
		listener := m.bridge.ircListener
		prefix := fmt.Sprintf("<%s> ", discordTag(breakHighlight(msg.Author.Username), msg.Author.Discriminator))
		budget := ircMessageBudget(channel, len(listener.GetNick()), false) - len(prefix)

		for _, line := range strings.Split(content, "\n") {
			if m.isFilteredDiscordMessage(line) {
				continue
			}
			for _, chunk := range splitIRCMessage(line, budget) {
				listener.Privmsg(channel, prefix+chunk)
			}
		}
		return
	}

	// If there is a cooldown, reset the cooldown
	if con.cooldownTimer != nil {
		m.SetConnectionCooldown(con)
	}

	_, nick := con.snapshot()

	for _, line := range strings.Split(content, "\n") {
		if m.isFilteredDiscordMessage(line) {
			continue
		}

		text := line
		isAction := msg.IsAction
		if strings.HasPrefix(line, "/me ") && len(line) > 4 {
			isAction = true
			text = line[4:]
		}

		budget := ircMessageBudget(channel, len(nick), isAction)
		for _, chunk := range splitIRCMessage(text, budget) {
			con.enqueue(IRCMessage{
				IRCChannel: channel,
				Message:    chunk,
				IsAction:   isAction,
			})
		}
	}
}

// RequestChannels finds all the Discord channels this user belongs to,
// and then find pairings in the global pairings list
// Currently just returns all participating IRC channels
// TODO (?)
func (m *IRCManager) RequestChannels(userID string) []Mapping {
	return m.bridge.Mappings()
}

func (m *IRCManager) isIgnoredHostmask(mask string) bool {
	for _, ban := range m.bridge.Config.IRCIgnores {
		if ban.Match(mask) {
			return true
		}
	}
	return false
}

func (m *IRCManager) isFilteredIRCMessage(txt string) bool {
	for _, ban := range m.bridge.Config.IRCFilteredMessages {
		if ban.Match(txt) {
			return true
		}
	}
	return false
}

func (m *IRCManager) isFilteredDiscordMessage(txt string) bool {
	for _, ban := range m.bridge.Config.DiscordFilteredMessages {
		if ban.Match(txt) {
			return true
		}
	}
	return false
}

func (m *IRCManager) generateUsername(discordUser DiscordUser) string {
	if len(m.bridge.Config.PuppetUsername) > 0 {
		return m.bridge.Config.PuppetUsername
	}
	return sanitiseNickname(discordUser.Username)
}

// safeCallback wraps an IRC callback so a panic in it is logged instead of
// killing the bridge (go-ircevent runs callbacks without recovering).
func safeCallback(name string, fn func(*irc.Event)) func(*irc.Event) {
	return func(e *irc.Event) {
		defer recoverAndLog(name)
		fn(e)
	}
}
