package cli

import (
	"encoding/json"
	"time"
)

// Event types and turn statuses named by the public contract.
const (
	eventTurnStarted  = "turn.started"
	eventToolStarted  = "tool.started"
	eventToolFinished = "tool.finished"
	eventAnswer       = "answer"
	eventProposal     = "proposal"
	eventTurnFinished = "turn.finished"

	statusCompleted   = "completed"
	statusStopped     = "stopped"
	statusFailed      = "failed"
	statusInterrupted = "interrupted"
	statusUnknown     = "unknown"
)

// target is the Kubernetes object hint that narrows an investigation. The
// service accepts it on create and reply and rejects a namespace that
// contradicts the top-level hint.
type target struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// createRequest opens a page or adds a turn. Only the message is required; a
// reply that omits the hints inherits the conversation's.
type createRequest struct {
	Message   string  `json:"message"`
	Namespace string  `json:"namespace,omitempty"`
	Target    *target `json:"target,omitempty"`
}

// createResponse is the 201 answer to opening a page or adding a turn.
type createResponse struct {
	PageID string `json:"pageId"`
	TurnID string `json:"turnId"`
}

// pageSummary is one row of the page list and the stable part of a page.
type pageSummary struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Status       string    `json:"status"`
	UpdatedAt    time.Time `json:"updatedAt"`
	ActiveTurnID *string   `json:"activeTurnId"`
}

// pageDetail is one conversation with its messages and resume cursor.
type pageDetail struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Status       string    `json:"status"`
	UpdatedAt    time.Time `json:"updatedAt"`
	ActiveTurnID *string   `json:"activeTurnId"`
	Messages     []message `json:"messages"`
	LastEventID  int64     `json:"lastEventId"`
}

// message is one user or assistant turn in the transcript.
type message struct {
	ID        string     `json:"id"`
	TurnID    string     `json:"turnId"`
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	CreatedAt time.Time  `json:"createdAt"`
	Citations []citation `json:"citations"`
}

// citation points an answer at one recorded tool attempt.
type citation struct {
	CallID  string `json:"callId"`
	Pointer string `json:"pointer,omitempty"`
}

// event is one entry of the ordered, append-only replay log.
type event struct {
	Seq    int64           `json:"seq"`
	PageID string          `json:"pageId"`
	TurnID string          `json:"turnId"`
	Type   string          `json:"type"`
	Time   time.Time       `json:"time"`
	Data   json.RawMessage `json:"data"`
}

// eventData collects the fields the CLI reads from an event's data object. The
// object differs by event type; every field is optional and unknown fields are
// ignored so a newer server can add detail without breaking an older plugin.
type eventData struct {
	CallID     string          `json:"callId"`
	Tool       string          `json:"tool"`
	Status     string          `json:"status"`
	DurationMS *int64          `json:"durationMs"`
	Arguments  json.RawMessage `json:"arguments"`
	Result     json.RawMessage `json:"result"`
	Redacted   bool            `json:"redacted"`
	Truncated  bool            `json:"truncated"`
	Content    string          `json:"content"`
	Citations  []citation      `json:"citations"`
	PRURL      string          `json:"prUrl"`
	Files      []diffFile      `json:"files"`
}

// evidence is one sanitized tool attempt, addressed by its call ID.
type evidence struct {
	CallID     string          `json:"callId"`
	Tool       string          `json:"tool"`
	Arguments  json.RawMessage `json:"arguments"`
	Result     json.RawMessage `json:"result"`
	Status     string          `json:"status"`
	DurationMS int64           `json:"durationMs"`
	Redacted   bool            `json:"redacted"`
	Truncated  bool            `json:"truncated"`
}

// diffFile is one proposed source change.
type diffFile struct {
	Path string `json:"path"`
	Diff string `json:"diff"`
}

// diffResult is the proposal for a page. No files means no change was proposed.
type diffResult struct {
	Files []diffFile `json:"files"`
	PRURL string     `json:"prUrl,omitempty"`
}

// stopResponse is the idempotent answer to a stop request.
type stopResponse struct {
	Status string `json:"status"`
}

// eventList accepts either the documented object with an events array or a
// bare array, so a small server change cannot strand the plugin.
type eventList struct {
	Events []event
}

// UnmarshalJSON decodes both accepted shapes.
func (l *eventList) UnmarshalJSON(raw []byte) error {
	if len(raw) > 0 && raw[0] == '[' {
		return json.Unmarshal(raw, &l.Events)
	}
	var object struct {
		Events []event `json:"events"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	l.Events = object.Events
	return nil
}
