package helper

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReplaceChannelRefs(t *testing.T) {
	channels := map[string]string{"v-hudu": "111", "general": "222"}
	resolve := func(name string) (string, bool) {
		id, ok := channels[name]
		return "<#" + id + ">", ok
	}

	text := "see " + ChannelRef("v-hudu") + " and " + ChannelRef("random") + "!"
	assert.True(t, HasChannelRefs(text))
	assert.Equal(t, "see <#111> and #random!", ReplaceChannelRefs(text, resolve))
	assert.Equal(t, "see #v-hudu and #random!", ReplaceChannelRefs(text, nil))

	// Exact names only, and typed "#text" is never touched.
	assert.Equal(t, "#V-Hudu", ReplaceChannelRefs(ChannelRef("V-Hudu"), resolve))
	assert.Equal(t, "#general is a #1 priority", ReplaceChannelRefs("#general is a #1 priority", resolve))
	assert.False(t, HasChannelRefs("#general <#TS:1700000000.0001> <#C123|general>"))
}
