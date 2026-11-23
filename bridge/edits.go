package bridge

import (
	"strings"
	"sync"
)

// How Discord message edits are relayed to IRC (config: discord_edits).
const (
	EditsFull    = "full"    // "[edit] <whole new message>"
	EditsCompact = "compact" // "s/old/new/" for small changes, else full
	EditsOff     = "off"     // don't relay edits
)

// messageCache is a bounded map from Discord message ID to the text we last
// relayed for it. The oldest entries are evicted first.
type messageCache struct {
	mu    sync.Mutex
	max   int
	items map[string]string
	order []string
}

func newMessageCache(max int) *messageCache {
	return &messageCache{max: max, items: make(map[string]string, max)}
}

func (c *messageCache) put(id, content string) {
	if id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[id]; !ok {
		c.order = append(c.order, id)
		for len(c.order) > c.max {
			delete(c.items, c.order[0])
			c.order = c.order[1:]
		}
	}
	c.items[id] = content
}

func (c *messageCache) get(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	content, ok := c.items[id]
	return content, ok
}

// editToRelay decides what (if anything) to send to IRC for an edit of
// message id whose new IRC text is content.
func (d *discordBot) editToRelay(id, content string, isAction bool) (string, bool, bool) {
	mode := d.bridge.discordEditsMode()
	old, known := d.relayed.get(id)
	d.relayed.put(id, content)

	if mode == EditsOff {
		return "", false, false
	}

	// Discord also sends updates when e.g. a link preview is added; the
	// text is unchanged, so there's nothing to tell IRC.
	if known && old == content {
		return "", false, false
	}

	if mode == EditsCompact && known {
		if sed := compactEdit(old, content); sed != "" {
			return sed, false, true
		}
	}

	return "[edit] " + content, isAction, true
}

// compactEdit describes a small single-line edit IRC-style, as
// "s/old/new/". It returns "" when the change is too big or ambiguous to
// describe that way.
func compactEdit(oldText, newText string) string {
	const maxWords = 3

	if oldText == newText || strings.Contains(oldText, "\n") || strings.Contains(newText, "\n") {
		return ""
	}

	ow := strings.Split(oldText, " ")
	nw := strings.Split(newText, " ")

	p := 0
	for p < len(ow) && p < len(nw) && ow[p] == nw[p] {
		p++
	}
	q := 0
	for q < len(ow)-p && q < len(nw)-p && ow[len(ow)-1-q] == nw[len(nw)-1-q] {
		q++
	}

	// Pure insertion or deletion: include a neighbouring word, so there is
	// something to substitute and the spacing comes out right.
	if len(ow)-q-p == 0 || len(nw)-q-p == 0 {
		switch {
		case p > 0:
			p--
		case q > 0:
			q--
		default:
			return ""
		}
	}

	from := strings.Join(ow[p:len(ow)-q], " ")
	to := strings.Join(nw[p:len(nw)-q], " ")

	if from == "" || len(ow)-q-p > maxWords || len(nw)-q-p > maxWords {
		return ""
	}
	// s/// replaces the first match; if the old text appears more than
	// once the result would be ambiguous.
	if strings.Count(oldText, from) != 1 {
		return ""
	}

	for _, delim := range []string{"/", "|", "#", "!", ","} {
		if !strings.Contains(from, delim) && !strings.Contains(to, delim) {
			return "s" + delim + from + delim + to + delim
		}
	}
	return ""
}
