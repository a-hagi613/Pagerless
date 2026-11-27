package investigations

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/a-hagi613/pagerless/internal/transcript"
)

// heartbeat is the interval of an SSE comment that keeps an idle connection
// alive through a proxy.
const heartbeat = 15 * time.Second

// stream replays the durable events after the cursor and then follows the
// page's log. The subscription is registered before the durable read, so a
// write that lands during the replay is delivered exactly once: the reader
// always resumes from the highest sequence number it has sent.
func (a *API) stream(response http.ResponseWriter, request *http.Request, pageID string, after int64) {
	flusher, ok := response.(http.Flusher)
	if !ok {
		writeError(response, internal("The connection cannot stream."))
		return
	}
	header := response.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)
	flusher.Flush()

	updates, release := a.Service.Store().Subscribe(pageID)
	defer release()

	cursor := after
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		events, err := a.Service.Store().EventsAfter(request.Context(), pageID, cursor)
		if err != nil {
			// The connection is already committed to SSE; end it cleanly and let
			// the client resume from its cursor.
			return
		}
		for _, event := range events {
			if !writeEvent(response, event) {
				return
			}
			cursor = event.Seq
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		select {
		case <-request.Context().Done():
			return
		case <-updates:
		case <-ticker.C:
			if _, err := fmt.Fprint(response, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeEvent renders one SSE frame with the durable sequence number as the id.
func writeEvent(response http.ResponseWriter, event transcript.Event) bool {
	data, err := json.Marshal(event)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(response, "id: %d\ndata: %s\n\n", event.Seq, data); err != nil {
		return false
	}
	return true
}
