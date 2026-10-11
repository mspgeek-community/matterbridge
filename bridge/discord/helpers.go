package bdiscord

import (
	"errors"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/mspgeek-community/matterbridge/bridge/helper"
)

func (b *Bdiscord) getAllowedMentions() *discordgo.MessageAllowedMentions {
	// If AllowMention is not specified, then allow all mentions (default Discord behavior)
	if !b.IsKeySet("AllowMention") {
		return nil
	}

	// Otherwise, allow only the mentions that are specified
	allowedMentionTypes := make([]discordgo.AllowedMentionType, 0, 3)
	for _, m := range b.GetStringSlice("AllowMention") {
		switch m {
		case "everyone":
			allowedMentionTypes = append(allowedMentionTypes, discordgo.AllowedMentionTypeEveryone)
		case "roles":
			allowedMentionTypes = append(allowedMentionTypes, discordgo.AllowedMentionTypeRoles)
		case "users":
			allowedMentionTypes = append(allowedMentionTypes, discordgo.AllowedMentionTypeUsers)
		}
	}

	return &discordgo.MessageAllowedMentions{
		Parse: allowedMentionTypes,
	}
}

func (b *Bdiscord) getNick(user *discordgo.User, guildID string) string {
	if user == nil {
		return ""
	}
	member := b.memberByID(guildID, user.ID)
	if member == nil {
		return user.Username
	}
	b.membersMutex.Lock()
	if _, ok := b.userMemberMap[user.ID]; !ok {
		b.addMemberLocked(member)
	}
	b.membersMutex.Unlock()
	if member.Nick != "" {
		// Only return if nick is set.
		return member.Nick
	}
	// Otherwise return username.
	return user.Username
}

const memberPageSize = 1000

func (b *Bdiscord) memberCount() int {
	b.membersMutex.RLock()
	defer b.membersMutex.RUnlock()
	return len(b.userMemberMap)
}

// addMemberLocked records a guild member. membersMutex must be held.
func (b *Bdiscord) addMemberLocked(member *discordgo.Member) {
	if member == nil || member.User == nil {
		return
	}
	b.userMemberMap[member.User.ID] = member
	b.nickMemberMap[member.User.Username] = member
	if member.Nick != "" {
		b.nickMemberMap[member.Nick] = member
	}
	b.memberIndexDirty = true
}

// loadMemberPage loads one page of guild members after the given user ID. It
// returns the last user ID seen and whether more members may follow.
func (b *Bdiscord) loadMemberPage(after string) (string, bool, error) {
	members, err := b.c.GuildMembers(b.guildID, after, memberPageSize)
	if err != nil {
		return after, false, err
	}
	b.membersMutex.Lock()
	defer b.membersMutex.Unlock()
	for _, member := range members {
		if member == nil || member.User == nil {
			b.Log.Warnf("Skipping missing information for a user.")
			continue
		}
		b.addMemberLocked(member)
		after = member.User.ID
	}
	return after, len(members) == memberPageSize, nil
}

// loadRemainingMembers pages through the rest of the guild members. It stops
// when session is replaced by a reconnect.
func (b *Bdiscord) loadRemainingMembers(session *discordgo.Session, after string) {
	pages := 1
	for {
		b.connectMutex.Lock()
		current := b.c == session
		b.connectMutex.Unlock()
		if !current {
			return
		}
		var more bool
		var err error
		after, more, err = b.loadMemberPage(after)
		if err != nil {
			b.Log.Errorf("Loading server members failed after %d pages: %s", pages, err)
			return
		}
		pages++
		if !more {
			b.Log.Infof("Loaded %d server members in %d pages", b.memberCount(), pages)
			return
		}
	}
}

// memberNameIndex returns the name index for @mention resolution, rebuilding
// it after member changes. Usernames, global display names and server
// nicknames all resolve.
func (b *Bdiscord) memberNameIndex() helper.NameIndex {
	b.membersMutex.Lock()
	defer b.membersMutex.Unlock()
	if b.memberIndex == nil || b.memberIndexDirty {
		idx := helper.NameIndex{}
		for id, member := range b.userMemberMap {
			idx.Add(id, member.User.Username, member.User.GlobalName, member.Nick)
		}
		b.memberIndex = idx
		b.memberIndexDirty = false
	}
	return b.memberIndex
}

func (b *Bdiscord) getGuildMemberByNick(nick string) (*discordgo.Member, error) {
	b.membersMutex.RLock()
	defer b.membersMutex.RUnlock()

	if member, ok := b.nickMemberMap[strings.TrimSpace(nick)]; ok {
		return member, nil
	}
	return nil, errors.New("Couldn't find guild member with nick " + nick) // This will most likely get ignored by the caller
}

func (b *Bdiscord) getChannelID(name string) string {
	if strings.Contains(name, "/") {
		return b.getCategoryChannelID(name)
	}
	b.channelsMutex.RLock()
	defer b.channelsMutex.RUnlock()

	idcheck := strings.Split(name, "ID:")
	if len(idcheck) > 1 {
		return idcheck[1]
	}
	for _, channel := range b.channels {
		if channel.Name == name && channel.Type == discordgo.ChannelTypeGuildText {
			return channel.ID
		}
	}
	return ""
}

// ChannelIDByName returns the Discord channel ID for a configured channel name.
func (b *Bdiscord) ChannelIDByName(name string) string {
	return b.getChannelID(name)
}

// UserMention returns the Discord mention of the member with these names (a
// user reference from another platform).
func (b *Bdiscord) UserMention(names []string) (string, bool) {
	id, ok := helper.ResolveUserByNames(b.memberNameIndex(), names)
	if !ok {
		return "", false
	}
	return "<@" + id + ">", true
}

// ChannelMention returns the mention of the channel named exactly name, for
// channel references from other platforms.
func (b *Bdiscord) ChannelMention(name string) (string, bool) {
	b.channelsMutex.RLock()
	defer b.channelsMutex.RUnlock()
	for _, channel := range b.channels {
		if channel.Name == name {
			return "<#" + channel.ID + ">", true
		}
	}
	return "", false
}

// GuildID returns the ID of the Discord server this bridge is connected to.
func (b *Bdiscord) GuildID() string {
	b.channelsMutex.RLock()
	defer b.channelsMutex.RUnlock()
	return b.guildID
}

func (b *Bdiscord) getCategoryChannelID(name string) string {
	b.channelsMutex.RLock()
	defer b.channelsMutex.RUnlock()
	res := strings.Split(name, "/")
	// shouldn't happen because function should be only called from getChannelID
	if len(res) != 2 {
		return ""
	}
	catName, chanName := res[0], res[1]
	for _, channel := range b.channels {
		// if we have a parentID, lookup the name of that parent (category)
		// and if it matches return it
		if channel.Name == chanName && channel.ParentID != "" {
			for _, cat := range b.channels {
				if cat.ID == channel.ParentID && cat.Name == catName {
					return channel.ID
				}
			}
		}
	}
	return ""
}

func (b *Bdiscord) getChannelName(id string) string {
	b.channelsMutex.RLock()
	defer b.channelsMutex.RUnlock()

	for _, c := range b.channelInfoMap {
		if c.Name == "ID:"+id {
			// if we have ID: specified in our gateway configuration return this
			return c.Name
		}
	}

	for _, channel := range b.channels {
		if channel.ID == id {
			return b.getCategoryChannelName(channel.Name, channel.ParentID)
		}
	}
	return ""
}

func (b *Bdiscord) getCategoryChannelName(name, parentID string) string {
	var usesCat bool
	// do we have a category configuration in the channel config
	for _, c := range b.channelInfoMap {
		if strings.Contains(c.Name, "/") {
			usesCat = true
			break
		}
	}
	// configuration without category, return the normal channel name
	if !usesCat {
		return name
	}
	// create a category/channel response
	for _, c := range b.channels {
		if c.ID == parentID {
			name = c.Name + "/" + name
		}
	}
	return name
}

var (
	// See https://discordapp.com/developers/docs/reference#message-formatting.
	channelMentionRE = regexp.MustCompile("<#[0-9]+>")
	userMentionRE    = regexp.MustCompile("@[^@\n]{1,32}")
	emoteRE          = regexp.MustCompile(`<a?(:\w+:)\d+>`)
)

func (b *Bdiscord) replaceChannelMentions(text string) string {

    replaceChannelMentionFunc := func(match string) string {
        channelID := match[2 : len(match)-1]
        var channelName string

        channel, err := b.c.State.Channel(channelID)
        if err != nil {
            // Fetch the channel from the API if not found in the state
            channel, err = b.c.Channel(channelID)
            if err != nil {
                return "#unknownchannel"
            }
        }

        if channel.Type == discordgo.ChannelTypeGuildNewsThread || channel.Type == discordgo.ChannelTypeGuildPublicThread || channel.Type == discordgo.ChannelTypeGuildPrivateThread {
            thread, err := b.c.Channel(channel.ID)
            if err != nil {
                return "#" + channel.Name // Return channel name if thread retrieval fails
            }
            return "<#"+ thread.Name +">"
        } else {
            channelName = channel.Name
        }
        return "#" + channelName
    }
    return channelMentionRE.ReplaceAllStringFunc(text, replaceChannelMentionFunc)
}
// replaceUserMentions turns "@name" into a Discord mention when name matches
// a member's username, display name or nickname (ignoring case, spaces and
// punctuation).
func (b *Bdiscord) replaceUserMentions(text string) string {
	idx := b.memberNameIndex()
	return helper.ReplaceMentions(text, func(name string) (string, bool) {
		id, ok := idx.Lookup(name)
		if !ok {
			return "", false
		}
		return "<@" + id + ">", true
	})
}
func (b *Bdiscord) sanitizeUsername(username string) string {
	return strings.ReplaceAll(username, " ", "")
}
func replaceEmotes(text string) string {
	return emoteRE.ReplaceAllString(text, "$1")
}

func (b *Bdiscord) replaceAction(text string) (string, bool) {
	length := len(text)
	if length > 1 && text[0] == '_' && text[length-1] == '_' {
		return text[1 : length-1], true
	}
	return text, false
}

// splitURL splits a webhookURL and returns the ID and token.
func (b *Bdiscord) splitURL(url string) (string, string, bool) {
	const (
		expectedWebhookSplitCount = 7
		webhookIdxID              = 5
		webhookIdxToken           = 6
	)
	webhookURLSplit := strings.Split(url, "/")
	if len(webhookURLSplit) != expectedWebhookSplitCount {
		return "", "", false
	}
	return webhookURLSplit[webhookIdxID], webhookURLSplit[webhookIdxToken], true
}

