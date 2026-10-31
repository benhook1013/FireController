package reviewprovider

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

var externalNativeTrigger = regexp.MustCompile(`^@coderabbitai full review\n\n<!-- firecontroller-review-trigger v1 operation=(review_operation_[a-f0-9]{32}) attempt=(review_attempt_[a-f0-9]{32}) head=([a-f0-9]{40}) -->$`)

type externalNativeCommand struct {
	at              time.Time
	id              int64
	operation, head string
}
type externalNativeWindows struct {
	commands   []externalNativeCommand
	edits      []IssueComment
	latestEdit []time.Time
}

func externalHostedNativeWindows(comments []IssueComment, provider Author) externalNativeWindows {
	w := externalNativeWindows{}
	for _, comment := range comments {
		if publicationActor(comment.Author, provider) {
			continue
		}
		if !comment.CreatedAt.Equal(comment.UpdatedAt) {
			w.edits = append(w.edits, comment)
			continue
		}
		command := false
		for _, line := range publicationVisibleLines(comment.Body) {
			if publicationCommand.MatchString(line) {
				command = true
				break
			}
		}
		if !command {
			continue
		}
		c := externalNativeCommand{at: comment.CreatedAt}
		if marker := externalNativeTrigger.FindStringSubmatch(comment.Body); marker != nil {
			c.id, c.operation, c.head = comment.ID, marker[1], marker[3]
		}
		w.commands = append(w.commands, c)
	}
	sort.Slice(w.commands, func(i, j int) bool { return w.commands[i].at.Before(w.commands[j].at) })
	sort.Slice(w.edits, func(i, j int) bool { return w.edits[i].CreatedAt.Before(w.edits[j].CreatedAt) })
	var latest time.Time
	for _, edit := range w.edits {
		if edit.UpdatedAt.After(latest) {
			latest = edit.UpdatedAt
		}
		w.latestEdit = append(w.latestEdit, latest)
	}
	return w
}
func (w externalNativeWindows) match(head string, first, last time.Time) (int64, string) {
	i := sort.Search(len(w.commands), func(i int) bool { return !w.commands[i].at.Before(first) }) - 1
	if i < 0 {
		return 0, ""
	}
	c := w.commands[i]
	if c.id == 0 || c.head != head || i+1 < len(w.commands) && !w.commands[i+1].at.After(last) {
		return 0, ""
	}
	// Equal-time commands cannot establish a unique attribution window.
	if i > 0 && w.commands[i-1].at.Equal(c.at) {
		return 0, ""
	}
	edit := sort.Search(len(w.edits), func(i int) bool { return w.edits[i].CreatedAt.After(last) }) - 1
	if edit >= 0 && !w.latestEdit[edit].Before(c.at) {
		return 0, ""
	}
	return c.id, strings.Clone(c.operation)
}
