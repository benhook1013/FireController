package client

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// Absence is a normal worker message. A present discriminator is exact.
type MessageOrigin string

func (o *MessageOrigin) UnmarshalJSON(data []byte) error {
	var value string
	if json.Unmarshal(data, &value) != nil || value != "controller" {
		return strictjson.ErrSyntax
	}
	*o = MessageOrigin(value)
	return nil
}
func validMessageOrigin(origin MessageOrigin, sender string) bool {
	return origin == "controller" && sender == "" || origin == "" && validID(sender)
}
func inboxOriginPath(parts []string) bool {
	if len(parts) < 5 || parts[0] != "api" || parts[1] != "v1" || parts[2] != "projects" {
		return false
	}
	switch parts[4] {
	case "inbox", "outbox", "messages", "conversations", "message-events":
		return true
	}
	return false
}
