package bridge

import (
	"fmt"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/42wim/matterbridge/bridge/discord/transmitter"
	"github.com/qaisjp/go-discord-irc/dstate"
	ircnick "github.com/qaisjp/go-discord-irc/irc/nick"

	"github.com/bwmarrin/discordgo"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
)

type discordBot struct {
	Session *discordgo.Session
	bridge  *Bridge

	guildID string

	transmitter *transmitter.Transmitter

	// relayed remembers what we last sent to IRC for recent messages, so
	// edits can be compared with it.
	relayed *messageCache

	// topics sets Discord channel topics from IRC.
	topics *topicSyncer
}

func newDiscord(bridge *Bridge, botToken, guildID string) (*discordBot, error) {

	// Create a new Discord session using the provided bot token.
	session, err := discordgo.New("Bot " + botToken)
	if err != nil {
		return nil, errors.Wrap(err, "discord, could not create new session")
	}
	session.StateEnabled = true

	discord := &discordBot{
		Session: session,
		bridge:  bridge,

		guildID: guildID,
		relayed: newMessageCache(2000),
		topics:  newTopicSyncer(),
	}

	// These events are all fired in separate goroutines
	discord.Session.AddHandler(discord.OnReady)
	discord.Session.AddHandler(discord.onMessageCreate)
	discord.Session.AddHandler(discord.onMessageUpdate)
	discord.Session.AddHandler(discord.onGuildEmojiUpdate)
	discord.Session.AddHandler(discord.onThreadCreate)

	if !bridge.Config.SimpleMode {
		discord.Session.AddHandler(discord.onMemberListChunk)
		discord.Session.AddHandler(discord.onMemberUpdate)
		discord.Session.AddHandler(discord.onMemberLeave)
		discord.Session.AddHandler(discord.OnPresencesReplace)
		discord.Session.AddHandler(discord.OnPresenceUpdate)
		discord.Session.AddHandler(discord.OnTypingStart)
		discord.Session.AddHandler(discord.OnMessageReactionAdd)
	}

	return discord, nil
}

func (d *discordBot) Open() error {
	d.transmitter = transmitter.New(d.Session, d.guildID, "irc-bridge", true)
	d.transmitter.Log = logrus.NewEntry(logrus.StandardLogger())
	if err := d.transmitter.RefreshGuildWebhooks(nil); err != nil {
		return fmt.Errorf("failed to refresh guild webhooks: %w", err)
	}

	d.Session.Identify.Intents = discordgo.MakeIntent(discordgo.IntentsAll)
	err := d.Session.Open()
	if err != nil {
		return errors.Wrap(err, "discord, could not open session")
	}

	return nil
}

func (d *discordBot) Close() error {
	return errors.Wrap(d.Session.Close(), "closing discord session")
}

// Returns `<@uid>` if a discord user or just `name` if a bot
func userToMention(u *discordgo.User) (mention string) {
	mention = u.Username
	if !u.Bot {
		mention = u.Mention()
	}
	return
}

// For spoiler colouring:
var spoilerPattern = regexp.MustCompile(`\|\|(.*?)\|\|`)
var colorCode = string(rune(3))

func (d *discordBot) publishMessage(s *discordgo.Session, m *discordgo.Message, wasEdit bool) {
	// Fix crash if these fields don't exist
	if m.Author == nil || s.State.User == nil {
		// todo: add sentry logging
		return
	}

	// Ignore all messages created by the bot itself
	if m.Author.ID == s.State.User.ID {
		return
	}

	// Ignore messages sent from our webhooks
	if d.transmitter.HasWebhook(m.Author.ID) {
		return
	}

	// Discord's own "started a thread" notices; onThreadCreate announces
	// threads instead.
	if isThreadSystemMessage(m) {
		return
	}

	// Work on a copy: m may be discordgo's cached state object, which other
	// goroutines read, and we rewrite Content and Mentions below.
	msgCopy := *m
	m = &msgCopy
	m.Mentions = append([]*discordgo.User(nil), m.Mentions...)

	// The message is an action if the raw content is wrapped in "_..._".
	// Decide this on the raw content and strip the underscores before any
	// rewriting, since ParseText changes the length of the content.
	var isAction bool
	m.Content, isAction = splitAction(m.Content)

	// If the message is "ping" reply with "Pong!"
	if m.Content == "ping" {
		_, err := s.ChannelMessageSend(m.ChannelID, "Pong!")
		if err != nil {
			log.Warningln("Could not respond to Discord ping message", err.Error())
		}
	}

	// Turn **bold**, *italic*, `code` etc. into IRC formatting. Done on the
	// raw content so nicks substituted for mentions below are never parsed
	// as markdown.
	if d.bridge.convertDiscordMarkdown() {
		m.Content = discordMarkdownToIRC(m.Content)
	}

	// HACK: this is before d.ParseText so that the existing <@uid> translation logic can be used
	if m.MessageReference != nil && m.MessageReference.ChannelID == m.ChannelID {
		prefix := "[reply]"
		msg, err := dstate.ChannelMessage(d.Session, m.MessageReference.ChannelID, m.MessageReference.MessageID)
		if err == nil && msg.Author != nil {
			prefix = userToMention(msg.Author) + ":"
			if !msg.Author.Bot {
				// HACK: theoretically could already be there, thereotically not a big problem
				m.Mentions = append(m.Mentions, msg.Author)
			}
		}
		m.Content = prefix + " " + m.Content
	}

	content := d.ParseText(m)

	if strings.Count(content, "||") >= 2 {
		content = spoilerPattern.ReplaceAllString(content, colorCode+"1,1$1"+colorCode)
	}

	// Messages in a thread (or forum post) go to the IRC channel of the
	// thread's parent, tagged with the thread's name.
	tag := ""
	if m.GuildID != "" {
		if _, mapped := d.bridge.GetMappingByDiscord(m.ChannelID); !mapped {
			if parentID, name, isThread := d.threadParent(m.ChannelID); isThread {
				if !d.bridge.bridgeDiscordThreads() {
					return
				}
				m.ChannelID = parentID
				tag = threadTag(name)
			}
		}
	}

	pmTarget := ""
	// Blank guild means that it's a PM
	if m.GuildID == "" {
		pmTarget, content = pmTargetFromContent(content, d.bridge.Config.Discriminator)
		// if the target could not be deduced. tell them this.
		switch pmTarget {
		case "":
			_, _ = d.Session.ChannelMessageSend(
				m.ChannelID,
				fmt.Sprintf(
					"Don't know who that is. Can't PM. Try 'name@%s, message here'",
					d.bridge.Config.Discriminator))
			return
		case "*UNKNOWN*":
			return
		default:
			break
		}
	}

	if wasEdit {
		var relay bool
		content, isAction, relay = d.editToRelay(m.ID, content, isAction)
		if !relay {
			return
		}
	} else {
		d.relayed.put(m.ID, content)
	}

	send := func(text string, action bool) {
		d.bridge.discordMessageEventsChan <- &DiscordMessage{
			Message:  m,
			Content:  prefixLines(text, tag),
			IsAction: action,
			PmTarget: pmTarget,
		}
	}

	// Sticker-only or attachment-only messages have no text; sending an
	// empty line would just make the IRC server complain.
	if strings.TrimSpace(content) != "" {
		send(content, isAction)
	}

	// Attachments etc. were already relayed with the original message.
	if wasEdit {
		return
	}

	for _, attachment := range m.Attachments {
		send(attachment.URL, isAction)
	}
	for _, extra := range messageExtras(m) {
		send(extra, false)
	}
}

func (d *discordBot) publishReaction(s *discordgo.Session, r *discordgo.MessageReaction) {
	if s.State.User == nil {
		return
	}

	user, err := s.User(r.UserID)
	if err != nil {
		log.Errorln(err)
		return
	}

	// Bridge needs these for mapping
	m := &discordgo.Message{
		ChannelID: r.ChannelID,
		Author:    user,
		GuildID:   r.GuildID,
	}

	// Reactions in a thread go to the parent's IRC channel, tagged.
	tag := ""
	if m.GuildID != "" {
		if _, mapped := d.bridge.GetMappingByDiscord(m.ChannelID); !mapped {
			if parentID, name, isThread := d.threadParent(m.ChannelID); isThread {
				if !d.bridge.bridgeDiscordThreads() {
					return
				}
				m.ChannelID = parentID
				tag = threadTag(name)
			}
		}
	}

	cachedMessage, err := dstate.ChannelMessage(d.Session, r.ChannelID, r.MessageID)
	reactionTarget := ""
	if err == nil && cachedMessage.Author != nil {
		// Copy, so we don't modify discordgo's cached message.
		msgCopy := *cachedMessage
		originalMessage := &msgCopy
		originalMessage.Mentions = append([]*discordgo.User(nil), originalMessage.Mentions...)

		// TODO 1: could add extra logic to figure out what length is needed to disambiguate
		// TODO 2: length should not cause command to exceed the max command length

		// HACK: this is before d.ParseText so that the existing <@uid> translation logic can be used
		username := userToMention(originalMessage.Author)
		if !originalMessage.Author.Bot {
			// HACK: theoretically could already be there, thereotically not a big problem
			originalMessage.Mentions = append(originalMessage.Mentions, originalMessage.Author)
		}
		originalMessage.Content = fmt.Sprintf(
			" to <%s> %s",
			username,
			// Truncate messages to just 40 characters so reactions to long messages
			// don't pollute the IRC log. Similarly, replace newlines with spaces
			// so that any reactions to messages with a newline within the first 40
			// characters don't cause multiple IRC messages to be sent.
			strings.ReplaceAll(TruncateString(40, originalMessage.Content), "\n", " "),
		)

		reactionTarget = d.ParseText(originalMessage)
	}

	emoji := r.Emoji.Name
	if r.Emoji.ID != "" {
		// Custom emoji
		emoji = fmt.Sprint(":", emoji, ":")
	}
	content := fmt.Sprint(tag, "reacted with ", emoji, reactionTarget)

	d.bridge.discordMessageEventsChan <- &DiscordMessage{
		Message:  m,
		Content:  content,
		IsAction: true,
		PmTarget: "",
	}
}

// splitAction reports whether raw Discord content is an action ("_text_",
// i.e. what /me produces) and returns the content without the underscores.
func splitAction(raw string) (string, bool) {
	if len(raw) > 2 && raw[0] == '_' && raw[len(raw)-1] == '_' {
		return raw[1 : len(raw)-1], true
	}
	return raw, false
}

// Up to date as of https://git.io/v5kJg
var channelMention = regexp.MustCompile(`<#(\d+)>`)
var roleMention = regexp.MustCompile(`<@&(\d+)>`)

var patternChannels = regexp.MustCompile("<#[^>]*>")
var emoteRegex = regexp.MustCompile(`<a?(:\w+:)\d+>`)

// Up to date as of https://git.io/v5kJg
func (d *discordBot) ParseText(m *discordgo.Message) string {
	// Replace @user mentions with name~d mentions
	content := m.Content

	for _, user := range m.Mentions {
		// Find the irc username with the discord ID in irc connections
		username, _ := d.bridge.ircManager.nickForUID(user.ID)

		if username == "" {
			// Nickname is their display name by default...
			nick := userDisplayName(user)

			// ...or their server nickname, if they have one
			member, err := d.Session.State.Member(d.guildID, user.ID)
			if err == nil && member.Nick != "" {
				nick = member.Nick
			}

			username = d.bridge.ircManager.generateNickname(DiscordUser{
				ID:            user.ID,
				Username:      user.Username,
				Discriminator: user.Discriminator,
				Nick:          nick,
				Bot:           user.Bot,
				Online:        false,
			}, false)

			log.WithFields(log.Fields{
				"discord-username": user.Username,
				"irc-username":     username,
				"discord-id":       user.ID,
			}).Infoln("Could not convert mention using existing IRC connection")
		} else {
			log.WithFields(log.Fields{
				"discord-username": user.Username,
				"irc-username":     username,
				"discord-id":       user.ID,
			}).Infoln("Converted mention using existing IRC connection")
		}

		content = strings.NewReplacer(
			"<@"+user.ID+">", username,
			"<@!"+user.ID+">", username,
		).Replace(content)
	}

	// Copied from message.go ContentWithMoreMentionsReplaced(s)
	for _, roleID := range m.MentionRoles {
		role, err := d.Session.State.Role(d.guildID, roleID)
		if err != nil || !role.Mentionable {
			continue
		}

		content = strings.Replace(content, "<&"+role.ID+">", "@"+role.Name, -1)
	}

	// Also copied from message.go ContentWithMoreMentionsReplaced(s)
	content = patternChannels.ReplaceAllStringFunc(content, func(mention string) string {
		channel, err := d.Session.State.Channel(mention[2 : len(mention)-1])
		if err != nil || channel.Type == discordgo.ChannelTypeGuildVoice {
			return mention
		}

		return "#" + channel.Name
	})

	// Break down malformed newlines
	content = strings.Replace(content, "\r\n", "\n", -1) // replace CRLF with LF
	content = strings.Replace(content, "\r", "\n", -1)   // replace CR with LF

	// Replace <#xxxxx> channel mentions
	content = channelMention.ReplaceAllStringFunc(content, func(str string) string {
		// Strip enclosing identifiers
		channelID := str[2 : len(str)-1]

		channel, err := d.Session.State.Channel(channelID)
		if err == nil {
			return "#" + channel.Name
		} else if err == discordgo.ErrStateNotFound {
			return "#deleted-channel"
		}

		log.WithError(err).WithField("mention", str).Warnln("channel mention lookup failed")
		return str
	})

	// Replace <@&xxxxx> role mentions
	content = roleMention.ReplaceAllStringFunc(content, func(str string) string {
		// Strip enclosing identifiers
		roleID := str[3 : len(str)-1]

		role, err := d.Session.State.Role(d.bridge.Config.GuildID, roleID)
		if err == nil {
			return "@" + role.Name
		} else if err == discordgo.ErrStateNotFound {
			return "@deleted-role"
		}

		log.WithError(err).WithField("mention", str).Warnln("role mention lookup failed")
		return str
	})

	// Replace emotes
	content = emoteRegex.ReplaceAllString(content, "$1")

	return content
}

func (d *discordBot) handlePresenceUpdate(uid string, status discordgo.Status, forceOnline bool) {
	// If they are offline, just deliver a mostly empty struct with the ID and online state
	if !forceOnline && !isStatusOnline(status) {
		if d.bridge.Config.DebugPresence {
			log.WithField("id", uid).Debugln("PRESENCE", status, "(handlePresenceUpdate - Online: false)")
		}
		d.sendUpdateUserChan(DiscordUser{
			ID:     uid,
			Online: false,
		})
		return
	}

	if d.bridge.Config.DebugPresence {
		log.WithField("id", uid).Debugln("PRESENCE", status, "(handlePresenceUpdate)")
	}

	// Otherwise get their GuildMember object...
	user, err := d.Session.State.Member(d.guildID, uid)
	if err != nil {
		log.Println(errors.Wrap(err, "get member from state in handlePresenceUpdate failed"))
		return
	}

	// .. and handle as per usual
	d.handleMemberUpdate(user, forceOnline)
}

func isStatusOnline(status discordgo.Status) bool {
	return status != discordgo.StatusOffline
}

func (d *discordBot) sendUpdateUserChan(user DiscordUser) bool {
	// Only log this for online events, because offline events won't have this
	if user.Username == "" && user.Online {
		log.WithFields(log.Fields{
			"err":                errors.WithStack(errors.New("Username is empty")).Error(),
			"user.Username":      user.Username,
			"user.Discriminator": user.Discriminator,
			"user.ID":            user.ID,
		}).Println("sendUpdateUserChan called with empty Username (see stack below)")
		debug.PrintStack()
	}

	d.bridge.updateUserChan <- user
	return true
}

// See https://github.com/reactiflux/discord-irc/pull/230/files#diff-7202bb7fb017faefd425a2af32df2f9dR357
func (d *discordBot) GetAvatar(guildID, username string) (_ string) {
	// First get all members
	guild, err := d.Session.State.Guild(guildID)
	if err != nil {
		log.WithError(err).WithField("guild", guildID).Warnln("could not get guild for avatar lookup")
		return
	}

	// guild.Members is updated by discordgo from other goroutines.
	d.Session.State.RLock()
	defer d.Session.State.RUnlock()

	// Matching members
	var foundMember *discordgo.Member

	// Try and find an exact case-sensitive match
	for _, member := range guild.Members {
		if member.User == nil {
			continue
		}
		if !memberHasName(member, username, false) {
			continue
		}

		// If there are multiple matches, return an empty string
		if foundMember == nil {
			foundMember = member
		} else {
			return
		}
	}

	// If no member found, check case-insensitively
	if foundMember == nil {
		for _, member := range guild.Members {
			if member.User == nil {
				continue
			}
			if !memberHasName(member, username, true) {
				continue
			}

			// If there are multiple matches, return an empty string
			if foundMember == nil {
				foundMember = member
			} else {
				return
			}
		}
	}

	// Do not provide an avatar if there is no matching user
	if foundMember == nil {
		return
	}

	return discordgo.EndpointUserAvatar(foundMember.User.ID, foundMember.User.Avatar)
}

// nameClashesWithMember reports whether some other guild member's display
// name sanitises to the same IRC nick as nick.
func (d *discordBot) nameClashesWithMember(uid, nick string) bool {
	guild, err := d.Session.State.Guild(d.bridge.Config.GuildID)
	if err != nil {
		log.WithError(err).Warnln("nickgen: guild not found when generating nickname")
		return false
	}

	d.Session.State.RLock()
	defer d.Session.State.RUnlock()

	for _, member := range guild.Members {
		if member.User == nil || member.User.ID == uid {
			continue
		}

		name := GetMemberNick(member)

		if name == "" {
			log.WithField("member", member).Errorln("blank username encountered")
			continue
		}

		if strings.EqualFold(sanitiseNickname(name), nick) {
			return true
		}
	}
	return false
}

// GetMemberNick returns the name a Discord GuildMember is shown as in the
// guild: their server nickname, else their global display name, else their
// username.
func GetMemberNick(m *discordgo.Member) string {
	if m.Nick != "" {
		return m.Nick
	}
	if m.User == nil {
		return ""
	}
	return userDisplayName(m.User)
}

// userDisplayName returns a user's global display name, falling back to
// their username for accounts (and bots) that have not set one.
func userDisplayName(u *discordgo.User) string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

// messageAuthorName returns the name a message's author is shown as in the
// guild (server nickname, display name, then username).
func messageAuthorName(m *discordgo.Message) string {
	if m.Member != nil && m.Member.Nick != "" {
		return m.Member.Nick
	}
	if m.Author == nil {
		return ""
	}
	return userDisplayName(m.Author)
}

// memberHasName reports whether name is the member's server nickname,
// display name or username.
func memberHasName(m *discordgo.Member, name string, foldCase bool) bool {
	eq := func(a, b string) bool {
		if a == "" {
			return false
		}
		if foldCase {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	return eq(m.Nick, name) || eq(m.User.GlobalName, name) || eq(m.User.Username, name)
}

// pmTargetFromContent returns an irc nick given a message sent to an IRC user via Discord
//
// Returns empty string if the nick could not be deduced.
// Also returns the content without the nick
func pmTargetFromContent(content string, discriminator string) (nick, newContent string) {
	// Pull out substrings
	// "qais,come on, i need this!" gives []string{"qais", "come on, i need this!"}
	subs := strings.SplitN(content, ",", 2)

	if len(subs) != 2 {
		return "", ""
	}

	nick = subs[0]
	newContent = strings.TrimPrefix(subs[1], " ")

	nickParts := strings.Split(nick, "@")

	// we were given an invalid nick if we can't split it into 2 parts
	if len(nickParts) < 2 {
		return "", ""
	}

	if nickParts[1] != discriminator {
		return "*UNKNOWN*", ""
	}

	nick = nickParts[0]

	// check if name is a valid nick
	for _, c := range []byte(nick) {
		if !ircnick.IsNickChar(c) {
			return "", ""
		}
	}

	return
}
