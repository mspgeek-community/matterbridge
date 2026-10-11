package helper

import "regexp"

// A channel reference marks a real channel link from the source platform by
// its exact channel name, e.g. "<#channel:v-hudu>". The gateway resolves it
// for each destination: to that platform's channel link when it has a channel
// with exactly that name, otherwise to plain "#v-hudu".
var channelRefRE = regexp.MustCompile(`<#channel:([^<>\s]+)>`)

// ChannelRef returns the reference for a channel name.
func ChannelRef(name string) string {
	return "<#channel:" + name + ">"
}

// ReplaceChannelRefs resolves every channel reference in text. resolve
// returns the destination's link for an exact channel name, or false.
func ReplaceChannelRefs(text string, resolve func(name string) (string, bool)) string {
	return channelRefRE.ReplaceAllStringFunc(text, func(match string) string {
		name := channelRefRE.FindStringSubmatch(match)[1]
		if resolve != nil {
			if link, ok := resolve(name); ok {
				return link
			}
		}
		return "#" + name
	})
}

// HasChannelRefs reports whether text contains channel references.
func HasChannelRefs(text string) bool {
	return channelRefRE.MatchString(text)
}
