package inbox

import (
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
)

// Message direction changes only selection and cursor identity. Both views show
// the actual recipient's receipt state, which no read may silently default.
type messageDirection bool

const (
	incomingMessages messageDirection = false
	outgoingMessages messageDirection = true
)

func (h *handler) listOutbox(w http.ResponseWriter, r *http.Request) {
	h.listMessages(w, r, outgoingMessages)
}

func storedMessagesUnavailable(direction messageDirection) error {
	message := "Stored incoming messages are unavailable."
	if direction == outgoingMessages {
		message = "Stored outgoing messages are unavailable."
	}
	return core.Fail("storage_unavailable", message, http.StatusInternalServerError)
}
