// Package client provides explicitly bound service calls.
package client

type Error struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	Action         string `json:"action"`
	Details        any    `json:"details,omitempty"`
	RequestID      string `json:"request_id,omitempty"`
	OutcomeUnknown bool   `json:"outcome_unknown,omitempty"`
	Status         int    `json:"-"`
}

func (e *Error) Error() string { return e.Message }

type CreateJobRequest struct {
	Title    string `json:"title"`
	Brief    string `json:"brief"`
	WorkerID string `json:"worker_id"`
	Status   string `json:"status"`
}

type EditJobRequest struct {
	ExpectedRevision int64   `json:"expected_revision"`
	Title            *string `json:"title,omitempty"`
	Brief            *string `json:"brief,omitempty"`
	WorkerID         *string `json:"worker_id,omitempty"`
	Status           *string `json:"status,omitempty"`
}

type SendMessageRequest struct {
	RecipientWorkerID string `json:"recipient_worker_id"`
	Body              string `json:"body"`
	ReplyToMessageID  string `json:"reply_to_message_id,omitempty"`
}

type JobReceipt struct {
	ID       string `json:"id"`
	Outcome  string `json:"outcome"`
	Revision int64  `json:"revision"`
}

type MessageReceipt struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	Outcome        string `json:"outcome"`
}

type PageOptions struct {
	Limit  int
	Cursor string
}
type JobFilter struct{ WorkerID, Status string }
type InboxFilter struct{ Unread, Unacknowledged bool }
type Page[T any] struct {
	Items   []T    `json:"items"`
	Cursor  string `json:"cursor"`
	HasMore bool   `json:"has_more"`
}
type Worker struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
}
type JobSummary struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	WorkerID string `json:"worker_id"`
	Status   string `json:"status"`
	Revision int64  `json:"revision"`
}
type Job struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Title     string `json:"title"`
	Brief     string `json:"brief"`
	WorkerID  string `json:"worker_id"`
	Status    string `json:"status"`
	Revision  int64  `json:"revision"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}
type JobHistoryEntry struct {
	Revision  int64  `json:"revision"`
	ActorID   string `json:"actor_id"`
	State     Job    `json:"state"`
	CreatedAt string `json:"created_at"`
}
type InboxItem struct {
	ID                string `json:"id"`
	ConversationID    string `json:"conversation_id"`
	SenderWorkerID    string `json:"sender_worker_id"`
	RecipientWorkerID string `json:"recipient_worker_id"`
	Preview           string `json:"preview"`
	PreviewTruncated  bool   `json:"preview_truncated"`
	Seen              bool   `json:"seen"`
	Acknowledged      bool   `json:"acknowledged"`
	CreatedAt         string `json:"created_at"`
}
type Message struct {
	ID                string `json:"id"`
	ConversationID    string `json:"conversation_id"`
	SenderWorkerID    string `json:"sender_worker_id"`
	RecipientWorkerID string `json:"recipient_worker_id"`
	ReplyToMessageID  string `json:"reply_to_message_id,omitempty"`
	Body              string `json:"body"`
	CreatedAt         string `json:"created_at"`
}
type ConversationPage struct {
	ConversationID string    `json:"conversation_id"`
	RootMessageID  string    `json:"root_message_id"`
	Items          []Message `json:"items"`
	Cursor         string    `json:"cursor"`
	HasMore        bool      `json:"has_more"`
}
