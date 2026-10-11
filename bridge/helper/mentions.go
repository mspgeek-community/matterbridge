package helper

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxMentionLength is the longest name tried after "@" (Discord's limit).
const maxMentionLength = 32

// reservedMentions are never resolved to users.
var reservedMentions = map[string]bool{"everyone": true, "here": true, "channel": true}

// NormalizeName folds a user or display name for cross-platform matching:
// lowercase, letters and digits only. "John Smith", "john_smith" and
// "JohnSmith" all become "johnsmith".
func NormalizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// NameIndex maps normalized names to user IDs. A name shared by different
// users is ambiguous and resolves to nothing.
type NameIndex map[string]string

const ambiguousName = "\x00ambiguous"

// Add registers names for userID.
func (idx NameIndex) Add(userID string, names ...string) {
	for _, name := range names {
		key := NormalizeName(name)
		if key == "" {
			continue
		}
		if existing, ok := idx[key]; ok && existing != userID {
			idx[key] = ambiguousName
			continue
		}
		idx[key] = userID
	}
}

// Lookup returns the user ID registered for name.
func (idx NameIndex) Lookup(name string) (string, bool) {
	id, ok := idx[NormalizeName(name)]
	if !ok || id == ambiguousName {
		return "", false
	}
	return id, true
}

// ReplaceMentions replaces "@name" mentions in text. A mention starts with "@"
// at the start of the text or after a character that cannot be part of a word,
// so "bob@example.com" is not a mention. For each mention the longest
// candidate name is tried first; resolve returns the replacement for a name,
// or false when the name is not a known user. Unresolved mentions are left
// unchanged.
func ReplaceMentions(text string, resolve func(name string) (string, bool)) string {
	var out strings.Builder
	i := 0
	for i < len(text) {
		at := strings.IndexByte(text[i:], '@')
		if at < 0 {
			break
		}
		at += i
		out.WriteString(text[i:at])
		i = at + 1
		if at > 0 {
			// "<@" is mention markup (<@123>, <@user:...>), not a typed mention.
			if prev, _ := utf8.DecodeLastRuneInString(text[:at]); prev == '@' || prev == '_' || prev == '<' ||
				unicode.IsLetter(prev) || unicode.IsNumber(prev) || inURL(text[:at]) {
				out.WriteByte('@')
				continue
			}
		}
		rest := text[i:]
		if end := strings.IndexAny(rest, "@\n"); end >= 0 {
			rest = rest[:end]
		}
		rest = truncateRunes(rest, maxMentionLength)
		replaced := false
		candidates := mentionCandidates(rest)
		for c := len(candidates) - 1; c >= 0; c-- {
			name := candidates[c]
			if reservedMentions[strings.ToLower(name)] {
				break
			}
			if replacement, ok := resolve(name); ok {
				out.WriteString(replacement)
				i += len(name)
				replaced = true
				break
			}
		}
		if !replaced {
			out.WriteByte('@')
		}
	}
	out.WriteString(text[i:])
	return out.String()
}

// inURL reports whether the text right before an "@" is part of a URL, as in
// "https://www.youtube.com/@channel".
func inURL(before string) bool {
	start := strings.LastIndexFunc(before, unicode.IsSpace) + 1
	token := strings.TrimLeft(before[start:], "<([")
	return strings.Contains(token, "://") || strings.HasPrefix(strings.ToLower(token), "www.")
}

func truncateRunes(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// keepClosingBrackets re-attaches closing brackets that trimming removed from
// name when name opened them, so "Mendy Green [MSPGeek]" keeps its "]" while
// "(@John)" still ends at "John".
func keepClosingBrackets(name, after string) string {
	pairs := map[rune]rune{')': '(', ']': '[', '}': '{'}
	for _, r := range after {
		open, ok := pairs[r]
		if !ok || strings.Count(name, string(open)) <= strings.Count(name, string(r)) {
			break
		}
		name += string(r)
	}
	return name
}

// mentionCandidates returns the prefixes of s that end before whitespace or
// trailing punctuation, shortest first: "John Smith, hi" gives "John",
// "John Smith", "John Smith, hi".
func mentionCandidates(s string) []string {
	var candidates []string
	add := func(c string) {
		c = strings.TrimRightFunc(c, func(r rune) bool {
			return unicode.IsSpace(r) || (unicode.IsPunct(r) && r != '_' && r != '-' && r != '.')
		})
		c = strings.TrimRight(c, ".")
		c = keepClosingBrackets(c, s[len(c):])
		if c != "" && (len(candidates) == 0 || candidates[len(candidates)-1] != c) {
			candidates = append(candidates, c)
		}
	}
	for i, r := range s {
		if unicode.IsSpace(r) {
			add(s[:i])
		}
	}
	add(s)
	return candidates
}
