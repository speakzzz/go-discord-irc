package bridge

import (
	"crypto/tls"
	"fmt"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gobwas/glob"
	"github.com/pkg/errors"
	"github.com/qaisjp/go-discord-irc/irc/varys"
	irc "github.com/qaisjp/go-ircevent"
	log "github.com/sirupsen/logrus"
)

// Config to be passed to New
type Config struct {
	AvatarURL                string
	DiscordBotToken, GuildID string

	// Map from Discord to IRC
	ChannelMappings map[string]string

	IRCServer       string
	Discriminator   string
	IRCServerPass   string
	IRCListenerName string // i.e, "DiscordBot", required to listen for messages in all cases
	WebIRCPass      string
	PuppetUsername  string // Username to connect to IRC with
	IRCIgnores      []glob.Glob
	DiscordIgnores  map[string]struct{} // Discord user IDs to not bridge
	DiscordAllowed  map[string]struct{} // Discord user IDs to only bridge
	ConnectionLimit int                 // number of IRC connections we can spawn

	IRCPuppetPrejoinCommands   []string
	IRCListenerPrejoinCommands []string

	// filters
	IRCFilteredMessages     []glob.Glob
	DiscordFilteredMessages []glob.Glob

	// NoTLS constrols whether to use TLS at all when connecting to the IRC server
	NoTLS bool

	// InsecureSkipVerify controls whether a client verifies the
	// server's certificate chain and host name.
	// If InsecureSkipVerify is true, TLS accepts any certificate
	// presented by the server and any host name in that certificate.
	// In this mode, TLS is susceptible to man-in-the-middle attacks.
	// This should be used only for testing.
	InsecureSkipVerify bool

	// SimpleMode, when enabled, will ensure that IRCManager not spawn
	// an IRC connection for each of the online Discord users.
	SimpleMode bool

	Suffix    string // Suffix is the suffix to append to IRC puppets
	Separator string // Separator is used in IRC puppets' username, in fallback situations, between the discriminator and username.

	// CooldownDuration is the duration in seconds for an IRC puppet to stay online before being disconnected
	CooldownDuration time.Duration

	// ShowJoinQuit determines whether or not to show JOIN, QUIT, KICK messages on Discord
	ShowJoinQuit bool

	// Maximum Nicklength for irc server
	MaxNickLength int

	// RejoinInterval is how often to retry joining mapped channels that the
	// listener or a puppet is not in. Zero disables retrying.
	RejoinInterval time.Duration

	Debug         bool
	DebugPresence bool
}

// A Bridge represents a bridging between an IRC server and channels in a Discord server
type Bridge struct {
	// Config holds the bridge settings. Settings that can change while the
	// bridge runs (filters, ignore/allow lists, avatar URL, debug, listener
	// name) must be changed with UpdateConfig and read with the
	// matching accessor, because they are read from many goroutines.
	Config   *Config
	configMu sync.RWMutex

	discord     *discordBot
	ircListener *ircListener
	ircManager  *IRCManager

	// mappings and ircChannelKeys are replaced on config reload (from the
	// config watcher goroutine) and read everywhere, so guard them.
	mappingsMu     sync.RWMutex
	mappings       []Mapping
	ircChannelKeys map[string]string // From "#test" to "password"

	done chan bool

	// stopRejoin stops the rejoin loop when the bridge shuts down.
	stopRejoin chan struct{}

	discordMessagesChan      chan IRCMessage
	discordMessageEventsChan chan *DiscordMessage
	updateUserChan           chan DiscordUser
	removeUserChan           chan string // user id
	cooldownExpiredChan      chan cooldownExpiry

	emojiMu sync.RWMutex
	emoji   map[string]*discordgo.Emoji
}

// UpdateConfig applies a change to the reloadable settings in Config while
// holding the config lock. Use it instead of assigning Config fields directly
// once the bridge is running.
func (b *Bridge) UpdateConfig(fn func(c *Config)) {
	b.configMu.Lock()
	defer b.configMu.Unlock()
	fn(b.Config)
}

// readConfig runs fn with the reloadable settings read-locked.
func (b *Bridge) readConfig(fn func(c *Config)) {
	b.configMu.RLock()
	defer b.configMu.RUnlock()
	fn(b.Config)
}

func (b *Bridge) avatarURL() (url string) {
	b.readConfig(func(c *Config) { url = c.AvatarURL })
	return
}

// Mappings returns a copy of the current channel mappings.
func (b *Bridge) Mappings() []Mapping {
	b.mappingsMu.RLock()
	defer b.mappingsMu.RUnlock()
	return append([]Mapping(nil), b.mappings...)
}

func (b *Bridge) setEmoji(emoji map[string]*discordgo.Emoji) {
	b.emojiMu.Lock()
	b.emoji = emoji
	b.emojiMu.Unlock()
}

func (b *Bridge) lookupEmoji(name string) (*discordgo.Emoji, bool) {
	b.emojiMu.RLock()
	defer b.emojiMu.RUnlock()
	e, ok := b.emoji[name]
	return e, ok
}

// recoverAndLog is deferred at the top of event handlers so that one bad
// event is logged instead of crashing the whole bridge.
func recoverAndLog(where string) {
	if r := recover(); r != nil {
		log.WithFields(log.Fields{
			"handler": where,
			"panic":   r,
		}).Errorln("recovered from panic in event handler")
		log.Errorln(string(debug.Stack()))
	}
}

// Close the Bridge
func (b *Bridge) Close() {
	b.done <- true
	<-b.done
}

// TODO: Use errors package
func (b *Bridge) load(opts *Config) error {
	if opts.IRCServer == "" {
		return errors.New("missing server name")
	}

	if err := b.SetChannelMappings(opts.ChannelMappings); err != nil {
		return errors.Wrap(err, "channel mappings could not be set")
	}

	// This should not be used anymore!
	opts.ChannelMappings = nil

	return nil
}

// SetChannelMappings allows you to set (or update) the
// hashmap containing irc to discord mappings.
//
// Calling this function whilst the bot is running will
// add or remove IRC bots accordingly.
func (b *Bridge) SetChannelMappings(inMappings map[string]string) error {
	var mappings []Mapping
	ircChannelKeys := make(map[string]string, len(mappings))
	for irc, discord := range inMappings {
		ircParts := strings.Split(irc, " ")
		ircChannel := ircParts[0]
		if parts := len(ircParts); parts != 1 && parts > 2 {
			log.Errorf("IRC channel irc %+v (to discord %+v) is invalid. Expected 0 or 1 spaces in the string. Ignoring.", irc, discord)
			continue
		} else if parts == 2 {
			ircChannelKeys[ircChannel] = ircParts[1]
		}

		mappings = append(mappings, Mapping{
			DiscordChannel: discord,
			IRCChannel:     ircChannel,
		})
	}

	// Check for duplicate channels
	for i, mapping := range mappings {
		for j, check := range mappings {
			if (mapping.DiscordChannel == check.DiscordChannel) || (mapping.IRCChannel == check.IRCChannel) {
				if i != j {
					return errors.New("channel_mappings contains duplicate entries")
				}
			}
		}
	}

	b.mappingsMu.Lock()
	oldMappings := b.mappings
	b.mappings = mappings
	b.ircChannelKeys = ircChannelKeys
	b.mappingsMu.Unlock()

	// If doing some changes mid-bot
	if oldMappings != nil {
		var newMappings []Mapping
		var removedMappings []Mapping

		// Find positive difference
		// These are the items in the new mappings list, but not the oldMappings
		for _, mapping := range mappings {
			found := false
			for _, curr := range oldMappings {
				if curr == mapping {
					found = true
					break
				}
			}

			if !found {
				newMappings = append(newMappings, mapping)
			}
		}

		// Find negative difference
		// These are the items in the oldMappings, but not the new one
		for _, mapping := range oldMappings {
			found := false
			for _, curr := range mappings {
				if curr == mapping {
					found = true
					break
				}
			}

			if !found {
				removedMappings = append(removedMappings, mapping)
			}
		}

		// The bots needs to leave the remove mappings
		rmChannels := []string{}
		for _, mapping := range removedMappings {
			// Looking for the irc channel to remove
			// inside our list of newly added channels.
			//
			// This will prevent swaps from joinquitting the bots.
			found := false
			for _, curr := range newMappings {
				if curr.IRCChannel == mapping.IRCChannel {
					found = true
				}
			}

			// If we've not found this channel to remove in the new channels
			// actually part the channel
			if !found {
				rmChannels = append(rmChannels, mapping.IRCChannel)
			}
		}

		if len(rmChannels) > 0 {
			part := "PART " + strings.Join(rmChannels, ",")
			b.ircListener.SendRaw(part)
			if err := b.ircManager.varys.SendRaw("", varys.InterpolationParams{}, part); err != nil {
				log.WithError(err).Errorln("could not part puppets from removed channels")
			}
		}

		// The bots needs to join the new mappings
		b.ircListener.JoinChannels()
		for _, conn := range b.ircManager.allConnections() {
			conn.JoinChannels()
		}
	}

	return nil
}

// New Bridge
func New(conf *Config) (*Bridge, error) {
	dib := &Bridge{
		Config:     conf,
		done:       make(chan bool),
		stopRejoin: make(chan struct{}),

		discordMessagesChan:      make(chan IRCMessage),
		discordMessageEventsChan: make(chan *DiscordMessage),
		updateUserChan:           make(chan DiscordUser),
		removeUserChan:           make(chan string),
		cooldownExpiredChan:      make(chan cooldownExpiry),

		emoji: make(map[string]*discordgo.Emoji),
	}

	if err := dib.load(conf); err != nil {
		return nil, errors.Wrap(err, "configuration invalid")
	}

	var err error

	dib.discord, err = newDiscord(dib, conf.DiscordBotToken, conf.GuildID)
	if err != nil {
		return nil, errors.Wrap(err, "Could not create discord bot")
	}

	dib.ircListener = newIRCListener(dib, conf.WebIRCPass)
	if dib.ircManager, err = newIRCManager(dib); err != nil {
		return nil, fmt.Errorf("failed to create ircManager: %w", err)
	}

	go dib.loop()

	return dib, nil
}

// SetIRCListenerName changes the username of the listener bot.
func (b *Bridge) SetIRCListenerName(name string) {
	b.UpdateConfig(func(c *Config) { c.IRCListenerName = name })
	b.ircListener.Nick(name)
}

// SetDebugMode allows you to control debug logging.
func (b *Bridge) SetDebugMode(debug bool) {
	b.UpdateConfig(func(c *Config) { c.Debug = debug })
	b.ircListener.SetDebugMode(debug)
}

// Open all the connections required to run the bridge
func (b *Bridge) Open() (err error) {

	// Open a websocket connection to Discord and begin listening.
	err = b.discord.Open()
	if err != nil {
		return errors.Wrap(err, "can't open discord")
	}

	err = b.ircListener.Connect(b.Config.IRCServer)
	if err != nil {
		return errors.Wrap(err, "can't open irc connection")
	}

	// run listener loop
	go b.ircListener.Loop()

	// retry channels we fail to join
	if b.Config.RejoinInterval > 0 {
		go b.rejoinLoop(b.Config.RejoinInterval, b.stopRejoin)
	}

	return
}

// SetupIRCConnection sets up an IRC connection with config settings like
// UseTLS, InsecureSkipVerify, and WebIRCPass.
func (b *Bridge) SetupIRCConnection(con *irc.Connection, hostname, ip string) {
	if !b.Config.NoTLS {
		con.UseTLS = true
		con.TLSConfig = &tls.Config{
			InsecureSkipVerify: b.Config.InsecureSkipVerify,
		}
	}

	// On kick, rejoin the channel
	con.AddCallback("KICK", func(e *irc.Event) {
		if e.Arguments[1] == con.GetNick() {
			con.Join(e.Arguments[0])
		}
	})

	con.Password = b.Config.IRCServerPass

	if b.Config.WebIRCPass != "" {
		con.WebIRC = fmt.Sprintf("%s discord %s %s", b.Config.WebIRCPass, hostname, ip)
	}
}

// GetJoinCommand produces a JOIN command based on the provided mappings.
// It returns an empty string if there is nothing to join.
func (b *Bridge) GetJoinCommand(mappings []Mapping) string {
	var channels, keyedChannels, keys []string

	b.mappingsMu.RLock()
	defer b.mappingsMu.RUnlock()

	for _, mapping := range mappings {
		channel := mapping.IRCChannel
		key, keyed := b.ircChannelKeys[channel]

		if keyed {
			keyedChannels = append(keyedChannels, channel)
			keys = append(keys, key)
		} else {
			channels = append(channels, channel)
		}
	}

	// Just append normal channels to the end of keyed channels
	keyedChannels = append(keyedChannels, channels...)

	if len(keyedChannels) == 0 {
		return ""
	}

	cmd := "JOIN " + strings.Join(keyedChannels, ",")
	if len(keys) > 0 {
		cmd += " " + strings.Join(keys, ",")
	}
	return cmd
}

// GetMappingByIRC returns a Mapping for a given IRC channel.
// Returns nil if a Mapping does not exist.
func (b *Bridge) GetMappingByIRC(channel string) (Mapping, bool) {
	for _, mapping := range b.Mappings() {
		if strings.EqualFold(mapping.IRCChannel, channel) {
			return mapping, true
		}
	}
	return Mapping{}, false
}

// GetMappingByDiscord returns a Mapping for a given Discord channel.
// Returns nil if a Mapping does not exist.
func (b *Bridge) GetMappingByDiscord(channel string) (Mapping, bool) {
	for _, mapping := range b.Mappings() {
		if mapping.DiscordChannel == channel {
			return mapping, true
		}
	}
	return Mapping{}, false
}

var emojiRegex = regexp.MustCompile("(:[a-zA-Z_-]+:)")

func (b *Bridge) loop() {
	for {
		select {

		// Messages from IRC to Discord
		case msg := <-b.discordMessagesChan:
			mapping, ok := b.GetMappingByIRC(msg.IRCChannel)

			if !ok {
				log.Warnln("Ignoring message sent from an unhandled IRC channel.")
				continue
			}

			var avatar string
			username := msg.Username

			// System messages have no username
			if username != "" {
				avatar = b.discord.GetAvatar(b.Config.GuildID, msg.Username)
				if avatar == "" {
					// If we don't have a Discord avatar, generate an adorable avatar
					avatar = strings.ReplaceAll(b.avatarURL(), "${USERNAME}", msg.Username)
				}

				if len(username) == 1 {
					// Append usernames with 1 character
					// This is because Discord doesn't accept single character usernames
					username += `.` // <- zero width space in here, ayylmao
				}
			}

			content := msg.Message

			// If the message has leading or trailing spaces, or if the message consists
			// entirely of whitespace, we want Discord to display them as intended,
			// rather than ignoring it. We surround the content with zero-width spaces
			// to achieve this. For example, 3 space characters sent from IRC should
			// render on Discord as 3 space characters too.
			if content == "" || strings.TrimSpace(content) != content {
				content = "\u200B" + content + "\u200B"
			}

			// Convert any emoji ye?
			content = emojiRegex.ReplaceAllStringFunc(content, func(emoji string) string {
				e, ok := b.lookupEmoji(strings.ToLower(emoji[1 : len(emoji)-1]))
				if !ok {
					return emoji
				}

				emoji = ":" + e.Name + ":" + e.ID
				if e.Animated {
					emoji = "a" + emoji
				}

				return "<" + emoji + ">"
			})

			if username == "" {
				// System messages come straight from the bot
				if _, err := b.discord.Session.ChannelMessageSend(mapping.DiscordChannel, content); err != nil {
					log.WithError(err).WithFields(log.Fields{
						"msg.channel":  mapping.DiscordChannel,
						"msg.username": username,
						"msg.content":  content,
					}).Errorln("could not transmit SYSTEM message to discord")
				}
			} else {
				go func() {
					_, err := b.discord.transmitter.Send(
						mapping.DiscordChannel,
						&discordgo.WebhookParams{
							Username:  username,
							AvatarURL: avatar,
							Content:   content,
							AllowedMentions: &discordgo.MessageAllowedMentions{
								// Allow user and role mentions, but not everyone or here mentions
								Parse: []discordgo.AllowedMentionType{
									discordgo.AllowedMentionTypeRoles,
									discordgo.AllowedMentionTypeUsers,
								},
							},
						},
					)

					if err != nil {
						log.WithFields(log.Fields{
							"error":        err,
							"msg.channel":  mapping.DiscordChannel,
							"msg.username": username,
							"msg.avatar":   avatar,
							"msg.content":  content,
						}).Errorln("could not transmit message to discord")
					}
				}()
			}

		// Messages from Discord to IRC
		case msg := <-b.discordMessageEventsChan:
			mapping, ok := b.GetMappingByDiscord(msg.ChannelID)

			// Do not do anything if we do not have a mapping for the PUBLIC channel
			if !ok && msg.PmTarget == "" {
				// log.Warnln("Ignoring message sent from an unhandled Discord channel.")
				continue
			}

			target := msg.PmTarget
			if target == "" {
				target = mapping.IRCChannel
			}

			b.ircManager.SendMessage(target, msg)

		// Notification to potentially update, or create, a user
		// We should not receive anything on this channel if we're in Simple Mode
		case user := <-b.updateUserChan:
			b.ircManager.HandleUser(user)

		case userID := <-b.removeUserChan:
			b.ircManager.DisconnectUser(userID)

		// A puppet's offline cooldown ran out
		case ev := <-b.cooldownExpiredChan:
			b.ircManager.expireCooldown(ev)

		// Done!
		case <-b.done:
			close(b.stopRejoin)
			b.discord.Close()
			b.ircListener.Quit()
			b.ircManager.Close()
			close(b.done)

			return
		}

	}
}
