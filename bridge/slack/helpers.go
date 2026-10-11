package bslack

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mspgeek-community/matterbridge/bridge/config"
	"github.com/mspgeek-community/matterbridge/bridge/helper"
	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
)

// populateReceivedMessage shapes the initial Matterbridge message that we will forward to the
// router before we apply message-dependent modifications.
func (b *Bslack) populateReceivedMessage(ev *slack.MessageEvent) (*config.Message, error) {
	// Use our own func because rtm.GetChannelInfo doesn't work for private channels.
	channel, err := b.channels.getChannelByID(ev.Channel)
	if err != nil {
		return nil, err
	}
	rmsg := &config.Message{
		Text:     ev.Text,
		Channel:  channel.Name,
		Account:  b.Account,
		ID:       ev.Timestamp,
		Extra:    make(map[string][]interface{}),
		ParentID: ev.ThreadTimestamp,
		Protocol: b.Protocol,
	}
	if b.useChannelID {
		rmsg.Channel = "ID:" + channel.ID
	}

	// Handle 'edit' messages.
	if ev.SubMessage != nil && !b.GetBool(editDisableConfig) {
		rmsg.ID = ev.SubMessage.Timestamp
		if ev.SubMessage.ThreadTimestamp != ev.SubMessage.Timestamp {
			b.Log.Debugf("SubMessage %#v", ev.SubMessage)
			rmsg.Text = ev.SubMessage.Text + b.GetString(editSuffixConfig)
		}
	}

	// For edits, only submessage has thread ts.
	// Ensures edits to threaded messages maintain their prefix hint on the
	// unthreaded end.
	if ev.SubMessage != nil {
		rmsg.ParentID = ev.SubMessage.ThreadTimestamp
	}

	if err = b.populateMessageWithUserInfo(ev, rmsg); err != nil {
		b.Log.Debugf("Error populating message: %v", err)
	} else {
		b.Log.Debugf("Message after processing: %#v", rmsg)
	}
	return rmsg, err
}

func (b *Bslack) populateMessageWithUserInfo(ev *slack.MessageEvent, rmsg *config.Message) error {
	if ev.SubType == sMessageDeleted || ev.SubType == sFileComment {
		b.Log.Debugf("Detected deletion event: %#v", ev)
		return nil
	}
	// First, deal with bot-originating messages but only do so when not using webhooks: we
	// would not be able to distinguish which bot would be sending them.
	if err := b.populateMessageWithBotInfo(ev, rmsg); err != nil {
		return err
	}

	// Second, deal with "real" users if we have the necessary information.
	var userID string
	switch {
	case ev.User != "":
		userID = ev.User
	case ev.SubMessage != nil && ev.SubMessage.User != "":
		userID = ev.SubMessage.User
	default:
		return nil
	}

	user := b.users.getUser(userID)
	if user == nil {
		return fmt.Errorf("could not find information for user with id %s", ev.User)
	}

	rmsg.UserID = user.ID
	rmsg.Username = user.Name
	if user.Profile.DisplayName != "" {
		rmsg.Username = user.Profile.DisplayName
	}
	if b.GetBool("UseFullName") && user.Profile.RealName != "" {
		rmsg.Username = user.Profile.RealName
	}
	return nil
}

func (b *Bslack) populateMessageWithBotInfo(ev *slack.MessageEvent, rmsg *config.Message) error {
	if ev.BotID == "" || b.GetString(outgoingWebhookConfig) != "" {
		return nil
	}

	var err error
	var bot *slack.Bot
	for {
		bot, err = b.rtm.GetBotInfo(slack.GetBotInfoParameters{Bot: ev.BotID})
		if err == nil {
			break
		}

		if err = handleRateLimit(b.Log, err); err != nil {
			b.Log.Errorf("Could not retrieve bot information: %#v", err)
			return err
		}
	}
	b.Log.Debugf("Found bot %#v", bot)

	if bot.Name != "" {
		rmsg.Username = bot.Name
		if ev.Username != "" {
			rmsg.Username = ev.Username
		}
		rmsg.UserID = bot.ID
	}
	return nil
}

var (
	mentionRE        = regexp.MustCompile(`<@([a-zA-Z0-9]+)>`)
	channelRE        = regexp.MustCompile(`<#[a-zA-Z0-9]+\|(.+?)>`)
	ChannelREAuto    = regexp.MustCompile(`<#[A-Z0-9]+\|>`)
	variableRE       = regexp.MustCompile(`<!((?:subteam\^)?[a-zA-Z0-9]+)(?:\|@?(.+?))?>`)
	urlRE            = regexp.MustCompile(`<([a-zA-Z][a-zA-Z0-9+.-]*:[^<>|\s]+)(?:\|([^>]*))?>`)
	codeFenceRE      = regexp.MustCompile(`(?m)^` + "```" + `\w+$`)
	topicOrPurposeRE = regexp.MustCompile(`(?s)(@.+) (cleared|set)(?: the)? channel (topic|purpose)(?:: (.*))?`)
)

func (b *Bslack) extractTopicOrPurpose(text string) (string, string) {
	r := topicOrPurposeRE.FindStringSubmatch(text)
	if len(r) == 5 {
		action, updateType, extracted := r[2], r[3], r[4]
		switch action {
		case "set":
			return updateType, extracted
		case "cleared":
			return updateType, ""
		}
	}
	b.Log.Warnf("Encountered channel topic or purpose change message with unexpected format: %s", text)
	return "unknown", ""
}

// @see https://api.slack.com/docs/message-formatting#linking_to_channels_and_users
func (b *Bslack) replaceMention(text string) string {
	replaceFunc := func(match string) string {
		userID := strings.Trim(match, "@<>")
		user := b.users.getUser(userID)
		if user == nil {
			return match
		}
		// Carry all of the user's names so the destination can link the
		// right person without guessing where the name ends.
		return helper.UserRef(append([]string{slackDisplayName(user)}, userNames(user)...)...)
	}
	return mentionRE.ReplaceAllStringFunc(text, replaceFunc)
}

// slackDisplayName is the name Slack shows for a user.
func slackDisplayName(user *slack.User) string {
	switch {
	case user.Profile.DisplayName != "":
		return user.Profile.DisplayName
	case user.RealName != "":
		return user.RealName
	}
	return user.Name
}

// UserMention returns the Slack mention of the user with these names (a user
// reference from another platform).
func (b *Bslack) UserMention(names []string) (string, bool) {
	if b.users == nil {
		return "", false
	}
	b.users.usersMutex.RLock()
	id, ok := helper.ResolveUserByNames(b.users.nameIndex, names)
	b.users.usersMutex.RUnlock()
	if !ok {
		return "", false
	}
	return "<@" + id + ">", true
}

// replaceOutgoingMentions turns "@name" from other platforms into a Slack
// mention when name matches a user's display name, real name or username
// (ignoring case, spaces and punctuation).
func (b *Bslack) replaceOutgoingMentions(text string) string {
	if b.users == nil {
		return text
	}
	return helper.ReplaceMentions(text, func(name string) (string, bool) {
		id, ok := b.users.lookupByName(name)
		if !ok {
			return "", false
		}
		return "<@" + id + ">", true
	})
}

// @see https://api.slack.com/docs/message-formatting#linking_to_channels_and_users
func (b *Bslack) replaceChannel(text string) string {
	for _, r := range channelRE.FindAllStringSubmatch(text, -1) {
		text = strings.Replace(text, r[0], "#"+r[1], 1)
	}
	return text
}

// replaceChannelByName turns Slack channel links without a label ("<#C123|>")
// into a channel reference carrying the exact Slack channel name. The gateway
// turns it into the destination's channel link when the destination has a
// channel with exactly that name (Discord), and into "#name" otherwise.
func (b *Bslack) replaceChannelByName(text string) string {
	return ChannelREAuto.ReplaceAllStringFunc(text, func(match string) string {
		channelID := strings.TrimSuffix(strings.TrimPrefix(match, "<#"), "|>")
		channel, err := b.channels.getChannelByID(channelID)
		if err != nil {
			return match
		}
		return helper.ChannelRef(channel.Name)
	})
}

// @see https://api.slack.com/docs/message-formatting#variables
func (b *Bslack) replaceVariable(text string) string {
	for _, r := range variableRE.FindAllStringSubmatch(text, -1) {
		if r[2] != "" {
			text = strings.Replace(text, r[0], "@"+r[2], 1)
		} else {
			text = strings.Replace(text, r[0], "@"+r[1], 1)
		}
	}
	return text
}

// replaceURL converts Slack links, <url|label> and <url>, to Markdown. A link
// whose label is just the URL becomes the plain URL so other platforms show
// their normal link preview instead of a masked link.
// @see https://api.slack.com/docs/message-formatting#linking_to_urls
func (b *Bslack) replaceURL(text string) string {
	return urlRE.ReplaceAllStringFunc(text, func(match string) string {
		sub := urlRE.FindStringSubmatch(match)
		url, label := sub[1], sub[2]
		if label == "" || sameLink(url, label) {
			return url
		}
		return "[" + label + "](" + url + ")"
	})
}

// sameLink reports whether label only repeats url, as Slack does for links
// typed into a message ("example.com/a" for "https://example.com/a").
func sameLink(url, label string) bool {
	norm := func(s string) string {
		s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
		s = strings.TrimPrefix(s, "mailto:")
		return strings.TrimSuffix(s, "/")
	}
	return norm(url) == norm(label)
}

func (b *Bslack) replaceb0rkedMarkDown(text string) string {
	// taken from https://github.com/mattermost/mattermost-server/blob/master/app/slackimport.go
	//
	regexReplaceAllString := []struct {
		regex *regexp.Regexp
		rpl   string
	}{
		// bold
		{
			regexp.MustCompile(`(^|[\s.;,])\*(\S[^*\n]+)\*`),
			"$1**$2**",
		},
		// strikethrough
		{
			regexp.MustCompile(`(^|[\s.;,])\~(\S[^~\n]+)\~`),
			"$1~~$2~~",
		},
		// single paragraph blockquote
		// Slack converts > character to &gt;
		{
			regexp.MustCompile(`(?sm)^&gt;`),
			">",
		},
	}
	for _, rule := range regexReplaceAllString {
		text = rule.regex.ReplaceAllString(text, rule.rpl)
	}
	return text
}

func (b *Bslack) replaceCodeFence(text string) string {
	return codeFenceRE.ReplaceAllString(text, "```")
}

// getUsersInConversation returns an array of userIDs that are members of channelID
func (b *Bslack) getUsersInConversation(channelID string) ([]string, error) {
	channelMembers := []string{}
	for {
		queryParams := &slack.GetUsersInConversationParameters{
			ChannelID: channelID,
		}

		members, nextCursor, err := b.sc.GetUsersInConversation(queryParams)
		if err != nil {
			if err = handleRateLimit(b.Log, err); err != nil {
				return channelMembers, fmt.Errorf("Could not retrieve users in channels: %#v", err)
			}
			continue
		}

		channelMembers = append(channelMembers, members...)

		if nextCursor == "" {
			break
		}
		queryParams.Cursor = nextCursor
	}
	return channelMembers, nil
}

func handleRateLimit(log *logrus.Entry, err error) error {
	rateLimit, ok := err.(*slack.RateLimitedError)
	if !ok {
		return err
	}
	log.Infof("Rate-limited by Slack. Sleeping for %v", rateLimit.RetryAfter)
	time.Sleep(rateLimit.RetryAfter)
	return nil
}
