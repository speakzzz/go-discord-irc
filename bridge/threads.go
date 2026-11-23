package bridge

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	log "github.com/sirupsen/logrus"
)

// threadParent reports whether channelID is a Discord thread (including a
// forum post), and if so returns its parent channel's ID and its name.
func (d *discordBot) threadParent(channelID string) (parentID, name string, ok bool) {
	ch, err := d.Session.State.Channel(channelID)
	if err != nil {
		// Not cached, e.g. an archived thread that just got a new message.
		ch, err = d.Session.Channel(channelID)
		if err != nil {
			log.WithError(err).WithField("channel", channelID).Debugln("could not look up channel")
			return "", "", false
		}
		if ch.IsThread() {
			_ = d.Session.State.ChannelAdd(ch)
		}
	}
	if !ch.IsThread() || ch.ParentID == "" {
		return "", "", false
	}
	return ch.ParentID, ch.Name, true
}

// threadTag is what IRC sees in front of messages from a thread.
func threadTag(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	return "[" + name + "] "
}

// prefixLines puts prefix in front of every line of s (each line is a
// separate IRC message).
func prefixLines(s, prefix string) string {
	if prefix == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// onThreadCreate announces new threads and forum posts in mapped channels.
// (The "started a thread" system message Discord posts in the parent
// channel is skipped in publishMessage, so this is the only announcement.)
func (d *discordBot) onThreadCreate(s *discordgo.Session, t *discordgo.ThreadCreate) {
	defer recoverAndLog("discord onThreadCreate")

	if t.Channel == nil || !t.NewlyCreated || !d.bridge.bridgeDiscordThreads() {
		return
	}
	if _, mapped := d.bridge.GetMappingByDiscord(t.ParentID); !mapped {
		return
	}
	if s.State.User != nil && t.OwnerID == s.State.User.ID {
		return
	}

	msg := &discordgo.Message{ChannelID: t.ParentID, GuildID: t.GuildID}
	if member, err := s.State.Member(t.GuildID, t.OwnerID); err == nil {
		msg.Member = member
		msg.Author = member.User
	} else if user, err := s.User(t.OwnerID); err == nil {
		msg.Author = user
	} else {
		log.WithError(err).WithField("owner", t.OwnerID).Warnln("could not look up thread owner")
		return
	}
	if msg.Author == nil || d.transmitter.HasWebhook(msg.Author.ID) {
		return
	}

	d.bridge.discordMessageEventsChan <- &DiscordMessage{
		Message:  msg,
		Content:  "started a thread: " + strings.Join(strings.Fields(t.Name), " "),
		IsAction: true,
	}
}

// messageExtras describes parts of a Discord message that have no text of
// their own, so IRC sees something instead of nothing: stickers, rich
// embeds (e.g. from bots) and polls. Link previews are skipped, since the
// link is already in the message.
func messageExtras(m *discordgo.Message) []string {
	var extras []string

	for _, s := range m.StickerItems {
		if s != nil && s.Name != "" {
			extras = append(extras, "[sticker: "+s.Name+"]")
		}
	}

	for _, e := range m.Embeds {
		if e == nil || e.Type != discordgo.EmbedTypeRich {
			continue
		}
		title := oneLine(e.Title)
		desc := TruncateString(150, oneLine(e.Description))
		var parts []string
		switch {
		case title != "" && desc != "":
			parts = append(parts, title+" — "+desc)
		case title != "":
			parts = append(parts, title)
		case desc != "":
			parts = append(parts, desc)
		}
		if e.URL != "" {
			parts = append(parts, e.URL)
		}
		if len(parts) > 0 {
			extras = append(extras, "[embed] "+strings.Join(parts, " "))
		}
	}

	if p := m.Poll; p != nil {
		question := oneLine(p.Question.Text)
		var answers []string
		for _, a := range p.Answers {
			if a.Media != nil && a.Media.Text != "" {
				answers = append(answers, oneLine(a.Media.Text))
			}
		}
		line := "[poll] " + question
		if len(answers) > 0 {
			line += fmt.Sprintf(" (%s)", strings.Join(answers, " / "))
		}
		extras = append(extras, line)
	}

	return extras
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// isThreadSystemMessage reports Discord's own messages about threads: the
// "X started a thread" notice in the parent channel and the starter message
// at the top of a thread. Neither is something a user said.
func isThreadSystemMessage(m *discordgo.Message) bool {
	return m.Type == discordgo.MessageTypeThreadCreated || m.Type == discordgo.MessageTypeThreadStarterMessage
}
