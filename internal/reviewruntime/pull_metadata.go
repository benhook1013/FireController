package reviewruntime

import (
	"strings"
	"unicode/utf8"
)

// Source facts retain the full bounded title. Only browser display metadata
// uses its separate 500-byte budget, cutting at a complete UTF-8 character.
func pullDisplayTitle(title string) string {
	title = strings.TrimSpace(title)
	if len(title) <= 500 {
		return title
	}
	end := 500
	for end > 0 && !utf8.RuneStart(title[end]) {
		end--
	}
	return title[:end]
}
