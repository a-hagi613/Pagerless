package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

const (
	// streamAttempts bounds automatic reconnection. Each retry resumes after the
	// last durable sequence, so no event is lost or repeated.
	streamAttempts = 4
	// streamBackoff is the first reconnect delay; it doubles per attempt.
	streamBackoff = 200 * time.Millisecond
)

var (
	// errStreamEnded means the server closed the stream before the turn finished,
	// which is safe to resume from the last sequence.
	errStreamEnded = errors.New("the event stream ended before the turn finished")
	// errStreamProtocol means the server did not speak the documented event
	// stream, which a reconnect will not fix.
	errStreamProtocol = errors.New("the server did not send a valid event stream")
)

// streamOutcome is what one followed stream reached.
type streamOutcome struct {
	// LastSeq is the highest durable sequence delivered.
	LastSeq int64
	// Status is the turn's terminal status, empty when it did not finish.
	Status string
	// Detached means the caller cancelled the command, so the turn keeps
	// running on the server.
	Detached bool
}

// follow streams events after the given sequence, reconnecting from the last
// delivered sequence when a connection drops. Cancelling ctx detaches without
// stopping the turn; a deadline is returned as an error so a script can tell a
// timeout from an intentional detach.
func (c *client) follow(ctx context.Context, pageID string, after int64, handle func(event)) (streamOutcome, error) {
	outcome := streamOutcome{LastSeq: after}
	var lastErr error
	for attempt := range streamAttempts {
		if attempt > 0 {
			if err := sleepContext(ctx, streamBackoff<<(attempt-1)); err != nil {
				return finishCancel(ctx, outcome)
			}
		}
		body, err := c.openEvents(ctx, pageID, outcome.LastSeq)
		if err != nil {
			if ctx.Err() != nil {
				return finishCancel(ctx, outcome)
			}
			if !retryableError(err) {
				return outcome, err
			}
			lastErr = err
			continue
		}
		status, err := readEvents(ctx, body, &outcome, handle)
		_ = body.Close()
		if err == nil {
			outcome.Status = status
			return outcome, nil
		}
		if ctx.Err() != nil {
			return finishCancel(ctx, outcome)
		}
		if !retryableError(err) {
			return outcome, err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errStreamEnded
	}
	return outcome, fmt.Errorf("the event stream stopped after %d attempts: %w", streamAttempts, lastErr)
}

// finishCancel reports an intentional detach as a successful outcome and a
// deadline as a timeout.
func finishCancel(ctx context.Context, outcome streamOutcome) (streamOutcome, error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		outcome.Detached = true
		return outcome, nil
	}
	return outcome, ctx.Err()
}

// readEvents consumes one SSE connection until the turn finishes or the
// connection ends. It drops any sequence the caller already has.
func readEvents(ctx context.Context, body io.Reader, outcome *streamOutcome, handle func(event)) (string, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxEventBytes)
	var data []byte
	var eventID int64
	dispatch := func() (string, bool, error) {
		if len(data) == 0 {
			data = nil
			eventID = 0
			return "", false, nil
		}
		var current event
		if err := json.Unmarshal(data, &current); err != nil {
			return "", false, fmt.Errorf("decode a streamed event: %w", err)
		}
		if current.Seq == 0 && eventID != 0 {
			current.Seq = eventID
		}
		data = nil
		eventID = 0
		if current.Seq <= outcome.LastSeq {
			return "", false, nil
		}
		outcome.LastSeq = current.Seq
		if handle != nil {
			handle(current)
		}
		if status := terminalStatus(current); status != "" {
			return status, true, nil
		}
		return "", false, nil
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		line := scanner.Text()
		if line == "" {
			status, done, err := dispatch()
			if err != nil {
				return "", err
			}
			if done {
				return status, nil
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "data:"):
			value := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if len(data) > 0 {
				data = append(data, '\n')
			}
			if len(data)+len(value) > maxEventBytes {
				return "", fmt.Errorf("%w: an event exceeds the %d byte limit", errStreamProtocol, maxEventBytes)
			}
			data = append(data, value...)
		case strings.HasPrefix(line, "id:"):
			if seq, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "id:")), 10, 64); err == nil {
				eventID = seq
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read the event stream: %w", err)
	}
	return "", errStreamEnded
}

// terminalStatus returns a turn's final status when the event finishes it.
func terminalStatus(current event) string {
	if current.Type != eventTurnFinished {
		return ""
	}
	var data struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(current.Data, &data); err == nil && data.Status != "" {
		return data.Status
	}
	return statusCompleted
}

// retryableError reports whether a reconnect could succeed.
func retryableError(err error) bool {
	if errors.Is(err, errStreamEnded) {
		return true
	}
	if errors.Is(err, errStreamProtocol) {
		return false
	}
	if api, ok := errors.AsType[*apiError](err); ok {
		return api.retryable()
	}
	if _, ok := errors.AsType[*json.SyntaxError](err); ok {
		return false
	}
	if _, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		return false
	}
	// Every other failure is a transport problem, which a fresh connection may
	// survive.
	return true
}

// sleepContext waits for the backoff or the caller's cancellation.
func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
