// Package inbound contains the normalized immutable coordinates of an accepted
// message. They are transport-independent and durable with the message queue.
package inbound

type Sender struct {
	ID          string `json:"id,omitempty"`
	Username    string `json:"username,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

type Chat struct {
	ID       string `json:"id"`
	Type     string `json:"type,omitempty"`
	Title    string `json:"title,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
}

type Message struct {
	ID     string `json:"id,omitempty"`
	SentAt string `json:"sent_at,omitempty"`
}

type Origin struct {
	Transport string  `json:"transport"`
	Chat      Chat    `json:"chat"`
	Message   Message `json:"message"`
	Sender    Sender  `json:"sender"`
}
