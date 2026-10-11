package bdiscord

import (
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/mspgeek-community/matterbridge/bridge/config"
	"github.com/mspgeek-community/matterbridge/bridge/helper"
)

func (b *Bdiscord) messageDelete(s *discordgo.Session, m *discordgo.MessageDelete) { //nolint:unparam
	if m.GuildID != b.guildID {
		b.Log.Debugf("Ignoring messageDelete because it originates from a different guild")
		return
	}
	rmsg := config.Message{Account: b.Account, ID: m.ID, Event: config.EventMsgDelete, Text: config.EventMsgDelete}
	//lookup our channel ID using the API. If its a thread, we need to get the parent channel ID and set m.ChannelID to that.
	PotentialThread, err := b.channelByID(m.ChannelID)
	//print  PotentialThread
	if err != nil {
		b.Log.Debugf("Error getting channel info for %s: %v", m.ChannelID, err)
		return
	}
	if PotentialThread.ParentID != "" && PotentialThread.IsThread() {
		m.ChannelID = PotentialThread.ParentID
	}
	rmsg.Channel = b.getChannelName(m.ChannelID)

	b.Log.Debugf("<= Sending message from %s to gateway", b.Account)
	b.Log.Debugf("<= Message is %#v", rmsg)
	b.Remote <- rmsg
}

// TODO(qaisjp): if other bridges support bulk deletions, it could be fanned out centrally
func (b *Bdiscord) messageDeleteBulk(s *discordgo.Session, m *discordgo.MessageDeleteBulk) { //nolint:unparam
	if m.GuildID != b.guildID {
		b.Log.Debugf("Ignoring messageDeleteBulk because it originates from a different guild")
		return
	}
	for _, msgID := range m.Messages {
		rmsg := config.Message{
			Account: b.Account,
			ID:      msgID,
			Event:   config.EventMsgDelete,
			Text:    config.EventMsgDelete,
			Channel: b.getChannelName(m.ChannelID),
		}

		b.Log.Debugf("<= Sending message from %s to gateway", b.Account)
		b.Log.Debugf("<= Message is %#v", rmsg)
		b.Remote <- rmsg
	}
}

func (b *Bdiscord) messageEvent(s *discordgo.Session, m *discordgo.Event) {
	b.Log.Debugf("== Receiving event: %s %s", m.Type, m.RawData)
}

func (b *Bdiscord) messageTyping(s *discordgo.Session, m *discordgo.TypingStart) {
	if m.GuildID != b.guildID {
		b.Log.Debugf("Ignoring messageTyping because it originates from a different guild")
		return
	}
	if !b.GetBool("ShowUserTyping") {
		return
	}

	// Ignore our own typing messages
	if m.UserID == b.userID {
		return
	}

	rmsg := config.Message{Account: b.Account, Event: config.EventUserTyping}
	rmsg.Channel = b.getChannelName(m.ChannelID)
	b.Remote <- rmsg
}

func (b *Bdiscord) messageUpdate(s *discordgo.Session, m *discordgo.MessageUpdate) { //nolint:unparam
	if m.GuildID != b.guildID {
		b.Log.Debugf("Ignoring messageUpdate because it originates from a different guild")
		return
	}
	if b.GetBool("EditDisable") {
		return
	}
	// Discord also sends MESSAGE_UPDATE for thread creation, pins, embeds and
	// thread counters. Those keep the old edited_timestamp, so only relay
	// updates whose edit happened just now, and each edit once.
	if m.Message == nil || m.EditedTimestamp == nil {
		return
	}
	if age := time.Since(*m.EditedTimestamp); age > config.EditMaxAge {
		b.Log.Debugf("Ignoring update of message %s: last edit was %s ago", m.ID, age.Round(time.Second))
		return
	}
	editKey := cEdit + m.ID + " " + m.EditedTimestamp.String()
	if b.cache.Contains(editKey) {
		b.Log.Debugf("Ignoring repeated update of message %s", m.ID)
		return
	}
	b.cache.Add(editKey, true)

	b.Log.Debugf("Sending edit message")
	m.Content += b.GetString("EditSuffix")
	b.relayMessage(&discordgo.MessageCreate{Message: m.Message}, true)
}

func (b *Bdiscord) replaceMentionsWithUsernames(m *discordgo.MessageCreate) string {
	for _, user := range m.Mentions {
		if user == nil {
			continue
		}
		display, nick := user.Username, ""
		if user.GlobalName != "" {
			display = user.GlobalName
		}
		if member := b.memberByID(m.GuildID, user.ID); member != nil && member.Nick != "" {
			display, nick = member.Nick, member.Nick
		}
		// Carry all of the user's names so the destination can link the
		// right person without guessing where the name ends.
		ref := helper.UserRef(display, nick, user.GlobalName, user.Username)
		m.Content = strings.NewReplacer(
			"<@"+user.ID+">", ref,
			"<@!"+user.ID+">", ref,
		).Replace(m.Content)
	}
	return m.Content
}

// memberByID returns a guild member from the local member map, the session
// state or, as a last resort, the API. It returns nil when the user is not a
// member (e.g. left the server).
func (b *Bdiscord) memberByID(guildID, userID string) *discordgo.Member {
	b.membersMutex.RLock()
	member, ok := b.userMemberMap[userID]
	b.membersMutex.RUnlock()
	if ok {
		return member
	}
	if member, err := b.c.State.Member(guildID, userID); err == nil {
		return member
	}
	member, err := b.c.GuildMember(guildID, userID)
	if err != nil {
		b.Log.Debugf("Could not get member %s: %s", userID, err)
		return nil
	}
	return member
}

// channelByID returns a channel or thread from the session state, falling
// back to the API.
func (b *Bdiscord) channelByID(channelID string) (*discordgo.Channel, error) {
	if channel, err := b.c.State.Channel(channelID); err == nil {
		return channel, nil
	}
	return b.c.Channel(channelID)
}

func (b *Bdiscord) messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) { //nolint:unparam
	if m.GuildID != b.guildID {
		b.Log.Debugf("Ignoring messageCreate because it originates from a different guild")
		return
	}
	b.relayMessage(m, false)
}

func (b *Bdiscord) relayMessage(m *discordgo.MessageCreate, edit bool) {
	var err error

	// partial updates may not carry an author
	if m.Message == nil || m.Author == nil {
		return
	}

	// not relay our own messages
	if m.Author.Username == b.nick {
		return
	}
	// if using webhooks, do not relay if it's ours
	if m.Author.Bot && b.transmitter.HasWebhook(m.Author.ID) {
		return
	}

	// add the url of the attachments to content
	if len(m.Attachments) > 0 {
		for _, attach := range m.Attachments {
			m.Content = m.Content + "\n" + attach.URL
		}
	}

	rmsg := config.Message{
		Account: b.Account,
		Avatar:  "https://cdn.discordapp.com/avatars/" + m.Author.ID + "/" + m.Author.Avatar + ".jpg",
		UserID:  m.Author.ID,
		ID:      m.ID,
		Edit:    edit,
	}

	b.Log.Debugf("== Receiving event %#v", m.Message)

	if m.Content != "" {
		m.Message.Content = b.replaceChannelMentions(m.Message.Content)
		m.Message.Content = b.replaceMentionsWithUsernames(m)

		rmsg.Text, err = m.ContentWithMoreMentionsReplaced(b.c)
		if err != nil {
			b.Log.Errorf("ContentWithMoreMentionsReplaced failed: %s", err)
			rmsg.Text = m.ContentWithMentionsReplaced()
		}
	}
	// set channel name
	rmsg.Channel = b.getChannelName(m.ChannelID)
	// inside messageCreate function, just after setting rmsg.Username
	rmsg.Username = b.getNick(m.Author, m.GuildID)
	b.Log.Debugf("Username for the message created: %s", rmsg.Username)

	// if we have embedded content add it to text
	if b.GetBool("ShowEmbeds") && m.Message.Embeds != nil {
		for _, embed := range m.Message.Embeds {
			rmsg.Text += handleEmbed(embed)
		}
	}

	// no empty messages
	if rmsg.Text == "" {
		return
	}

	// do we have a /me action
	var ok bool
	rmsg.Text, ok = b.replaceAction(rmsg.Text)
	if ok {
		rmsg.Event = config.EventUserAction
	}

	// Replace emotes
	rmsg.Text = replaceEmotes(rmsg.Text)

	// Add our parent id if it exists, and if it's not referring to a message in another channel
	if ref := m.MessageReference; ref != nil && ref.ChannelID == m.ChannelID {
		//rmsg.ParentID = ref.MessageID //used to track replies previously
		rmsg.ThreadID = ref.MessageID
		if ref := m.MessageReference; ref != nil && ref.ChannelID == m.ChannelID {
			if m.ReferencedMessage != nil && m.ReferencedMessage.Author != nil {
				authorName := "@" + b.getNick(m.ReferencedMessage.Author, m.GuildID)
				authorIcon := "https://cdn.discordapp.com/avatars/" + m.ReferencedMessage.Author.ID + "/" + m.ReferencedMessage.Author.Avatar + ".jpg"
				originalMessageContent := m.ReferencedMessage.Content
				urls := make([]string, len(m.ReferencedMessage.Attachments))
				for i, attachment := range m.ReferencedMessage.Attachments {
					urls[i] = attachment.URL
				}
				allUrls := strings.Join(urls, " ")
				originalMessageContent += " " + allUrls
				channelName := b.replaceChannelMentions("<#" + m.ReferencedMessage.ChannelID + ">")
				rmsg.Text = authorName + "\n~|" + originalMessageContent + "\n~|" + rmsg.Text + "\n~|" + authorIcon + "\n~|" + channelName + "\n~|" + m.ReferencedMessage.Timestamp.Local().Format("2006-01-02 15:04:05")
				// Store the original message content and author's name in rmsg.Extra

			}
		}
	}
	rmsg.Text = b.replaceChannelMentions(rmsg.Text)
	if rmsg.ParentID == "" {
		channel, err := b.channelByID(m.ChannelID)
		if err != nil {
			b.Log.Errorf("Error fetching channel: %v", err)
		} else if channel.IsThread() {
			parent, err := b.channelByID(channel.ParentID)
			if err != nil {
				b.Log.Errorf("Error fetching parent of thread %s: %v", channel.ID, err)
			} else {
				rmsg.ParentID = channel.ID
				rmsg.Channel = parent.Name
				rmsg.ThreadID = m.ChannelID
			}
		}
	}
	b.Log.Debugf("<= Sending message from %s on %s to gateway", m.Author.Username, b.Account)
	b.Log.Debugf("<= Message is %#v", rmsg)
	b.Remote <- rmsg
}

func (b *Bdiscord) memberUpdate(s *discordgo.Session, m *discordgo.GuildMemberUpdate) {
	if m.GuildID != b.guildID {
		b.Log.Debugf("Ignoring memberUpdate because it originates from a different guild")
		return
	}
	if m.Member == nil || m.Member.User == nil {
		b.Log.Warnf("Received member update with no member information: %#v", m)
		return
	}

	b.membersMutex.Lock()
	defer b.membersMutex.Unlock()

	if currMember, ok := b.userMemberMap[m.Member.User.ID]; ok {
		b.Log.Debugf(
			"%s: memberupdate: user %s (nick %s) changes nick to %s",
			b.Account,
			m.Member.User.Username,
			b.userMemberMap[m.Member.User.ID].Nick,
			m.Member.Nick,
		)
		delete(b.nickMemberMap, currMember.User.Username)
		delete(b.nickMemberMap, currMember.Nick)
		delete(b.userMemberMap, m.Member.User.ID)
	}
	b.addMemberLocked(m.Member)
}

func (b *Bdiscord) memberAdd(s *discordgo.Session, m *discordgo.GuildMemberAdd) {
	if m.GuildID != b.guildID {
		b.Log.Debugf("Ignoring memberAdd because it originates from a different guild")
		return
	}
	if b.GetBool("nosendjoinpart") {
		return
	}
	if m.Member == nil {
		b.Log.Warnf("Received member update with no member information: %#v", m)
		return
	}
	username := m.Member.User.Username
	if m.Member.Nick != "" {
		username = m.Member.Nick
	}

	rmsg := config.Message{
		Account:  b.Account,
		Event:    config.EventJoinLeave,
		Username: "system",
		Text:     username + " joins",
	}
	b.Log.Debugf("<= Sending message from %s to gateway", b.Account)
	b.Log.Debugf("<= Message is %#v", rmsg)
	b.Remote <- rmsg
}

func (b *Bdiscord) memberRemove(s *discordgo.Session, m *discordgo.GuildMemberRemove) {
	if m.GuildID != b.guildID {
		b.Log.Debugf("Ignoring memberRemove because it originates from a different guild")
		return
	}
	if b.GetBool("nosendjoinpart") {
		return
	}
	if m.Member == nil {
		b.Log.Warnf("Received member update with no member information: %#v", m)
		return
	}
	username := m.Member.User.Username
	if m.Member.Nick != "" {
		username = m.Member.Nick
	}

	rmsg := config.Message{
		Account:  b.Account,
		Event:    config.EventJoinLeave,
		Username: "system",
		Text:     username + " leaves",
	}
	b.Log.Debugf("<= Sending message from %s to gateway", b.Account)
	b.Log.Debugf("<= Message is %#v", rmsg)
	b.Remote <- rmsg
}

func handleEmbed(embed *discordgo.MessageEmbed) string {
	var t []string
	var result string

	t = append(t, embed.Title)
	t = append(t, embed.Description)
	t = append(t, embed.URL)

	i := 0
	for _, e := range t {
		if e == "" {
			continue
		}

		i++
		if i == 1 {
			result += " embed: " + e
			continue
		}

		result += " - " + e
	}

	if result != "" {
		result += "\n"
	}

	return result
}
