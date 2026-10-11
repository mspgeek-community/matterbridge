package helper

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeName(t *testing.T) {
	for _, in := range []string{"John Smith", "john_smith", "JohnSmith", "john.smith", " JOHN-SMITH "} {
		assert.Equal(t, "johnsmith", NormalizeName(in), in)
	}
	assert.Equal(t, "zoë", NormalizeName("Zoë"))
	assert.Equal(t, "", NormalizeName("!!!"))
}

func TestNameIndex(t *testing.T) {
	idx := NameIndex{}
	idx.Add("U1", "John Smith", "jsmith", "")
	idx.Add("U2", "Jane Doe", "johnsmith") // collides with U1
	idx.Add("U1", "JohnSmith")             // same user again is fine

	_, ok := idx.Lookup("john smith")
	assert.False(t, ok, "name shared by two users is ambiguous")
	id, ok := idx.Lookup("JSMITH")
	assert.True(t, ok)
	assert.Equal(t, "U1", id)
	id, ok = idx.Lookup("jane_doe")
	assert.True(t, ok)
	assert.Equal(t, "U2", id)
	_, ok = idx.Lookup("nobody")
	assert.False(t, ok)
}

func TestReplaceMentions(t *testing.T) {
	idx := NameIndex{}
	idx.Add("U1", "John Smith")
	idx.Add("U2", "John")
	idx.Add("U3", "here")
	idx.Add("U4", "Mendy Green [MSPGeek]")
	resolve := func(name string) (string, bool) {
		id, ok := idx.Lookup(name)
		return "<@" + id + ">", ok
	}

	cases := map[string]string{
		"hi @John Smith how are you":           "hi <@U1> how are you", // longest match wins
		"@John Smith, can you check?":          "<@U1>, can you check?",
		"@John: ping":                          "<@U2>: ping",
		"(@JohnSmith)":                         "(<@U1>)",
		"mail bob@example.com":                 "mail bob@example.com", // not a mention
		"https://www.youtube.com/@John":        "https://www.youtube.com/@John",
		"see (https://medium.com/@John Smith)": "see (https://medium.com/@John Smith)",
		"www.youtube.com/@John and @John":      "www.youtube.com/@John and <@U2>",
		"@nobody here":                         "@nobody here",
		"@here look":                           "@here look", // reserved
		"@@John":                               "@@John",
		"two @John and @John Smith":            "two <@U2> and <@U1>",
		"@Mendy Green [MSPGeek] yo":            "<@U4> yo",   // Discord -> Slack
		"@MendyGreen[MSPGeek] what":            "<@U4> what", // Slack -> Discord
		"hi @Mendy Green [MSPGeek], ok?":       "hi <@U4>, ok?",
		"[@MendyGreen[MSPGeek]]":               "[<@U4>]",
		"(@John) and [@John]":                  "(<@U2>) and [<@U2>]",
		"":                                     "",
	}
	for in, want := range cases {
		assert.Equal(t, want, ReplaceMentions(in, resolve), in)
	}
}
