package bridge

import (
	"strings"
	"sync"
	"time"

	irc "github.com/qaisjp/go-ircevent"
	log "github.com/sirupsen/logrus"
)

// joinFailureCodes are the numerics a server sends when a JOIN is refused.
var joinFailureCodes = []string{
	"403", // ERR_NOSUCHCHANNEL
	"405", // ERR_TOOMANYCHANNELS
	"437", // ERR_UNAVAILRESOURCE (channel juped / temporarily unavailable)
	"471", // ERR_CHANNELISFULL (+l)
	"473", // ERR_INVITEONLYCHAN (+i)
	"474", // ERR_BANNEDFROMCHAN (+b)
	"475", // ERR_BADCHANNELKEY (+k)
	"477", // ERR_NEEDREGGEDNICK (+r, must be logged in)
	"489", // ERR_SECUREONLYCHAN / voice needed, depending on ircd
}

// joinTracker records which channels a connection is in, as confirmed by
// the server, and the last join error per channel (to avoid log spam).
type joinTracker struct {
	mu      sync.Mutex
	joined  map[string]bool
	lastErr map[string]string
}

func newJoinTracker() *joinTracker {
	return &joinTracker{joined: map[string]bool{}, lastErr: map[string]string{}}
}

func channelKey(channel string) string { return strings.ToLower(channel) }

func (t *joinTracker) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.joined = map[string]bool{}
	t.lastErr = map[string]string{}
}

func (t *joinTracker) markJoined(channel string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.joined[channelKey(channel)] = true
	delete(t.lastErr, channelKey(channel))
}

func (t *joinTracker) markParted(channel string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.joined, channelKey(channel))
}

// noteError records a join failure and reports whether it differs from the
// last failure seen for that channel (i.e. whether it is worth a warning).
func (t *joinTracker) noteError(channel, reason string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := channelKey(channel)
	if t.lastErr[key] == reason {
		return false
	}
	t.lastErr[key] = reason
	return true
}

// missing returns the mappings whose IRC channel we are not currently in.
func (t *joinTracker) missing(mappings []Mapping) []Mapping {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Mapping
	for _, m := range mappings {
		if !t.joined[channelKey(m.IRCChannel)] {
			out = append(out, m)
		}
	}
	return out
}

func isOwnEvent(e *irc.Event, nick string) bool {
	return e.Connection != nil && strings.EqualFold(nick, e.Connection.GetNick())
}

// joinTrackingCallbacks returns IRC callbacks that keep t up to date and log
// refused joins for mapped channels. who names the connection in logs.
func (b *Bridge) joinTrackingCallbacks(t *joinTracker, who func() string) map[string]func(*irc.Event) {
	cbs := map[string]func(*irc.Event){
		"JOIN": func(e *irc.Event) {
			if len(e.Arguments) > 0 && isOwnEvent(e, e.Nick) {
				t.markJoined(e.Arguments[0])
			}
		},
		"PART": func(e *irc.Event) {
			if len(e.Arguments) > 0 && isOwnEvent(e, e.Nick) {
				t.markParted(e.Arguments[0])
			}
		},
		"KICK": func(e *irc.Event) {
			if len(e.Arguments) > 1 && isOwnEvent(e, e.Arguments[1]) {
				t.markParted(e.Arguments[0])
			}
		},
	}

	for _, code := range joinFailureCodes {
		code := code
		cbs[code] = func(e *irc.Event) {
			// <code> <our nick> <channel> :<reason>
			if len(e.Arguments) < 2 {
				return
			}
			channel := e.Arguments[1]
			if _, mapped := b.GetMappingByIRC(channel); !mapped {
				return
			}
			reason := e.Message()

			entry := log.WithFields(log.Fields{
				"who":     who(),
				"channel": channel,
				"code":    code,
				"reason":  reason,
			})
			if t.noteError(channel, code+" "+reason) {
				entry.Warnln("Could not join IRC channel")
			} else {
				entry.Debugln("Could not join IRC channel (again)")
			}
		}
	}

	return cbs
}

// rejoinMissingChannels sends one JOIN for the mapped channels t says we are
// not in. send must not block on a dead connection.
func (b *Bridge) rejoinMissingChannels(t *joinTracker, who string, send func(string)) {
	missing := t.missing(b.Mappings())
	if len(missing) == 0 {
		return
	}
	cmd := b.GetJoinCommand(missing)
	if cmd == "" {
		return
	}

	names := make([]string, 0, len(missing))
	for _, m := range missing {
		names = append(names, m.IRCChannel)
	}
	log.WithFields(log.Fields{"who": who, "channels": names}).Debugln("Retrying join for missing channels")
	send(cmd)
}

// rejoinLoop periodically retries channels that the listener or a puppet
// failed to join (bans lifted, keys fixed, services back up, ...).
func (b *Bridge) rejoinLoop(interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			func() {
				defer recoverAndLog("rejoin loop")
				b.rejoinAll()
			}()
		}
	}
}

func (b *Bridge) rejoinAll() {
	if l := b.ircListener; l.isWelcomed() && l.Connected() {
		b.rejoinMissingChannels(l.joins, "listener "+l.GetNick(), l.SendRaw)
	}

	for _, con := range b.ircManager.allConnections() {
		if !con.isWelcomed() {
			continue
		}
		_, nick := con.snapshot()
		b.rejoinMissingChannels(con.joins, "puppet "+nick, con.SendRaw)
	}
}

// deafMode returns the user mode letter that makes a client deaf to channel
// messages: the server's DEAF ISUPPORT token if it advertises one
// (charybdis-style ircds use D), otherwise d (ircu, Nefarious, Unreal,
// InspIRCd).
func (b *Bridge) deafMode() string {
	if b.ircListener != nil {
		if feat, ok := b.ircListener.GetFeature("DEAF"); ok {
			feat.Lock()
			value := feat.Value
			feat.Unlock()
			if value != "" {
				return value
			}
		}
	}
	return "d"
}

// expandPrejoin fills in ${DEAF} in prejoin commands. ${NICK} is filled in
// later, per connection.
func (b *Bridge) expandPrejoin(commands []string) []string {
	if len(commands) == 0 {
		return commands
	}
	deaf := b.deafMode()
	out := make([]string, len(commands))
	for i, c := range commands {
		out[i] = strings.ReplaceAll(c, "${DEAF}", deaf)
	}
	return out
}
