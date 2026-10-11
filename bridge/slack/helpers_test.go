package bslack

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mspgeek-community/matterbridge/bridge"
	"github.com/mspgeek-community/matterbridge/bridge/config"
	"github.com/mspgeek-community/matterbridge/bridge/helper"
	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
)

func TestExtractTopicOrPurpose(t *testing.T) {
	testcases := map[string]struct {
		input          string
		wantChangeType string
		wantOutput     string
	}{
		"success - topic type":   {"@someone set channel topic: foo bar", "topic", "foo bar"},
		"success - purpose type": {"@someone set channel purpose: foo bar", "purpose", "foo bar"},
		"success - one line":     {"@someone set channel topic: foo bar", "topic", "foo bar"},
		"success - multi-line":   {"@someone set channel topic: foo\nbar", "topic", "foo\nbar"},
		"success - cleared":      {"@someone cleared channel topic", "topic", ""},
		"error - unhandled":      {"some unmatched message", "unknown", ""},
	}

	logger := logrus.New()
	logger.SetOutput(ioutil.Discard)
	cfg := &bridge.Config{Bridge: &bridge.Bridge{Log: logrus.NewEntry(logger)}}
	b := newBridge(cfg)
	for name, tc := range testcases {
		gotChangeType, gotOutput := b.extractTopicOrPurpose(tc.input)

		assert.Equalf(t, tc.wantChangeType, gotChangeType, "This testcase failed: %s", name)
		assert.Equalf(t, tc.wantOutput, gotOutput, "This testcase failed: %s", name)
	}
}

func TestIsFreshEdit(t *testing.T) {
	ts := func(d time.Duration) *slack.Edited {
		return &slack.Edited{Timestamp: fmt.Sprintf("%d.000100", time.Now().Add(-d).Unix())}
	}
	assert.False(t, isFreshEdit(nil))
	assert.False(t, isFreshEdit(&slack.Edited{}))
	assert.False(t, isFreshEdit(&slack.Edited{Timestamp: "garbage"}))
	assert.True(t, isFreshEdit(ts(10*time.Second)))
	assert.False(t, isFreshEdit(ts(config.EditMaxAge+time.Minute)))
	assert.False(t, isFreshEdit(ts(30*24*time.Hour)))
}

func TestReplaceOutgoingMentions(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(ioutil.Discard)
	cfg := &bridge.Config{Bridge: &bridge.Bridge{Log: logrus.NewEntry(logger)}}
	b := newBridge(cfg)
	b.users = newUserManager(logrus.NewEntry(logger), nil)
	for _, u := range []*slack.User{
		{ID: "U1", Name: "jsmith", RealName: "John Smith", Profile: slack.UserProfile{DisplayName: "Johnny"}},
		{ID: "U2", Name: "kim", Profile: slack.UserProfile{RealName: "Kim Doe"}},
	} {
		b.users.users[u.ID] = u
		b.users.nameIndex.Add(u.ID, userNames(u)...)
	}

	// Discord sends "@" + nickname, which may contain spaces.
	assert.Equal(t, "hi <@U1> and <@U2>!", b.replaceOutgoingMentions("hi @John Smith and @kim_doe!"))
	assert.Equal(t, "<@U1>", b.replaceOutgoingMentions("@johnny"))
	assert.Equal(t, "@stranger", b.replaceOutgoingMentions("@stranger"))
	assert.Equal(t, "a@b.com", b.replaceOutgoingMentions("a@b.com"))
}

// A failed refresh must not leave refreshInProgress set: populateUsers(true)
// would then wait forever and block the Slack event loop.
func TestPopulateRefreshFlagClearedOnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":false,"error":"invalid_auth"}`)
	}))
	defer server.Close()
	logger := logrus.New()
	logger.SetOutput(ioutil.Discard)
	sc := slack.New("xoxb-test", slack.OptionAPIURL(server.URL+"/"))

	u := newUserManager(logrus.NewEntry(logger), sc)
	u.populateUsers(true)
	assert.False(t, u.refreshInProgress)

	c := newChannelManager(logrus.NewEntry(logger), sc)
	c.populateChannels(true)
	assert.False(t, c.refreshInProgress)

	done := make(chan struct{})
	go func() {
		u.populateUsers(true) // used to block forever here
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("populateUsers blocked after a failed refresh")
	}
}

func TestReplaceURL(t *testing.T) {
	b := &Bslack{}
	cases := map[string]string{
		"see <https://example.com/a|example.com/a> now":   "see https://example.com/a now",
		"<https://example.com/|https://example.com>":      "https://example.com/",
		"<https://example.com>":                           "https://example.com",
		"<https://example.com/docs|the docs>":             "[the docs](https://example.com/docs)",
		"<https://x.com/?a=1&amp;b=2|x.com/?a=1&amp;b=2>": "https://x.com/?a=1&amp;b=2",
		"<mailto:bob@example.com|bob@example.com>":        "mailto:bob@example.com",
		"<tel:+15551234|call us>":                         "[call us](tel:+15551234)",
		"two <https://a.com|a.com> and <https://b.com|B>": "two https://a.com and [B](https://b.com)",
		"<#C123|general> and <@U123> stay":                "<#C123|general> and <@U123> stay",
		"<!here> stays":                                   "<!here> stays",
	}
	for in, want := range cases {
		assert.Equal(t, want, b.replaceURL(in), in)
	}
}

func TestReplaceChannelByName(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(ioutil.Discard)
	b := &Bslack{channels: newChannelManager(logrus.NewEntry(logger), nil)}
	b.channels.channelsByID["C111"] = &slack.Channel{GroupConversation: slack.GroupConversation{Name: "v-hudu"}}

	// Only real channel links are marked, by their exact Slack name.
	assert.Equal(t, "see <#channel:v-hudu> now", b.replaceChannelByName("see <#C111|> now"))
	assert.Equal(t, "typed #v-hudu stays", b.replaceChannelByName("typed #v-hudu stays"))
}

func TestSlackMentionsUseUserRefs(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(ioutil.Discard)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":false,"error":"user_not_found"}`)
	}))
	defer server.Close()
	sc := slack.New("xoxb-test", slack.OptionAPIURL(server.URL+"/"))
	b := &Bslack{users: newUserManager(logrus.NewEntry(logger), sc)}
	u := &slack.User{ID: "U1", Name: "mendy", RealName: "Mendy Green", Profile: slack.UserProfile{DisplayName: "Mendy Green [MSPGeek]"}}
	b.users.users[u.ID] = u
	b.users.nameIndex.Add(u.ID, userNames(u)...)

	// Incoming: a real Slack mention becomes a reference with all names.
	out := b.replaceMention("hi <@U1>, ok")
	var names []string
	rest := helper.ReplaceUserRefs(out, func(n []string) (string, bool) { names = n; return "X", true })
	assert.Equal(t, "hi X, ok", rest)
	assert.Equal(t, "Mendy Green [MSPGeek]", names[0])
	assert.Contains(t, names, "mendy")
	assert.Contains(t, names, "Mendy Green")

	// Unknown user IDs are left as they are.
	assert.Equal(t, "<@U9>", b.replaceMention("<@U9>"))

	// Outgoing: a reference from Discord resolves to the Slack user.
	mention, ok := b.UserMention([]string{"Mendy G", "mendy"})
	assert.True(t, ok)
	assert.Equal(t, "<@U1>", mention)
}

// During startup the connected event starts a channel refresh while the router
// already calls JoinChannel. JoinChannel must wait for that refresh instead of
// failing with "channel not found", which removed the Slack bridge.
func TestJoinChannelWaitsForRunningRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"channels":[{"id":"C1","name":"general","is_channel":true,"is_member":true}],"response_metadata":{"next_cursor":""}}`)
	}))
	defer server.Close()
	logger := logrus.New()
	logger.SetOutput(ioutil.Discard)
	sc := slack.New("xoxb-test", slack.OptionAPIURL(server.URL+"/"))
	b := &Bslack{sc: sc, channels: newChannelManager(logrus.NewEntry(logger), sc)}

	go b.channels.populateChannels(true) // the connected event's refresh
	time.Sleep(50 * time.Millisecond)

	assert.NoError(t, b.JoinChannel(config.ChannelInfo{Name: "general"}))
}
