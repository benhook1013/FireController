package coordimportmodel

// PreparedMessage retains an explicitly mapped historical message. Handling
// timestamps remain provenance; native recipient state records only flags.
type PreparedMessage struct {
	SourceKey         string `json:"source_key"`
	ReplyToSourceKey  string `json:"reply_to_source_key,omitempty"`
	SenderWorkerID    string `json:"sender_worker_id"`
	RecipientWorkerID string `json:"recipient_worker_id"`
	Body              string `json:"body"`
	CreatedAt         string `json:"created_at"`
	SeenAt            string `json:"seen_at,omitempty"`
	AcknowledgedAt    string `json:"acknowledged_at,omitempty"`
}
