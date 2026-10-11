package gateway

import (
	"fmt"
	"io/ioutil"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/d5/tengo/v2"
	"github.com/d5/tengo/v2/stdlib"
	lru "github.com/hashicorp/golang-lru"
	"github.com/kyokomi/emoji/v2"
	"github.com/mspgeek-community/matterbridge/bridge"
	"github.com/mspgeek-community/matterbridge/bridge/config"
	"github.com/mspgeek-community/matterbridge/bridge/helper"
	"github.com/mspgeek-community/matterbridge/internal"

	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
)

type Gateway struct {
	config.Config

	Router         *Router
	MyConfig       *config.Gateway
	Bridges        map[string]*bridge.Bridge
	Channels       map[string]*config.ChannelInfo
	ChannelOptions map[string]config.ChannelOptions
	Message        chan config.Message
	Name           string
	Messages       *lru.Cache
	logger         *logrus.Entry
}

type BrMsgID struct {
	br        *bridge.Bridge
	ID        string
	ChannelID string
}

const apiProtocol = "api"

// New creates a new Gateway object associated with the specified router and
// following the given configuration.
func New(rootLogger *logrus.Logger, cfg *config.Gateway, r *Router) *Gateway {
	logger := rootLogger.WithFields(logrus.Fields{"prefix": "gateway"})

	cache, _ := lru.New(5000)
	gw := &Gateway{
		Channels: make(map[string]*config.ChannelInfo),
		Message:  r.Message,
		Router:   r,
		Bridges:  make(map[string]*bridge.Bridge),
		Config:   r.Config,
		Messages: cache,
		logger:   logger,
	}

	if err := gw.AddConfig(cfg); err != nil {
		logger.Errorf("Failed to add configuration to gateway: %#v", err)
	}
	return gw
}

// lookupIDs returns the bridged copies of the message stored under key, from
// the in-memory cache or the persistent message store.
func (gw *Gateway) lookupIDs(key string) ([]*BrMsgID, bool) {
	if res, ok := gw.Messages.Get(key); ok {
		return res.([]*BrMsgID), true
	}
	store := gw.Router.store
	if store == nil {
		return nil, false
	}
	stored, err := store.load(gw.Name, key)
	if err != nil {
		gw.logger.Errorf("Loading message mapping %s failed: %s", key, err)
		return nil, false
	}
	if len(stored) == 0 {
		return nil, false
	}
	ids := make([]*BrMsgID, 0, len(stored))
	for _, item := range stored {
		br, ok := gw.Bridges[item.Account]
		if !ok {
			continue
		}
		ids = append(ids, &BrMsgID{br: br, ID: item.ID, ChannelID: item.ChannelID})
	}
	gw.Messages.Add(key, ids)
	return ids, true
}

// rememberIDs stores the bridged copies of the message stored under key in
// the in-memory cache and the persistent message store.
func (gw *Gateway) rememberIDs(key string, ids []*BrMsgID) {
	gw.Messages.Add(key, ids)
	store := gw.Router.store
	if store == nil {
		return
	}
	stored := make([]storedMsgID, 0, len(ids))
	for _, id := range ids {
		stored = append(stored, storedMsgID{Account: id.br.Account, ID: id.ID, ChannelID: id.ChannelID})
	}
	if err := store.save(gw.Name, key, stored); err != nil {
		gw.logger.Errorf("Saving message mapping %s failed: %s", key, err)
	}
}

// FindCanonicalMsgID returns the ID under which a message was stored in the cache.
func (gw *Gateway) FindCanonicalMsgID(protocol string, mID string) string {
	ID := protocol + " " + mID
	if gw.Messages.Contains(ID) {
		return ID
	}

	// If not keyed, iterate through cache for downstream, and infer upstream.
	for _, mid := range gw.Messages.Keys() {
		v, _ := gw.Messages.Peek(mid)
		ids := v.([]*BrMsgID)
		for _, downstreamMsgObj := range ids {
			if ID == downstreamMsgObj.ID {
				return mid.(string)
			}
		}
	}

	// Fall back to the persistent store for messages no longer in memory.
	if _, ok := gw.lookupIDs(ID); ok {
		return ID
	}
	if store := gw.Router.store; store != nil {
		canonical, err := store.canonicalFor(gw.Name, ID)
		if err != nil {
			gw.logger.Errorf("Looking up message %s failed: %s", ID, err)
		}
		return canonical
	}
	return ""
}

// AddBridge sets up a new bridge in the gateway object with the specified configuration.
func (gw *Gateway) AddBridge(cfg *config.Bridge) error {
	br := gw.Router.getBridge(cfg.Account)
	if br == nil {
		gw.checkConfig(cfg)
		br = bridge.New(cfg)
		br.Config = gw.Router.Config
		br.General = &gw.BridgeValues().General
		br.Log = gw.logger.WithFields(logrus.Fields{"prefix": br.Protocol})
		brconfig := &bridge.Config{
			Remote: gw.Message,
			Bridge: br,
		}
		// add the actual bridger for this protocol to this bridge using the bridgeMap
		if _, ok := gw.Router.BridgeMap[br.Protocol]; !ok {
			gw.logger.Fatalf("Incorrect protocol %s specified in gateway configuration %s, exiting.", br.Protocol, cfg.Account)
		}
		br.Bridger = gw.Router.BridgeMap[br.Protocol](brconfig)
	}
	gw.mapChannelsToBridge(br)
	gw.Bridges[cfg.Account] = br
	return nil
}

func (gw *Gateway) checkConfig(cfg *config.Bridge) {
	match := false
	for _, key := range gw.Router.Config.Viper().AllKeys() {
		if strings.HasPrefix(key, strings.ToLower(cfg.Account)) {
			match = true
			break
		}
	}
	if !match {
		gw.logger.Fatalf("Account %s defined in gateway %s but no configuration found, exiting.", cfg.Account, gw.Name)
	}
}

// AddConfig associates a new configuration with the gateway object.
func (gw *Gateway) AddConfig(cfg *config.Gateway) error {
	gw.Name = cfg.Name
	gw.MyConfig = cfg
	if err := gw.mapChannels(); err != nil {
		gw.logger.Errorf("mapChannels() failed: %s", err)
	}
	for _, br := range append(gw.MyConfig.In, append(gw.MyConfig.InOut, gw.MyConfig.Out...)...) {
		br := br // scopelint
		err := gw.AddBridge(&br)
		if err != nil {
			return err
		}
	}
	return nil
}

func (gw *Gateway) mapChannelsToBridge(br *bridge.Bridge) {
	for ID, channel := range gw.Channels {
		if br.Account == channel.Account {
			br.Channels[ID] = *channel
		}
	}
}

func (gw *Gateway) reconnectBridge(br *bridge.Bridge) {
	if err := br.Disconnect(); err != nil {
		gw.logger.Errorf("Disconnect() %s failed: %s", br.Account, err)
	}
	time.Sleep(time.Second * 5)
RECONNECT:
	gw.logger.Infof("Reconnecting %s", br.Account)
	err := br.Connect()
	if err != nil {
		gw.logger.Errorf("Reconnection failed: %s. Trying again in 60 seconds", err)
		time.Sleep(time.Second * 60)
		goto RECONNECT
	}
	br.Joined = make(map[string]bool)
	if err := br.JoinChannels(); err != nil {
		gw.logger.Errorf("JoinChannels() %s failed: %s", br.Account, err)
	}
}

func (gw *Gateway) handleReceive(msg config.Message) {
	gw.logger.Debugf("Received message: %#v", msg)
}

func (gw *Gateway) mapChannelConfig(cfg []config.Bridge, direction string) {
	for _, br := range cfg {
		if isAPI(br.Account) {
			br.Channel = apiProtocol
		}
		// make sure to lowercase irc channels in config #348
		if strings.HasPrefix(br.Account, "irc.") {
			br.Channel = strings.ToLower(br.Channel)
		}
		if strings.HasPrefix(br.Account, "mattermost.") && strings.HasPrefix(br.Channel, "#") {
			gw.logger.Errorf("Mattermost channels do not start with a #: remove the # in %s", br.Channel)
			os.Exit(1)
		}
		if strings.HasPrefix(br.Account, "zulip.") && !strings.Contains(br.Channel, "/topic:") {
			gw.logger.Errorf("Breaking change, since matterbridge 1.14.0 zulip channels need to specify the topic with channel/topic:mytopic in %s of %s", br.Channel, br.Account)
			os.Exit(1)
		}
		ID := br.Channel + br.Account
		if _, ok := gw.Channels[ID]; !ok {
			channel := &config.ChannelInfo{
				Name:        br.Channel,
				Direction:   direction,
				ID:          ID,
				Options:     br.Options,
				Account:     br.Account,
				SameChannel: make(map[string]bool),
			}
			channel.SameChannel[gw.Name] = br.SameChannel
			gw.Channels[channel.ID] = channel
		} else {
			// if we already have a key and it's not our current direction it means we have a bidirectional inout
			if gw.Channels[ID].Direction != direction {
				gw.Channels[ID].Direction = "inout"
			}
		}
		gw.Channels[ID].SameChannel[gw.Name] = br.SameChannel
	}
}

func (gw *Gateway) mapChannels() error {
	gw.mapChannelConfig(gw.MyConfig.In, "in")
	gw.mapChannelConfig(gw.MyConfig.Out, "out")
	gw.mapChannelConfig(gw.MyConfig.InOut, "inout")
	return nil
}

func (gw *Gateway) getDestChannel(msg *config.Message, dest bridge.Bridge) []config.ChannelInfo {
	var channels []config.ChannelInfo

	// for messages received from the api check that the gateway is the specified one
	if msg.Protocol == apiProtocol && gw.Name != msg.Gateway {
		return channels
	}

	// discord join/leave is for the whole bridge, isn't a per channel join/leave
	if msg.Event == config.EventJoinLeave && getProtocol(msg) == "discord" && msg.Channel == "" {
		for _, channel := range gw.Channels {
			if channel.Account == dest.Account && strings.Contains(channel.Direction, "out") &&
				gw.validGatewayDest(msg) {
				channels = append(channels, *channel)
			}
		}
		return channels
	}

	// if source channel is in only, do nothing
	for _, channel := range gw.Channels {
		// lookup the channel from the message
		if channel.ID == getChannelID(msg) {
			// we only have destinations if the original message is from an "in" (sending) channel
			if !strings.Contains(channel.Direction, "in") {
				return channels
			}
			continue
		}
	}
	for _, channel := range gw.Channels {
		if _, ok := gw.Channels[getChannelID(msg)]; !ok {
			continue
		}

		// do samechannelgateway logic
		if channel.SameChannel[msg.Gateway] {
			if msg.Channel == channel.Name && msg.Account != dest.Account {
				channels = append(channels, *channel)
			}
			continue
		}
		if strings.Contains(channel.Direction, "out") && channel.Account == dest.Account && gw.validGatewayDest(msg) {
			channels = append(channels, *channel)
		}
	}
	return channels
}

func (gw *Gateway) getDestMsgID(msgID string, dest *bridge.Bridge, channel *config.ChannelInfo) string {
	//log the message ID to the console, together with the new channel name, and the bridge name
	if IDs, ok := gw.lookupIDs(msgID); ok {
		for _, id := range IDs {
			if id == nil || id.br == nil {
				continue
			}
			// check protocol, bridge name and channelname
			// for people that reuse the same bridge multiple times. see #342
			//if dest.Protocol == id.br.Protocol && dest.Name == id.br.Name && channel.ID == id.ChannelID {
			if dest.Protocol == id.br.Protocol && dest.Name == id.br.Name {
				//print the results and the destination.
				destMsg := strings.Replace(id.ID, dest.Protocol+" ", "", 1)
				return destMsg
			}
		}
	}
	return ""
}

// ignoreTextEmpty returns true if we need to ignore a message with an empty text.
func (gw *Gateway) ignoreTextEmpty(msg *config.Message) bool {
	if msg.Text != "" {
		return false
	}
	if msg.Event == config.EventUserTyping {
		return false
	}
	// we have an attachment or actual bytes, do not ignore
	if msg.Extra != nil &&
		(msg.Extra["attachments"] != nil ||
			len(msg.Extra["file"]) > 0 ||
			len(msg.Extra[config.EventFileFailureSize]) > 0) {
		return false
	}
	gw.logger.Debugf("ignoring empty message %#v from %s", msg, msg.Account)
	return true
}

func (gw *Gateway) ignoreMessage(msg *config.Message) bool {
	// if we don't have the bridge, ignore it
	if _, ok := gw.Bridges[msg.Account]; !ok {
		return true
	}

	igNicks := strings.Fields(gw.Bridges[msg.Account].GetString("IgnoreNicks"))
	igMessages := strings.Fields(gw.Bridges[msg.Account].GetString("IgnoreMessages"))
	if gw.ignoreTextEmpty(msg) || gw.ignoreText(msg.Username, igNicks) || gw.ignoreText(msg.Text, igMessages) || gw.ignoreFilesComment(msg.Extra, igMessages) {
		return true
	}

	return false
}

// ignoreFilesComment returns true if we need to ignore a file with matched comment.
func (gw *Gateway) ignoreFilesComment(extra map[string][]interface{}, igMessages []string) bool {
	if extra == nil {
		return false
	}
	for _, f := range extra["file"] {
		fi, ok := f.(config.FileInfo)
		if !ok {
			continue
		}
		if gw.ignoreText(fi.Comment, igMessages) {
			return true
		}
	}
	return false
}

func (gw *Gateway) modifyUsername(msg *config.Message, dest *bridge.Bridge) string {
	if dest.GetBool("StripNick") {
		re := regexp.MustCompile("[^a-zA-Z0-9]+")
		msg.Username = re.ReplaceAllString(msg.Username, "")
	}
	nick := dest.GetString("RemoteNickFormat")

	// loop to replace nicks
	br := gw.Bridges[msg.Account]
	for _, outer := range br.GetStringSlice2D("ReplaceNicks") {
		search := outer[0]
		replace := outer[1]
		// TODO move compile to bridge init somewhere
		re, err := regexp.Compile(search)
		if err != nil {
			gw.logger.Errorf("regexp in %s failed: %s", msg.Account, err)
			break
		}
		msg.Username = re.ReplaceAllString(msg.Username, replace)
	}

	if len(msg.Username) > 0 {
		// fix utf-8 issue #193
		i := 0
		for index := range msg.Username {
			if i == 1 {
				i = index
				break
			}
			i++
		}
		nick = strings.ReplaceAll(nick, "{NOPINGNICK}", msg.Username[:i]+"\u200b"+msg.Username[i:])
	}

	nick = strings.ReplaceAll(nick, "{BRIDGE}", br.Name)
	nick = strings.ReplaceAll(nick, "{PROTOCOL}", br.Protocol)
	nick = strings.ReplaceAll(nick, "{GATEWAY}", gw.Name)
	nick = strings.ReplaceAll(nick, "{LABEL}", br.GetString("Label"))
	nick = strings.ReplaceAll(nick, "{NICK}", msg.Username)
	nick = strings.ReplaceAll(nick, "{USERID}", msg.UserID)
	nick = strings.ReplaceAll(nick, "{CHANNEL}", msg.Channel)
	tengoNick, err := gw.modifyUsernameTengo(msg, br)
	if err != nil {
		gw.logger.Errorf("modifyUsernameTengo error: %s", err)
	}
	nick = strings.ReplaceAll(nick, "{TENGO}", tengoNick)
	return nick
}

func (gw *Gateway) modifyAvatar(msg *config.Message, dest *bridge.Bridge) string {
	iconurl := dest.GetString("IconURL")
	iconurl = strings.Replace(iconurl, "{NICK}", msg.Username, -1)
	if msg.Avatar == "" {
		msg.Avatar = iconurl
	}
	return msg.Avatar
}

func (gw *Gateway) modifyMessage(msg *config.Message) {
	if gw.BridgeValues().General.TengoModifyMessage != "" {
		gw.logger.Warnf("General TengoModifyMessage=%s is deprecated and will be removed in v1.20.0, please move to Tengo InMessage=%s", gw.BridgeValues().General.TengoModifyMessage, gw.BridgeValues().General.TengoModifyMessage)
	}

	if err := modifyInMessageTengo(gw.BridgeValues().General.TengoModifyMessage, msg); err != nil {
		gw.logger.Errorf("TengoModifyMessage failed: %s", err)
	}

	inMessage := gw.BridgeValues().Tengo.InMessage
	if inMessage == "" {
		inMessage = gw.BridgeValues().Tengo.Message
		if inMessage != "" {
			gw.logger.Warnf("Tengo Message=%s is deprecated and will be removed in v1.20.0, please move to Tengo InMessage=%s", inMessage, inMessage)
		}
	}

	if err := modifyInMessageTengo(inMessage, msg); err != nil {
		gw.logger.Errorf("Tengo.Message failed: %s", err)
	}

	// replace :emoji: to unicode
	emoji.ReplacePadding = ""
	msg.Text = emoji.Sprint(msg.Text)

	br := gw.Bridges[msg.Account]
	// loop to replace messages
	for _, outer := range br.GetStringSlice2D("ReplaceMessages") {
		search := outer[0]
		replace := outer[1]
		// TODO move compile to bridge init somewhere
		re, err := regexp.Compile(search)
		if err != nil {
			gw.logger.Errorf("regexp in %s failed: %s", msg.Account, err)
			break
		}
		msg.Text = re.ReplaceAllString(msg.Text, replace)
	}

	gw.handleExtractNicks(msg)

	// messages from api have Gateway specified, don't overwrite
	if msg.Protocol != apiProtocol {
		msg.Gateway = gw.Name
	}
}

// tsRefRE matches thread references ("<#TS:1700000000.000100>").
var tsRefRE = regexp.MustCompile(`<#TS:([0-9]+\.[0-9]+)>`)

func (gw *Gateway) ReplaceTSString(
	canonicalParentMsgID string,
	canonicalThreadMsgID string,
	Text string,
	dest *bridge.Bridge,
	channel *config.ChannelInfo,

) string {
	Text = tsRefRE.ReplaceAllStringFunc(Text, func(s string) string {
		gw.logger.Debugf("Replacing thread reference with canonicalThreadMsgID %s", canonicalThreadMsgID)
		var destMsgID string
		if strings.Contains(canonicalThreadMsgID, "slack") {
			destMsgID = gw.getDestMsgID(canonicalThreadMsgID, dest, channel)
		} else {
			destMsgID = strings.Replace(canonicalThreadMsgID, "discord ", "", 1)
		}
		return "<#" + destMsgID + ">"
	})
	return Text
}

// slackForwardTS returns the timestamp of the original message when msg is a
// Slack forward, or "" otherwise.
func slackForwardTS(msg *config.Message) string {
	for _, attachment := range msg.Extra["slack_attachment"] {
		if attachments, ok := attachment.([]slack.Attachment); ok {
			for _, attach := range attachments {
				if attach.FromURL != "" {
					return attach.Ts.String()
				}
			}
		}
	}
	return ""
}

// channelMentioner is implemented by bridges that can link channels by name.
type channelMentioner interface {
	ChannelMention(name string) (string, bool)
}

// userMentioner is implemented by bridges that can mention users by name.
type userMentioner interface {
	UserMention(names []string) (string, bool)
}

// resolveRefs replaces channel and user references in the text and file
// comments of msg with dest's links and mentions, or plain "#name"/"@name"
// when dest cannot link them.
func resolveRefs(msg *config.Message, dest *bridge.Bridge) {
	var resolveChannel func(string) (string, bool)
	if mentioner, ok := dest.Bridger.(channelMentioner); ok {
		resolveChannel = mentioner.ChannelMention
	}
	var resolveUser func([]string) (string, bool)
	if mentioner, ok := dest.Bridger.(userMentioner); ok {
		resolveUser = mentioner.UserMention
	}
	resolve := func(text string) string {
		text = helper.ReplaceChannelRefs(text, resolveChannel)
		return helper.ReplaceUserRefs(text, resolveUser)
	}
	msg.Text = resolve(msg.Text)

	files := msg.Extra["file"]
	needsCopy := false
	for _, f := range files {
		if fi, ok := f.(config.FileInfo); ok && (helper.HasChannelRefs(fi.Comment) || helper.HasUserRefs(fi.Comment)) {
			needsCopy = true
			break
		}
	}
	if !needsCopy {
		return
	}
	// Extra is shared by all destinations of the message; copy before changing.
	extra := make(map[string][]interface{}, len(msg.Extra))
	for k, v := range msg.Extra {
		extra[k] = v
	}
	newFiles := make([]interface{}, len(files))
	for i, f := range files {
		if fi, ok := f.(config.FileInfo); ok {
			fi.Comment = resolve(fi.Comment)
			newFiles[i] = fi
			continue
		}
		newFiles[i] = f
	}
	extra["file"] = newFiles
	msg.Extra = extra
}

// discordChannelResolver is implemented by the discord bridge.
type discordChannelResolver interface {
	ChannelIDByName(name string) string
	GuildID() string
}

// discordMessageURL returns a link to the Discord copy of the message stored
// under canonicalID, or to the Discord channel when that copy is unknown.
// It uses the running Discord bridge; it never opens a gateway connection.
func (gw *Gateway) discordMessageURL(canonicalID string, dest *bridge.Bridge, channel *config.ChannelInfo) string {
	resolver, ok := dest.Bridger.(discordChannelResolver)
	if !ok {
		return ""
	}
	guildID := resolver.GuildID()
	channelID := resolver.ChannelIDByName(channel.Name)
	if guildID == "" || channelID == "" {
		gw.logger.Debugf("Cannot build Discord link for channel %s", channel.Name)
		return ""
	}
	url := "https://discord.com/channels/" + guildID + "/" + channelID
	if msgID := gw.getDestMsgID(canonicalID, dest, channel); msgID != "" {
		url += "/" + msgID
	}
	return url
}

// SendMessage sends a message (with specified parentID) to the channel on the selected
// destination bridge and returns a message ID or an error.
func (gw *Gateway) SendMessage(
	rmsg *config.Message,
	dest *bridge.Bridge,
	channel *config.ChannelInfo,
	canonicalParentMsgID string,
	canonicalThreadMsgID string,
) (string, error) {
	msg := *rmsg
	// Only send the avatar download event to ourselves.
	if msg.Event == config.EventAvatarDownload {
		if channel.ID != getChannelID(rmsg) {
			return "", nil
		}
	} else {
		// do not send to ourself for any other event
		if channel.ID == getChannelID(rmsg) {
			return "", nil
		}
	}
	// Only send irc notices to irc
	if msg.Event == config.EventNoticeIRC && dest.Protocol != "irc" {
		return "", nil
	}

	// Too noisy to log like other events
	debugSendMessage := ""
	if msg.Event != config.EventUserTyping {
		debugSendMessage = fmt.Sprintf("=> Sending %#v from %s (%s) to %s (%s)", msg, msg.Account, rmsg.Channel, dest.Account, channel.Name)
	}

	msg.Channel = channel.Name
	msg.Avatar = gw.modifyAvatar(rmsg, dest)
	msg.Username = gw.modifyUsername(rmsg, dest)

	// exclude file delete event as the msg ID here is the native file ID that needs to be deleted
	if msg.Event != config.EventFileDelete {
		msg.ID = gw.getDestMsgID(rmsg.Protocol+" "+rmsg.ID, dest, channel)
	}

	// An edit of a message we have no bridged copy of must not be posted as a
	// new message: that replays old messages.
	if rmsg.Edit && msg.ID == "" {
		gw.logger.Debugf("Dropping edit of %s %s for %s: bridged message not found", rmsg.Protocol, rmsg.ID, dest.Account)
		return "", nil
	}

	// for api we need originchannel as channel
	if dest.Protocol == apiProtocol {
		msg.Channel = rmsg.Channel
	}

	msg.ParentID = gw.getDestMsgID(canonicalParentMsgID, dest, channel)
	if msg.ParentID == "" {
		msg.ParentID = strings.Replace(canonicalParentMsgID, dest.Protocol+" ", "", 1)
	}
	// Add this block to replace <#TS:(random timestamp here)> with <#(RESULTSFROMGW.getDestMsgID)>
	msg.Text = gw.ReplaceTSString(canonicalParentMsgID, canonicalThreadMsgID, msg.Text, dest, channel)
	resolveRefs(&msg, dest)

	// if the parentID is still empty and we have a parentID set in the original message
	// this means that we didn't find it in the cache so set it to a "msg-parent-not-found" constant
	if msg.ParentID == "" && rmsg.ParentID != "" {
		msg.ParentID = config.ParentIDNotFound
	}
	msg.ThreadID = gw.getDestMsgID(canonicalThreadMsgID, dest, channel)
	if msg.ThreadID == "" {
		msg.ThreadID = strings.Replace(canonicalThreadMsgID, dest.Protocol+" ", "", 1)
	}
	// A message forwarded inside Slack carries the original as an attachment
	// with FromURL set. Point Discord readers at the bridged original.
	if fromTS := slackForwardTS(&msg); fromTS != "" {
		if replyURL := gw.discordMessageURL("slack "+fromTS, dest, channel); replyURL != "" {
			msg.Text += "\n> *In reply to:* " + replyURL
		}
	}

	if msg.ThreadID == "" && rmsg.ParentID != "" {
		//msg.ThreadID = config.ParentIDNotFound
	}

	drop, err := gw.modifyOutMessageTengo(rmsg, &msg, dest)
	if err != nil {
		gw.logger.Errorf("modifySendMessageTengo: %s", err)
	}

	if drop {
		gw.logger.Debugf("=> Tengo dropping %#v from %s (%s) to %s (%s)", msg, msg.Account, rmsg.Channel, dest.Account, channel.Name)
		return "", nil
	}

	if debugSendMessage != "" {
		gw.logger.Debug(debugSendMessage)
	}
	// if we are using mattermost plugin account, send messages to MattermostPlugin channel
	// that can be picked up by the mattermost matterbridge plugin
	if dest.Account == "mattermost.plugin" {
		gw.Router.MattermostPlugin <- msg
	}

	defer func(t time.Time) {
		gw.logger.Debugf("=> Send from %s (%s) to %s (%s) took %s", msg.Account, rmsg.Channel, dest.Account, channel.Name, time.Since(t))
	}(time.Now())

	mID, err := dest.Send(msg)
	if err != nil {
		return mID, err
	}

	// append the message ID (mID) from this bridge (dest) to our brMsgIDs slice
	if mID != "" {
		gw.logger.Debugf("mID %s: %s", dest.Account, mID)
		return mID, nil
		// brMsgIDs = append(brMsgIDs, &BrMsgID{dest, dest.Protocol + " " + mID, channel.ID})
	}
	return "", nil
}

func (gw *Gateway) validGatewayDest(msg *config.Message) bool {
	return msg.Gateway == gw.Name
}

func getChannelID(msg *config.Message) string {
	return msg.Channel + msg.Account
}

func isAPI(account string) bool {
	return strings.HasPrefix(account, "api.")
}

// ignoreText returns true if text matches any of the input regexes.
func (gw *Gateway) ignoreText(text string, input []string) bool {
	for _, entry := range input {
		if entry == "" {
			continue
		}
		// TODO do not compile regexps everytime
		re, err := regexp.Compile(entry)
		if err != nil {
			gw.logger.Errorf("incorrect regexp %s", entry)
			continue
		}
		if re.MatchString(text) {
			gw.logger.Debugf("matching %s. ignoring %s", entry, text)
			return true
		}
	}
	return false
}

func getProtocol(msg *config.Message) string {
	p := strings.Split(msg.Account, ".")
	return p[0]
}

func modifyInMessageTengo(filename string, msg *config.Message) error {
	if filename == "" {
		return nil
	}
	res, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
	}
	s := tengo.NewScript(res)
	s.SetImports(stdlib.GetModuleMap(stdlib.AllModuleNames()...))
	_ = s.Add("msgText", msg.Text)
	_ = s.Add("msgUsername", msg.Username)
	_ = s.Add("msgUserID", msg.UserID)
	_ = s.Add("msgAccount", msg.Account)
	_ = s.Add("msgChannel", msg.Channel)
	c, err := s.Compile()
	if err != nil {
		return err
	}
	if err := c.Run(); err != nil {
		return err
	}
	msg.Text = c.Get("msgText").String()
	msg.Username = c.Get("msgUsername").String()
	return nil
}

func (gw *Gateway) modifyUsernameTengo(msg *config.Message, br *bridge.Bridge) (string, error) {
	filename := gw.BridgeValues().Tengo.RemoteNickFormat
	if filename == "" {
		return "", nil
	}
	res, err := ioutil.ReadFile(filename)
	if err != nil {
		return "", err
	}
	s := tengo.NewScript(res)
	s.SetImports(stdlib.GetModuleMap(stdlib.AllModuleNames()...))
	_ = s.Add("result", "")
	_ = s.Add("msgText", msg.Text)
	_ = s.Add("msgUsername", msg.Username)
	_ = s.Add("msgUserID", msg.UserID)
	_ = s.Add("nick", msg.Username)
	_ = s.Add("msgAccount", msg.Account)
	_ = s.Add("msgChannel", msg.Channel)
	_ = s.Add("channel", msg.Channel)
	_ = s.Add("msgProtocol", msg.Protocol)
	_ = s.Add("remoteAccount", br.Account)
	_ = s.Add("protocol", br.Protocol)
	_ = s.Add("bridge", br.Name)
	_ = s.Add("gateway", gw.Name)
	c, err := s.Compile()
	if err != nil {
		return "", err
	}
	if err := c.Run(); err != nil {
		return "", err
	}
	return c.Get("result").String(), nil
}

func (gw *Gateway) modifyOutMessageTengo(origmsg *config.Message, msg *config.Message, br *bridge.Bridge) (bool, error) {
	filename := gw.BridgeValues().Tengo.OutMessage
	var (
		res  []byte
		err  error
		drop bool
	)

	if filename == "" {
		res, err = internal.Asset("tengo/outmessage.tengo")
		if err != nil {
			return drop, err
		}
	} else {
		res, err = ioutil.ReadFile(filename)
		if err != nil {
			return drop, err
		}
	}

	s := tengo.NewScript(res)

	s.SetImports(stdlib.GetModuleMap(stdlib.AllModuleNames()...))
	_ = s.Add("inAccount", origmsg.Account)
	_ = s.Add("inProtocol", origmsg.Protocol)
	_ = s.Add("inChannel", origmsg.Channel)
	_ = s.Add("inGateway", origmsg.Gateway)
	_ = s.Add("inEvent", origmsg.Event)
	_ = s.Add("outAccount", br.Account)
	_ = s.Add("outProtocol", br.Protocol)
	_ = s.Add("outChannel", msg.Channel)
	_ = s.Add("outGateway", gw.Name)
	_ = s.Add("outEvent", msg.Event)
	_ = s.Add("msgText", msg.Text)
	_ = s.Add("msgUsername", msg.Username)
	_ = s.Add("msgUserID", msg.UserID)
	_ = s.Add("msgDrop", drop)
	c, err := s.Compile()
	if err != nil {
		return drop, err
	}

	if err := c.Run(); err != nil {
		return drop, err
	}

	drop = c.Get("msgDrop").Bool()
	msg.Text = c.Get("msgText").String()
	msg.Username = c.Get("msgUsername").String()

	return drop, nil
}
