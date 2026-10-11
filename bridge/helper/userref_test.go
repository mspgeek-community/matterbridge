package helper

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUserRefRoundTrip(t *testing.T) {
	ref := UserRef("Mendy Green [MSPGeek]", "", "mendy.green", "J.R. <@weird|name>\nx")
	assert.True(t, HasUserRefs("hi "+ref))
	assert.Equal(t, "", UserRef("", ""))

	var got []string
	out := ReplaceUserRefs("hi "+ref+"!", func(names []string) (string, bool) {
		got = names
		return "<@42>", true
	})
	assert.Equal(t, "hi <@42>!", out)
	assert.Equal(t, []string{"Mendy Green [MSPGeek]", "mendy.green", "J.R. <@weird|name>\nx"}, got)

	// Unresolved and non-linking destinations show the display name.
	assert.Equal(t, "hi @Mendy Green [MSPGeek]!", ReplaceUserRefs("hi "+ref+"!", nil))
	assert.Equal(t, "hi @Mendy Green [MSPGeek]!", ReplaceUserRefs("hi "+ref+"!",
		func([]string) (string, bool) { return "", false }))

	// Plain text and other markup are untouched.
	assert.Equal(t, "@John <@123> <#channel:x>", ReplaceUserRefs("@John <@123> <#channel:x>", nil))
}

func TestResolveUserByNames(t *testing.T) {
	idx := NameIndex{}
	idx.Add("D1", "mendygreen", "Mendy Green")
	idx.Add("D2", "jsmith")

	id, ok := ResolveUserByNames(idx, []string{"Mendy Green [MSPGeek]", "mendy.green", "nomatch"})
	assert.True(t, ok)
	assert.Equal(t, "D1", id)

	_, ok = ResolveUserByNames(idx, []string{"Mendy Green", "jsmith"})
	assert.False(t, ok, "names pointing to different users are not linked")

	_, ok = ResolveUserByNames(idx, []string{"nobody"})
	assert.False(t, ok)
}

func TestReplaceMentionsSkipsMentionMarkup(t *testing.T) {
	resolve := func(name string) (string, bool) { return "<@X>", true }
	assert.Equal(t, "<@123> <@!456>", ReplaceMentions("<@123> <@!456>", resolve))
}
