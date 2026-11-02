package messaging

import (
	"regexp"
	"strings"
)

// Plain text of a message: the formatting marks the client renders
// (**bold**, *italic*, ~~strike~~, `code`, ```fences```) removed, whitespace
// collapsed. What the search index holds, because people search for what
// they read on screen, not for the asterisks around it. Mirrors
// web/src/format.js plainText; the two are kept the same by the same
// examples in both test suites.

var (
	fenceRE  = regexp.MustCompile("(?s)```[a-zA-Z0-9+#-]*\\n?(.*?)```")
	codeRE   = regexp.MustCompile("`([^`\\n]+)`")
	boldRE   = regexp.MustCompile(`\*\*(\S(?:.*?\S)?)\*\*`)
	italicRE = regexp.MustCompile(`(^|[^*\w])\*(\S(?:[^*\n]*?\S)?)\*(\W|$)`)
	strikeRE = regexp.MustCompile(`~~(\S(?:[^~\n]*?\S)?)~~`)
	spaceRE  = regexp.MustCompile(`\s+`)
)

func plainText(s string) string {
	s = fenceRE.ReplaceAllString(s, "$1")
	s = codeRE.ReplaceAllString(s, "$1")
	s = boldRE.ReplaceAllString(s, "$1")
	s = italicRE.ReplaceAllString(s, "$1$2$3")
	s = strikeRE.ReplaceAllString(s, "$1")
	return strings.TrimSpace(spaceRE.ReplaceAllString(s, " "))
}
