package helper

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
)

// A user reference marks a real @mention from the source platform. It carries
// every name of the mentioned user (the first is the display name) so the
// destination can link the right user without guessing where a name ends in
// plain text. The names are base64url-encoded JSON, so no character in a name
// can break the marker: "<@user:WyJNZW5keSBHcmVlbiJd>".
var userRefRE = regexp.MustCompile(`<@user:([A-Za-z0-9_-]+)>`)

// UserRef returns the reference for a user. The first non-empty name is shown
// when the destination cannot link the user.
func UserRef(names ...string) string {
	var clean []string
	for _, name := range names {
		if name != "" {
			clean = append(clean, name)
		}
	}
	if len(clean) == 0 {
		return ""
	}
	data, _ := json.Marshal(clean)
	return "<@user:" + base64.RawURLEncoding.EncodeToString(data) + ">"
}

// ReplaceUserRefs resolves every user reference in text. resolve returns the
// destination's mention for the user with these names, or false; unresolved
// references become "@" + display name.
func ReplaceUserRefs(text string, resolve func(names []string) (string, bool)) string {
	return userRefRE.ReplaceAllStringFunc(text, func(match string) string {
		names := decodeUserRef(match)
		if len(names) == 0 {
			return match
		}
		if resolve != nil {
			if mention, ok := resolve(names); ok {
				return mention
			}
		}
		return "@" + names[0]
	})
}

// HasUserRefs reports whether text contains user references.
func HasUserRefs(text string) bool {
	return userRefRE.MatchString(text)
}

func decodeUserRef(match string) []string {
	sub := userRefRE.FindStringSubmatch(match)
	if sub == nil {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(sub[1])
	if err != nil {
		return nil
	}
	var names []string
	if json.Unmarshal(data, &names) != nil {
		return nil
	}
	return names
}

// ResolveUserByNames looks up every name in idx and returns the user ID when
// the names that match all point to the same user.
func ResolveUserByNames(idx NameIndex, names []string) (string, bool) {
	found := ""
	for _, name := range names {
		id, ok := idx.Lookup(name)
		if !ok {
			continue
		}
		if found != "" && found != id {
			return "", false // names point to different users
		}
		found = id
	}
	return found, found != ""
}
