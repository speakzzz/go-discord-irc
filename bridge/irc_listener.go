package bridge

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ircf "github.com/qaisjp/go-discord-irc/irc/format"
	irc "github.com/qaisjp/go-ircevent"
	log "github.com/sirupsen/logrus"
)

type ircListener struct {
	*irc.Connection
	bridge *Bridge

	listenerCallbackIDs map[string]int

	// isonMu serialises ISON queries so replies can't be mixed up.
	isonMu sync.Mutex
	// welcomed is set (atomically) once we have received 001.
	welcomed int32
}

func newIRCListener(dib *Bridge, webIRCPass string) *ircListener {
	irccon := irc.IRC(dib.Config.IRCListenerName, "discord")
	listener := &ircListener{
		Connection:          irccon,
		bridge:              dib,
		listenerCallbackIDs: make(map[string]int),
	}

	dib.SetupIRCConnection(irccon, "discord.", "fd75:f5f5:226f::")
	listener.SetDebugMode(dib.Config.Debug)

	// Nick tracker for nick tracking
	irccon.SetupNickTrack()

	// Welcome event
	irccon.AddCallback("001", safeCallback("listener 001", listener.OnWelcome))

	// Called when received channel names... essentially OnJoinChannel
	irccon.AddCallback("366", safeCallback("listener 366", listener.OnJoinChannel))
	irccon.AddCallback("PRIVMSG", safeCallback("listener PRIVMSG", listener.OnPrivateMessage))
	irccon.AddCallback("NOTICE", safeCallback("listener NOTICE", listener.OnPrivateMessage))
	irccon.AddCallback("CTCP_ACTION", safeCallback("listener CTCP_ACTION", listener.OnPrivateMessage))

	irccon.AddCallback("900", safeCallback("listener 900", func(e *irc.Event) {
		// Try to rejoin channels after authenticated with NickServ
		listener.JoinChannels()
	}))

	// we are assuming this will be posible to run independent of any
	// future NICK callbacks added, otherwise do it like the STQUIT callback
	listener.AddCallback("NICK", safeCallback("listener NICK", listener.nickTrackNick))

	// Note that this might override SetupNickTrack!
	listener.OnJoinQuitSettingChange()

	return listener
}

func (i *ircListener) nickTrackNick(event *irc.Event) {
	i.bridge.ircManager.renamePuppetNick(event.Nick, event.Message())
}

func (i *ircListener) OnNickRelayToDiscord(event *irc.Event) {
	// ignored hostmasks, or we're a puppet? no relay
	if i.bridge.ircManager.isIgnoredHostmask(event.Source) ||
		i.isPuppetNick(event.Nick) ||
		i.isPuppetNick(event.Message()) {
		return
	}

	oldNick := event.Nick
	newNick := event.Message()

	msg := IRCMessage{
		Username: "",
		Message:  fmt.Sprintf("_%s changed their nick to %s_", oldNick, newNick),
	}

	for _, m := range i.bridge.Mappings() {
		channel := m.IRCChannel
		if channelObj, ok := i.Connection.GetChannel(channel); ok {
			if _, ok := channelObj.GetUser(newNick); ok {
				msg.IRCChannel = channel
				i.bridge.discordMessagesChan <- msg
			}
		}
	}
}

func (i *ircListener) nickTrackPuppetQuit(e *irc.Event) {
	// Protect against HostServ changing nicks or ircd's with CHGHOST/CHGIDENT or similar
	// sending us a QUIT for a puppet nick only for it to rejoin right after.
	// The puppet nick won't see a true disconnection itself and thus will still see itself
	// as connected.
	i.bridge.ircManager.forgetPuppetNickIfDisconnected(e.Nick)
}

func (i *ircListener) OnJoinQuitSettingChange() {
	// always remove our listener callbacks
	for ev, id := range i.listenerCallbackIDs {
		i.RemoveCallback(ev, id)
		delete(i.listenerCallbackIDs, ev)
	}

	// we're either going to track quits, or track and relay said, so swap out the callback
	// based on which is in effect.
	if i.bridge.Config.ShowJoinQuit {
		i.listenerCallbackIDs["STNICK"] = i.AddCallback("STNICK", safeCallback("listener STNICK", i.OnNickRelayToDiscord))

		// KICK is not state tracked!
		callbacks := []string{"STJOIN", "STPART", "STQUIT", "KICK"}
		for _, cb := range callbacks {
			id := i.AddCallback(cb, safeCallback("listener "+cb, i.OnJoinQuitCallback))
			i.listenerCallbackIDs[cb] = id
		}
	} else {
		id := i.AddCallback("STQUIT", safeCallback("listener STQUIT", i.nickTrackPuppetQuit))
		i.listenerCallbackIDs["STQUIT"] = id
	}
}

func (i *ircListener) OnJoinQuitCallback(event *irc.Event) {
	// KICK needs a channel and a target; everything else needs a channel
	// (or, for QUIT, nothing).
	if event.Code == "KICK" && len(event.Arguments) < 2 {
		return
	}
	if event.Code != "STQUIT" && len(event.Arguments) < 1 {
		return
	}

	// This checks if the source of the event was from a puppet.
	if (event.Code == "KICK" && i.isPuppetNick(event.Arguments[1])) || i.isPuppetNick(event.Nick) {
		// since we replace the STQUIT callback we have to manage our puppet nicks when
		// this call back is active!
		if event.Code == "STQUIT" {
			i.nickTrackPuppetQuit(event)
		}
		return
	}

	// Ignored hostmasks
	if i.bridge.ircManager.isIgnoredHostmask(event.Source) {
		return
	}

	who := event.Nick
	message := event.Nick
	id := " (" + event.User + "@" + event.Host + ") "

	switch event.Code {
	case "STJOIN":
		message += " joined" + id
	case "STPART":
		message += " left" + id
		if len(event.Arguments) > 1 {
			message += ": " + event.Arguments[1]
		}
	case "STQUIT":
		message += " quit" + id

		reason := event.Nick
		if len(event.Arguments) == 1 {
			reason = event.Arguments[0]
		}
		message += "Quit: " + reason
	case "KICK":
		who = event.Arguments[1]
		message = event.Arguments[1] + " was kicked by " + event.Nick
		if len(event.Arguments) > 2 {
			message += ": " + event.Arguments[2]
		}
	}

	msg := IRCMessage{
		// IRCChannel: set on the fly
		Username: "",
		Message:  message,
	}

	if event.Code == "STQUIT" {
		// Notify channels that the user is in
		for _, m := range i.bridge.Mappings() {
			channel := m.IRCChannel
			channelObj, ok := i.Connection.GetChannel(channel)
			if !ok {
				log.WithField("channel", channel).WithField("who", who).Warnln("Trying to process QUIT. Channel not found in irc listener cache.")
				continue
			}
			if _, ok := channelObj.GetUser(who); !ok {
				continue
			}
			msg.IRCChannel = channel
			i.bridge.discordMessagesChan <- msg
		}
	} else {
		msg.IRCChannel = event.Arguments[0]
		i.bridge.discordMessagesChan <- msg
	}
}

// FIXME: the user might not be on any channel that we're in and that would
// lead to incorrect assumptions the user doesn't exist!
// Good way to check is to utilize ISON
func (i *ircListener) DoesUserExist(user string) bool {
	ret := false
	i.IterChannels(func(name string, ch *irc.Channel) {
		if !ret {
			_, ret = ch.GetUser(user)
		}
	})
	return ret
}

var errListenerNotReady = errors.New("listener is not connected")

// IsOnIRC asks the server (with ISON) whether nick is in use anywhere on the
// network, not just in channels we share.
func (i *ircListener) IsOnIRC(nick string, timeout time.Duration) (bool, error) {
	if atomic.LoadInt32(&i.welcomed) == 0 || !i.Connected() {
		return false, errListenerNotReady
	}

	i.isonMu.Lock()
	defer i.isonMu.Unlock()

	result := make(chan bool, 1)
	id := i.AddCallback("303", func(e *irc.Event) {
		online := false
		for _, n := range strings.Fields(e.Message()) {
			if strings.EqualFold(n, nick) {
				online = true
				break
			}
		}
		select {
		case result <- online:
		default:
		}
	})
	defer i.RemoveCallback("303", id)

	i.SendRaw("ISON " + nick)

	select {
	case online := <-result:
		return online, nil
	case <-time.After(timeout):
		return false, errors.New("ISON timed out")
	}
}

func (i *ircListener) SetDebugMode(debug bool) {
	// i.VerboseCallbackHandler = debug
	// i.Debug = debug
}

func (i *ircListener) OnWelcome(e *irc.Event) {
	atomic.StoreInt32(&i.welcomed, 1)

	// Execute prejoin commands
	for _, com := range i.bridge.Config.IRCListenerPrejoinCommands {
		i.SendRaw(strings.ReplaceAll(com, "${NICK}", i.GetNick()))
	}

	// Join all channels
	i.JoinChannels()
}

func (i *ircListener) JoinChannels() {
	if cmd := i.bridge.GetJoinCommand(i.bridge.Mappings()); cmd != "" {
		i.SendRaw(cmd)
	}
}

func (i *ircListener) OnJoinChannel(e *irc.Event) {
	if len(e.Arguments) > 1 {
		log.Infof("Listener has joined IRC channel %s.", e.Arguments[1])
	}
}

func (i *ircListener) isPuppetNick(nick string) bool {
	if i.GetNick() == nick {
		return true
	}
	return i.bridge.ircManager.isPuppetNick(nick)
}

func (i *ircListener) OnPrivateMessage(e *irc.Event) {
	// Ignore private messages
	if len(e.Arguments) == 0 || e.Arguments[0] == "" || e.Arguments[0][0] != '#' {
		// If you decide to extend this to respond to PMs, make sure
		// you do not respond to NOTICEs, see issue #50.
		return
	}

	if i.isPuppetNick(e.Nick) || // ignore msg's from our puppets
		i.bridge.ircManager.isIgnoredHostmask(e.Source) || //ignored hostmasks
		i.bridge.ircManager.isFilteredIRCMessage(e.Message()) { // filtered
		return
	}

	replacements := i.bridge.ircManager.mentionReplacements()

	msg := strings.NewReplacer(
		replacements...,
	).Replace(e.Message())

	if e.Code == "CTCP_ACTION" {
		msg = "_" + msg + "_"
	}

	msg = ircf.BlocksToMarkdown(ircf.Parse(msg))

	go func(e *irc.Event) {
		i.bridge.discordMessagesChan <- IRCMessage{
			IRCChannel: e.Arguments[0],
			Username:   e.Nick,
			Message:    msg,
		}
	}(e)
}
