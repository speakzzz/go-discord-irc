package bridge

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/qaisjp/go-discord-irc/irc/varys"
	irc "github.com/qaisjp/go-ircevent"
	log "github.com/sirupsen/logrus"
)

// messageQueueSize is how many outgoing lines a puppet buffers before
// SendMessage has to hand off to a goroutine.
const messageQueueSize = 128

// An ircConnection should only ever communicate with its manager
// Refer to `(m *IRCManager) HandleUser` to see how these are spawned
type ircConnection struct {
	// uid is the Discord user ID. It never changes, so it is safe to read
	// from any goroutine without locking.
	uid string

	// discord and nick are guarded by manager.mu, because IRC callbacks and
	// Discord handlers read them from other goroutines.
	discord DiscordUser
	nick    string

	// quitMessage, cooldownTimer and cooldownGen are only touched from the
	// bridge loop goroutine.
	quitMessage   string
	cooldownTimer *time.Timer
	cooldownGen   uint64

	// messages is never closed. Senders and the send loop select on done
	// instead, so a late send can never panic.
	messages  chan IRCMessage
	done      chan struct{}
	closeOnce sync.Once
	startOnce sync.Once

	manager *IRCManager

	// PM state, guarded by pmMu (PRIVMSG callbacks run concurrently).
	pmMu             sync.Mutex
	pmDiscordChannel string // channel ID for their discord channel for PMs
	pmNoticed        bool   // Tell users this feature is in beta
	pmNoticedSenders map[string]struct{}
}

func newIRCConnection(m *IRCManager, user DiscordUser, nick string) *ircConnection {
	return &ircConnection{
		uid:              user.ID,
		discord:          user,
		nick:             nick,
		messages:         make(chan IRCMessage, messageQueueSize),
		done:             make(chan struct{}),
		manager:          m,
		pmNoticedSenders: make(map[string]struct{}),
	}
}

// close stops the send loop and makes any pending enqueue give up.
func (i *ircConnection) close() {
	i.closeOnce.Do(func() { close(i.done) })
}

// snapshot returns a consistent copy of the fields guarded by manager.mu.
func (i *ircConnection) snapshot() (DiscordUser, string) {
	i.manager.mu.RLock()
	defer i.manager.mu.RUnlock()
	return i.discord, i.nick
}

// enqueue queues a line to be sent by this puppet. It never blocks the
// caller, and never panics if the connection is closed concurrently.
func (i *ircConnection) enqueue(msg IRCMessage) {
	select {
	case i.messages <- msg:
	case <-i.done:
	default:
		// Queue is full (e.g. we are not welcomed yet). Wait in the
		// background rather than stalling the bridge loop.
		go func() {
			select {
			case i.messages <- msg:
			case <-i.done:
			}
		}()
	}
}

func (i *ircConnection) sendLoop() {
	for {
		select {
		case m := <-i.messages:
			msg := m.Message
			if m.IsAction {
				msg = fmt.Sprintf("\001ACTION %s\001", msg)
			}
			i.Privmsg(m.IRCChannel, msg)
		case <-i.done:
			return
		}
	}
}

func (i *ircConnection) GetNick() string {
	nick, err := i.manager.varys.GetNick(i.uid)
	if err != nil {
		log.WithError(err).WithField("discord", i.uid).Errorln("could not get puppet nick")
		return ""
	}
	return nick
}

func (i *ircConnection) Connected() bool {
	connected, err := i.manager.varys.Connected(i.uid)
	if err != nil {
		log.WithError(err).WithField("discord", i.uid).Errorln("could not get puppet connection state")
		return false
	}
	return connected
}

func (i *ircConnection) OnWelcome(e *irc.Event) {
	// execute puppet prejoin commands
	err := i.manager.varys.SendRaw(i.uid, varys.InterpolationParams{Nick: true}, i.manager.bridge.Config.IRCPuppetPrejoinCommands...)
	if err != nil {
		log.WithError(err).WithField("discord", i.uid).Errorln("could not send puppet prejoin commands")
	}

	i.JoinChannels()

	// just in case NickServ, Q:Lines, or otherwise force our nick to be not what we expect!
	if nick := i.GetNick(); nick != "" {
		i.manager.setConnectionNick(i, nick)
	}

	// OnWelcome runs again after every reconnect; only start one sender.
	i.startOnce.Do(func() { go i.sendLoop() })
}

func (i *ircConnection) JoinChannels() {
	if cmd := i.manager.bridge.GetJoinCommand(i.manager.RequestChannels(i.uid)); cmd != "" {
		i.SendRaw(cmd)
	}
}

// UpdateDetails must only be called from the bridge loop goroutine.
func (i *ircConnection) UpdateDetails(discord DiscordUser) {
	current, _ := i.snapshot()

	if current.Username != discord.Username {
		i.quitMessage = fmt.Sprintf("Changing real name from %s to %s", current.Username, discord.Username)
		i.manager.CloseConnection(i)

		// After one second make the user reconnect, via the bridge loop.
		// This should be enough time for the nick tracker to update.
		updates := i.manager.bridge.updateUserChan
		time.AfterFunc(time.Second, func() {
			updates <- discord
		})
		return
	}

	// if their details haven't changed, don't do anything
	if (current.Nick == discord.Nick) && (current.Discriminator == discord.Discriminator) {
		return
	}

	newNick := i.manager.generateNickname(discord, true)

	i.manager.mu.Lock()
	i.discord = discord
	if i.manager.puppetNicks[i.nick] == i {
		delete(i.manager.puppetNicks, i.nick)
	}
	i.nick = newNick
	i.manager.puppetNicks[newNick] = i
	i.manager.mu.Unlock()

	if err := i.manager.varys.Nick(i.uid, newNick); err != nil {
		log.WithError(err).WithField("discord", i.uid).Errorln("could not change puppet nick")
	}
}

// introducePM makes sure a DM channel exists and returns its ID.
func (i *ircConnection) introducePM(nick string) (string, bool) {
	d := i.manager.bridge.discord

	i.pmMu.Lock()
	defer i.pmMu.Unlock()

	if i.pmDiscordChannel == "" {
		c, err := d.Session.UserChannelCreate(i.uid)
		if err != nil {
			// todo: sentry
			log.Warnln("Could not create private message room", i.uid, err)
			return "", false
		}
		i.pmDiscordChannel = c.ID
	}

	if !i.pmNoticed {
		i.pmNoticed = true
		_, err := d.Session.ChannelMessageSend(
			i.pmDiscordChannel,
			fmt.Sprintf("To reply type: `%s@%s, your message here`", nick, i.manager.bridge.Config.Discriminator))
		if err != nil {
			log.Warnln("Could not send pmNotice", i.uid, err)
			return "", false
		}
	}

	nick = strings.ToLower(nick)
	if _, ok := i.pmNoticedSenders[nick]; !ok {
		i.pmNoticedSenders[nick] = struct{}{}
	}

	return i.pmDiscordChannel, true
}

func (i *ircConnection) OnPrivateMessage(e *irc.Event) {
	// Ignored hostmasks
	if i.manager.isIgnoredHostmask(e.Source) {
		return
	}

	if len(e.Arguments) == 0 || e.Arguments[0] == "" {
		return
	}

	// Alert private messages
	if e.Arguments[0][0] != '#' {
		if e.Message() == "help" {
			i.Privmsg(e.Nick, "Commands: help, who")
		} else if e.Message() == "who" {
			user, _ := i.snapshot()
			i.Privmsg(e.Nick, fmt.Sprintf("I am: %s with ID %s", discordTag(user.Nick, user.Discriminator), user.ID))
		}

		d := i.manager.bridge.discord

		channelID, ok := i.introducePM(e.Nick)
		if !ok {
			return
		}

		msg := fmt.Sprintf(
			"%s,%s - %s@%s: %s", e.Connection.Server, e.Source,
			e.Nick, i.manager.bridge.Config.Discriminator, e.Message())
		_, err := d.Session.ChannelMessageSend(channelID, msg)
		if err != nil {
			log.Warnln("Could not send PM", i.uid, err)
			return
		}
		return
	}

	// GTANet does not support deafness so the below logmsg has been disabled
	// log.Println("Non listener IRC connection received PRIVMSG from channel. Something went wrong.")
}

func (i *ircConnection) SendRaw(message string) {
	if err := i.manager.varys.SendRaw(i.uid, varys.InterpolationParams{}, message); err != nil {
		log.WithError(err).WithField("discord", i.uid).Errorln("could not send raw IRC line")
	}
}

func (i *ircConnection) SetAway(status string) {
	i.SendRaw(fmt.Sprintf("AWAY :%s", status))
}

func (i *ircConnection) Privmsg(target, message string) {
	// SendRaw appends the CRLF itself.
	i.SendRaw(fmt.Sprintf("PRIVMSG %s :%s", target, message))
}
