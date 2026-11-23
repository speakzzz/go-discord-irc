package bridge

import (
	"fmt"
	"sync"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	ircf "github.com/qaisjp/go-discord-irc/irc/format"
	irc "github.com/qaisjp/go-ircevent"
	log "github.com/sirupsen/logrus"
)

// How IRC topic changes are shown on Discord (config: irc_topic_to_discord).
const (
	TopicMessage = "message" // post "nick changed the topic to: ..."
	TopicSync    = "topic"   // set the Discord channel's topic to match
	TopicBoth    = "both"
	TopicOff     = "off"
)

// Discord limits channel topics to 1024 characters.
const discordTopicMax = 1024

// onTopicChange handles TOPIC: someone changed a channel's topic.
func (i *ircListener) onTopicChange(e *irc.Event) {
	if len(e.Arguments) < 1 {
		return
	}
	mode := i.bridge.ircTopicMode()
	if mode == TopicOff {
		return
	}
	if i.isPuppetNick(e.Nick) || i.bridge.ircManager.isIgnoredHostmask(e.Source) {
		return
	}

	channel := e.Arguments[0]
	mapping, ok := i.bridge.GetMappingByIRC(channel)
	if !ok {
		return
	}

	topic := ""
	if len(e.Arguments) > 1 {
		topic = e.Arguments[len(e.Arguments)-1]
	}
	text := ircf.BlocksToMarkdown(ircf.Parse(topic))

	if mode == TopicMessage || mode == TopicBoth {
		msg := fmt.Sprintf("_%s changed the topic to:_ %s", e.Nick, text)
		if topic == "" {
			msg = fmt.Sprintf("_%s cleared the topic_", e.Nick)
		}
		go func() {
			i.bridge.discordMessagesChan <- IRCMessage{IRCChannel: channel, Message: msg}
		}()
	}

	if mode == TopicSync || mode == TopicBoth {
		i.bridge.discord.syncTopic(mapping.DiscordChannel, text)
	}
}

// onTopicReply handles 332 RPL_TOPIC, the current topic sent when we join,
// so the Discord topic also catches up with changes made while we were away.
func (i *ircListener) onTopicReply(e *irc.Event) {
	// 332 <our nick> <channel> :<topic>
	if len(e.Arguments) < 3 {
		return
	}
	if mode := i.bridge.ircTopicMode(); mode != TopicSync && mode != TopicBoth {
		return
	}
	mapping, ok := i.bridge.GetMappingByIRC(e.Arguments[1])
	if !ok {
		return
	}
	i.bridge.discord.syncTopic(mapping.DiscordChannel, ircf.BlocksToMarkdown(ircf.Parse(e.Arguments[2])))
}

// topicSyncer sets Discord channel topics. Discord allows only a couple of
// topic edits per channel every ten minutes and makes later ones wait, so
// rapid changes are collapsed: only the latest topic for a channel is set.
type topicSyncer struct {
	mu      sync.Mutex
	pending map[string]string // channel ID -> newest wanted topic
	running map[string]bool   // channel ID -> worker active
}

func newTopicSyncer() *topicSyncer {
	return &topicSyncer{pending: map[string]string{}, running: map[string]bool{}}
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}

func (d *discordBot) syncTopic(channelID, topic string) {
	topic = truncateRunes(topic, discordTopicMax)

	// Discord's API can't clear a topic through this edit call, and an
	// unchanged topic needs no edit.
	if topic == "" {
		return
	}
	if ch, err := d.Session.State.Channel(channelID); err == nil && ch.Topic == topic {
		return
	}

	t := d.topics
	t.mu.Lock()
	t.pending[channelID] = topic
	if t.running[channelID] {
		t.mu.Unlock()
		return
	}
	t.running[channelID] = true
	t.mu.Unlock()

	go d.topicWorker(channelID)
}

func (d *discordBot) topicWorker(channelID string) {
	t := d.topics
	for {
		t.mu.Lock()
		topic, ok := t.pending[channelID]
		if !ok {
			t.running[channelID] = false
			t.mu.Unlock()
			return
		}
		delete(t.pending, channelID)
		t.mu.Unlock()

		d.setChannelTopic(channelID, topic)
	}
}

func (d *discordBot) setChannelTopic(channelID, topic string) {
	defer recoverAndLog("topic sync")
	if _, err := d.Session.ChannelEdit(channelID, &discordgo.ChannelEdit{Topic: topic}); err != nil {
		log.WithError(err).WithField("channel", channelID).
			Errorln("Could not set Discord channel topic (does the bot have Manage Channels?)")
	}
}
