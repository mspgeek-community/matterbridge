package gateway

import (
	"io/ioutil"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testStoreLogger() *logrus.Entry {
	logger := logrus.New()
	logger.SetOutput(ioutil.Discard)
	return logger.WithField("prefix", "test")
}

func TestMsgStoreSaveLoad(t *testing.T) {
	store, err := openMsgStore(filepath.Join(t.TempDir(), "store.db"), testStoreLogger())
	require.NoError(t, err)

	ids := []storedMsgID{
		{Account: "slack.test", ID: "slack 1700000000.000100", ChannelID: "testingslack.test"},
		{Account: "irc.freenode", ID: "irc abc", ChannelID: "#wimtestingirc.freenode"},
	}
	require.NoError(t, store.save("bridge1", "discord 42", ids))

	got, err := store.load("bridge1", "discord 42")
	require.NoError(t, err)
	assert.ElementsMatch(t, ids, got)

	got, err = store.load("bridge2", "discord 42")
	require.NoError(t, err)
	assert.Empty(t, got, "mappings are per gateway")

	canonical, err := store.canonicalFor("bridge1", "slack 1700000000.000100")
	require.NoError(t, err)
	assert.Equal(t, "discord 42", canonical)

	canonical, err = store.canonicalFor("bridge1", "slack unknown")
	require.NoError(t, err)
	assert.Equal(t, "", canonical)

	n, err := store.prune(time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	got, err = store.load("bridge1", "discord 42")
	require.NoError(t, err)
	assert.Empty(t, got)
}

// A new router on the same store must resolve mappings made by the previous
// one, as after a container restart.
func TestMsgStoreSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	cfg := append([]byte("[general]\nMessageStore=\""+path+"\"\n"), testconfig2...)

	r := maketestRouter(cfg)
	require.NotNil(t, r.store)
	gw := r.Gateways["bridge1"]
	gw.rememberIDs("discord 42", []*BrMsgID{
		{br: gw.Bridges["slack.test"], ID: "slack 1700000000.000100", ChannelID: "testingslack.test"},
	})

	r2 := maketestRouter(cfg)
	gw2 := r2.Gateways["bridge1"]
	assert.False(t, gw2.Messages.Contains("discord 42"), "new router starts with an empty memory cache")

	channel := gw2.Channels["testingslack.test"]
	assert.Equal(t, "1700000000.000100", gw2.getDestMsgID("discord 42", gw2.Bridges["slack.test"], channel))
	assert.Equal(t, "discord 42", gw2.FindCanonicalMsgID("discord", "42"))
	assert.Equal(t, "discord 42", gw2.FindCanonicalMsgID("slack", "1700000000.000100"))
	assert.Equal(t, "", r2.Gateways["bridge2"].FindCanonicalMsgID("discord", "42"))
}

func TestMsgStoreDisabled(t *testing.T) {
	r := maketestRouter(append([]byte("[general]\nMessageStore=\"-\"\n"), testconfig2...))
	assert.Nil(t, r.store)
	gw := r.Gateways["bridge1"]
	gw.rememberIDs("discord 1", nil)
	_, ok := gw.lookupIDs("discord 1")
	assert.True(t, ok)
}

func init() {
	// Tests without an explicit MessageStore must not write to the default path.
	defaultMessageStorePath = "-"
}
