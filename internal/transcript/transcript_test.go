package transcript

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), Options{Path: Path(t.TempDir())})
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newPage(t *testing.T, store *Store, id string) (Page, Turn) {
	t.Helper()
	now := time.Now().UTC()
	page := Page{ID: id, Title: "why", Status: StatusRunning, CreatedAt: now, UpdatedAt: now}
	turn := Turn{ID: id + "-t1", PageID: id, Status: StatusRunning, CreatedAt: now, UpdatedAt: now}
	page.ActiveTurnID = turn.ID
	message := Message{ID: id + "-m1", PageID: id, TurnID: turn.ID, Role: "user", Content: "why", CreatedAt: now}
	if err := store.CreatePage(context.Background(), page, message, turn); err != nil {
		t.Fatalf("create the page: %v", err)
	}
	return page, turn
}

func TestEventsAreNumberedFromOnePerPage(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	newPage(t, store, "p1")
	newPage(t, store, "p2")
	for index := range 3 {
		seq, err := store.AppendEvent(context.Background(), Event{
			PageID: "p1", TurnID: "p1-t1", Type: EventToolStarted, Time: time.Now().UTC(),
			Data: json.RawMessage(`{"callId":"c"}`),
		})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if want := int64(index + 2); seq != want {
			t.Fatalf("sequence %d, want %d", seq, want)
		}
	}
	events, err := store.EventsAfter(context.Background(), "p1", 0)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("read %d events, want 4", len(events))
	}
	if events[0].Type != EventTurnStarted || events[0].Seq != 1 {
		t.Fatalf("first event is %+v", events[0])
	}
	after, err := store.EventsAfter(context.Background(), "p1", 2)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(after) != 2 || after[0].Seq != 3 {
		t.Fatalf("resume returned %+v", after)
	}
}

func TestConcurrentAppendsProduceUniqueOrderedSequences(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	newPage(t, store, "p1")
	const writers = 40
	var wait sync.WaitGroup
	wait.Add(writers)
	for index := range writers {
		go func(index int) {
			defer wait.Done()
			_, err := store.AppendEvent(context.Background(), Event{
				PageID: "p1", TurnID: "p1-t1", Type: EventToolFinished, Time: time.Now().UTC(),
				Data: mustJSON(map[string]int{"writer": index}),
			})
			if err != nil {
				t.Errorf("append %d: %v", index, err)
			}
		}(index)
	}
	wait.Wait()
	events, err := store.EventsAfter(context.Background(), "p1", 0)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != writers+1 {
		t.Fatalf("read %d events, want %d", len(events), writers+1)
	}
	for index, event := range events {
		if event.Seq != int64(index+1) {
			t.Fatalf("event %d has sequence %d", index, event.Seq)
		}
	}
}

func TestEvidenceAttemptsAreNeverOverwrittenAcrossTurns(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	_, first := newPage(t, store, "p1")
	now := time.Now().UTC()
	second := Turn{ID: "p1-t2", PageID: "p1", Status: StatusRunning, CreatedAt: now, UpdatedAt: now}
	message := Message{ID: "p1-m2", PageID: "p1", TurnID: second.ID, Role: "user", Content: "again", CreatedAt: now}
	if err := store.FinishTurn(context.Background(), "p1", first.ID, StatusCompleted, nil, nil, "", ""); err != nil {
		t.Fatalf("finish the first turn: %v", err)
	}
	if err := store.AddTurn(context.Background(), "p1", message, second, "", Target{}); err != nil {
		t.Fatalf("add the second turn: %v", err)
	}
	for index, turnID := range []string{first.ID, second.ID} {
		evidence := Evidence{
			PageID: "p1", CallID: "call-1", TurnID: turnID, Tool: "resources_get",
			Arguments: json.RawMessage(`{}`), Result: json.RawMessage(fmt.Sprintf(`{"turn":%d}`, index)),
			Status: EvidenceOK, CreatedAt: time.Now().UTC(),
		}
		if _, err := store.PutEvidence(context.Background(), evidence); err != nil {
			t.Fatalf("store evidence: %v", err)
		}
	}
	latest, err := store.Evidence(context.Background(), "p1", "call-1")
	if err != nil {
		t.Fatalf("read evidence: %v", err)
	}
	if latest.Attempt != 2 || latest.TurnID != second.ID {
		t.Fatalf("latest evidence is %+v", latest)
	}
	firstAttempt, found, err := store.EvidenceInTurn(context.Background(), "p1", first.ID, "call-1")
	if err != nil || !found {
		t.Fatalf("first attempt: %v %v", found, err)
	}
	if firstAttempt.Attempt != 1 {
		t.Fatalf("first attempt number %d", firstAttempt.Attempt)
	}
}

func TestRestartMarksRunningTurnsInterrupted(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	page, turn := newPage(t, store, "p1")
	interrupted, err := store.InterruptRunningTurns(context.Background())
	if err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if len(interrupted) != 1 || interrupted[0].ID != turn.ID {
		t.Fatalf("interrupted %+v", interrupted)
	}
	stored, err := store.Turn(context.Background(), turn.ID)
	if err != nil {
		t.Fatalf("read the turn: %v", err)
	}
	if stored.Status != StatusInterrupted {
		t.Fatalf("turn status %q", stored.Status)
	}
	updated, err := store.Page(context.Background(), page.ID)
	if err != nil {
		t.Fatalf("read the page: %v", err)
	}
	if updated.Status != StatusInterrupted || updated.ActiveTurnID != "" {
		t.Fatalf("page is %+v", updated)
	}
	events, err := store.EventsAfter(context.Background(), page.ID, 0)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	last := events[len(events)-1]
	if last.Type != EventTurnFinished {
		t.Fatalf("last event is %s", last.Type)
	}
	var data map[string]any
	if err := json.Unmarshal(last.Data, &data); err != nil {
		t.Fatalf("decode the event: %v", err)
	}
	if data["status"] != StatusInterrupted {
		t.Fatalf("event data is %s", last.Data)
	}
}

func TestRetentionDeletesTerminalPagesAndKeepsActiveOnes(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	_, first := newPage(t, store, "done")
	if err := store.FinishTurn(context.Background(), "done", first.ID, StatusCompleted, nil, nil, "", ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	newPage(t, store, "active")
	removed, err := store.DeleteExpired(context.Background(), time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed %d pages, want 1", removed)
	}
	if _, err := store.Page(context.Background(), "done"); err == nil {
		t.Fatal("the terminal page survived retention")
	}
	if _, err := store.Page(context.Background(), "active"); err != nil {
		t.Fatalf("the active page was deleted: %v", err)
	}
}
