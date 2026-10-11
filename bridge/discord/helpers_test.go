package bdiscord

import (
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/mspgeek-community/matterbridge/bridge/helper"
	"github.com/stretchr/testify/assert"
)

func TestReplaceUserMentions(t *testing.T) {
	b := &Bdiscord{
		userMemberMap: map[string]*discordgo.Member{},
		nickMemberMap: map[string]*discordgo.Member{},
	}
	b.addMemberLocked(&discordgo.Member{User: &discordgo.User{ID: "1", Username: "jsmith", GlobalName: "John Smith"}})
	b.addMemberLocked(&discordgo.Member{User: &discordgo.User{ID: "2", Username: "kdoe"}, Nick: "Kim Doe"})

	// Slack sends display names with spaces removed.
	assert.Equal(t, "hey <@1>, see <@2>", b.replaceUserMentions("hey @JohnSmith, see @KimDoe"))
	assert.Equal(t, "<@1> and <@2>", b.replaceUserMentions("@jsmith and @kim doe"))
	assert.Equal(t, "@nobody", b.replaceUserMentions("@nobody"))

	// The index follows member updates.
	b.addMemberLocked(&discordgo.Member{User: &discordgo.User{ID: "3", Username: "newbie"}})
	assert.Equal(t, "<@3>", b.replaceUserMentions("@newbie"))
}

func TestChannelMention(t *testing.T) {
	b := &Bdiscord{channels: []*discordgo.Channel{{ID: "111", Name: "v-hudu"}, {ID: "222", Name: "general"}}}
	link, ok := b.ChannelMention("v-hudu")
	assert.True(t, ok)
	assert.Equal(t, "<#111>", link)
	_, ok = b.ChannelMention("V-Hudu") // exact names only
	assert.False(t, ok)
	_, ok = b.ChannelMention("missing")
	assert.False(t, ok)
}

func TestDiscordMentionsUseUserRefs(t *testing.T) {
	b := &Bdiscord{
		userMemberMap: map[string]*discordgo.Member{},
		nickMemberMap: map[string]*discordgo.Member{},
	}
	b.addMemberLocked(&discordgo.Member{User: &discordgo.User{ID: "1", Username: "mendyg", GlobalName: "Mendy Green"}, Nick: "Mendy Green [MSPGeek]"})

	// Incoming: a real Discord mention becomes a reference with all names.
	m := &discordgo.MessageCreate{Message: &discordgo.Message{
		Content:  "hey <@1> and <@!1>",
		Mentions: []*discordgo.User{{ID: "1", Username: "mendyg", GlobalName: "Mendy Green"}},
	}}
	out := b.replaceMentionsWithUsernames(m)
	assert.Equal(t, "hey @Mendy Green [MSPGeek] and @Mendy Green [MSPGeek]", helper.ReplaceUserRefs(out, nil))

	// Outgoing: a reference from Slack resolves to the member by any name.
	mention, ok := b.UserMention([]string{"Mendy Green [MSPGeek]", "mendy"})
	assert.True(t, ok)
	assert.Equal(t, "<@1>", mention)
	_, ok = b.UserMention([]string{"stranger"})
	assert.False(t, ok)
}
