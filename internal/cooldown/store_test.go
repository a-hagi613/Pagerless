package cooldown

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"

	"github.com/a-hagi613/pagerless/internal/history"
)

// testStart is the fixed instant every store test starts from.
var testStart = time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)

// Constants the store tests share, so a value that identifies a row is written
// once.
const (
	testSubjectKey  = "subject-key"
	testSeverity    = "warning"
	testClusterID   = "cluster-test"
	testReasonClass = "pod.crashloop"
	testScope       = "app"
)

// newTestStore opens a real SQLite history file, because the Cooldown state is
// written through the same handle and the same pragmas the server uses.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	historyStore, err := history.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("open the history database: %v", err)
	}
	t.Cleanup(func() { _ = historyStore.Close() })
	store, err := New(historyStore.DB(), WithClock(clocktesting.NewFakeClock(testStart)))
	if err != nil {
		t.Fatalf("open the cooldown store: %v", err)
	}
	return store
}

// testRecord returns a Record for the test Fingerprint.
func testRecord() Record {
	return Record{
		Fingerprint: testFingerprint,
		ClusterID:   testClusterID,
		SubjectKey:  testSubjectKey,
		APIVersion:  "apps/v1",
		Kind:        "Deployment",
		Namespace:   "shop",
		Name:        "checkout",
		UID:         "d-1",
		ReasonClass: testReasonClass,
		Scope:       testScope,
		FirstSeenAt: testStart,
	}
}

func TestBeginPageAdvancesTheLadder(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := context.Background()
	record := testRecord()
	ladder := store.Ladder()

	at := testStart
	for level := 0; level <= ladder.MaxLevel()+1; level++ {
		window, err := store.BeginPage(ctx, record, at)
		if err != nil {
			t.Fatalf("begin page at level %d: %v", level, err)
		}
		wantLevel := level
		if wantLevel > ladder.MaxLevel() {
			wantLevel = ladder.MaxLevel()
		}
		if window.Level != wantLevel {
			t.Fatalf("level = %d, want %d", window.Level, wantLevel)
		}
		if want := at.Add(Jitter(ladder.Step(wantLevel), testFingerprint)); !window.CooldownUntil.Equal(want) {
			t.Fatalf("cooldown until = %s, want %s", window.CooldownUntil, want)
		}
		if window.RecurrenceCount != int32(wantLevel+1) {
			t.Fatalf("recurrence count = %d, want %d", window.RecurrenceCount, wantLevel+1)
		}
		stored, found, err := store.Get(ctx, testFingerprint)
		if err != nil || !found {
			t.Fatalf("get the cooldown: found=%v err=%v", found, err)
		}
		if stored.NextBackoffLevel != ladder.Next(wantLevel+1) {
			t.Fatalf("next level = %d, want %d", stored.NextBackoffLevel, ladder.Next(wantLevel+1))
		}
		at = window.CooldownUntil.Add(time.Second)
	}
}

// TestBeginPageInsideACooldownCountsTheSuppression is the budget rule: the same
// failure inside its Cooldown opens no Page, but is counted and escalates the
// Page that eventually opens.
func TestBeginPageInsideACooldownCountsTheSuppression(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := context.Background()
	record := testRecord()

	window, err := store.BeginPage(ctx, record, testStart)
	if err != nil {
		t.Fatalf("begin the first page: %v", err)
	}
	inside := testStart.Add(time.Minute)
	for want := int64(1); want <= 3; want++ {
		if _, err := store.BeginPage(ctx, record, inside); !errors.Is(err, ErrNotOpen) {
			t.Fatalf("begin inside the cooldown: err = %v, want ErrNotOpen", err)
		}
		stored, found, err := store.Get(ctx, testFingerprint)
		if err != nil || !found {
			t.Fatalf("get the suppressed cooldown: found=%v err=%v", found, err)
		}
		if stored.SuppressedCount != want {
			t.Fatalf("suppressed count = %d, want %d", stored.SuppressedCount, want)
		}
	}
	after := window.CooldownUntil.Add(time.Second)
	next, err := store.BeginPage(ctx, record, after)
	if err != nil {
		t.Fatalf("begin after the cooldown: %v", err)
	}
	if next.SuppressedCount != 3 {
		t.Fatalf("the new Page carries %d suppressions, want 3", next.SuppressedCount)
	}
	if next.Level != 1 {
		t.Fatalf("level = %d, want the escalated level 1", next.Level)
	}
}

func TestSuppressRequiresAnExistingRow(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	if _, err := store.Suppress(context.Background(), testFingerprint, testStart); err == nil {
		t.Fatal("suppressing an unknown fingerprint must fail rather than create a row")
	}
	if _, found, err := store.Get(context.Background(), testFingerprint); err != nil || found {
		t.Fatalf("a suppression must not create a row: found=%v err=%v", found, err)
	}
}

// TestSeedNeverReplacesARow is the restart rule: a Page restored from the API
// server may add state that SQLite lost, but it can never reset a Cooldown.
func TestSeedNeverReplacesARow(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := context.Background()
	record := testRecord()
	if _, err := store.BeginPage(ctx, record, testStart); err != nil {
		t.Fatalf("begin the page: %v", err)
	}
	first, found, err := store.Get(ctx, testFingerprint)
	if err != nil || !found {
		t.Fatalf("get the cooldown: found=%v err=%v", found, err)
	}
	restored := record
	restored.NextBackoffLevel = 0
	restored.LastPagedAt = testStart.Add(-time.Hour)
	restored.CooldownUntil = testStart.Add(-time.Hour)
	if err := store.Seed(ctx, restored); err != nil {
		t.Fatalf("seed: %v", err)
	}
	second, _, err := store.Get(ctx, testFingerprint)
	if err != nil {
		t.Fatalf("get the cooldown after seeding: %v", err)
	}
	if second.NextBackoffLevel != first.NextBackoffLevel || !second.CooldownUntil.Equal(first.CooldownUntil) {
		t.Fatalf("seeding replaced the row: %+v, want %+v", second, first)
	}
}

func TestSeedInsertsAMissingRow(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := context.Background()
	record := testRecord()
	record.NextBackoffLevel = 2
	record.LastPagedAt = testStart
	record.CooldownUntil = testStart.Add(2 * time.Hour)
	if err := store.Seed(ctx, record); err != nil {
		t.Fatalf("seed: %v", err)
	}
	stored, found, err := store.Get(ctx, testFingerprint)
	if err != nil || !found {
		t.Fatalf("get the seeded cooldown: found=%v err=%v", found, err)
	}
	if stored.NextBackoffLevel != 2 || !stored.CooldownUntil.Equal(record.CooldownUntil) {
		t.Fatalf("seeded row = %+v, want the reconstructed one", stored)
	}
}

// TestResetNeedsAHealthyWindowWithoutObservations proves the level returns to
// the first step only after thirty healthy minutes with nothing still observed.
func TestResetNeedsAHealthyWindowWithoutObservations(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := context.Background()
	record := testRecord()
	if _, err := store.BeginPage(ctx, record, testStart); err != nil {
		t.Fatalf("begin the page: %v", err)
	}
	resolvedAt := testStart.Add(time.Hour)
	if err := store.MarkResolved(ctx, record.SubjectKey, resolvedAt); err != nil {
		t.Fatalf("mark resolved: %v", err)
	}
	if _, err := store.PutObservation(ctx, Observation{
		Fingerprint: testFingerprint,
		SubjectKey:  record.SubjectKey,
		State:       StatePaged,
		Severity:    testSeverity,
		FirstSeenAt: testStart,
		LastSeenAt:  resolvedAt,
	}, resolvedAt); err != nil {
		t.Fatalf("store an active observation: %v", err)
	}
	// The window elapsed, but the subject still has an active observation.
	reset, err := store.Reset(ctx, resolvedAt.Add(DefaultResetAfterHealthy+time.Minute))
	if err != nil {
		t.Fatalf("reset with an active observation: %v", err)
	}
	if reset != 0 {
		t.Fatalf("reset %d rows while an observation is active, want 0", reset)
	}
	if err := store.ResolveObservation(ctx, testFingerprint, resolvedAt.Add(time.Hour)); err != nil {
		t.Fatalf("resolve the observation: %v", err)
	}
	reset, err = store.Reset(ctx, resolvedAt.Add(DefaultResetAfterHealthy-time.Minute))
	if err != nil {
		t.Fatalf("reset inside the healthy window: %v", err)
	}
	if reset != 0 {
		t.Fatalf("reset %d rows inside the healthy window, want 0", reset)
	}
	reset, err = store.Reset(ctx, resolvedAt.Add(DefaultResetAfterHealthy+time.Minute))
	if err != nil {
		t.Fatalf("reset after the healthy window: %v", err)
	}
	if reset != 1 {
		t.Fatalf("reset %d rows after the healthy window, want 1", reset)
	}
	stored, _, err := store.Get(ctx, testFingerprint)
	if err != nil {
		t.Fatalf("get the reset cooldown: %v", err)
	}
	if stored.NextBackoffLevel != 0 || stored.SuppressedCount != 0 {
		t.Fatalf("reset row = %+v, want the first level", stored)
	}
}

func TestPutObservationCoalescesHeartbeatsAndWritesTransitions(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := context.Background()
	observation := Observation{
		Fingerprint:   testFingerprint,
		SubjectKey:    testSubjectKey,
		State:         StateObserving,
		Severity:      testSeverity,
		FirstSeenAt:   testStart,
		LastSeenAt:    testStart,
		Occurrences:   1,
		LastTriggerAt: testStart,
	}
	wrote, err := store.PutObservation(ctx, observation, testStart)
	if err != nil || !wrote {
		t.Fatalf("the first observation wrote=%v err=%v, want a write", wrote, err)
	}
	heartbeat := observation
	heartbeat.LastSeenAt = testStart.Add(5 * time.Second)
	wrote, err = store.PutObservation(ctx, heartbeat, testStart.Add(5*time.Second))
	if err != nil || wrote {
		t.Fatalf("a heartbeat inside ten seconds wrote=%v err=%v, want no write", wrote, err)
	}
	transition := heartbeat
	transition.State = StatePaged
	wrote, err = store.PutObservation(ctx, transition, testStart.Add(6*time.Second))
	if err != nil || !wrote {
		t.Fatalf("a state transition wrote=%v err=%v, want an immediate write", wrote, err)
	}
	later := transition
	later.LastSeenAt = testStart.Add(16 * time.Second)
	wrote, err = store.PutObservation(ctx, later, testStart.Add(16*time.Second))
	if err != nil || !wrote {
		t.Fatalf("a heartbeat after ten seconds wrote=%v err=%v, want a write", wrote, err)
	}
	stored, found, err := store.Observation(ctx, testFingerprint)
	if err != nil || !found {
		t.Fatalf("get the observation: found=%v err=%v", found, err)
	}
	if stored.State != StatePaged || !stored.LastTriggerAt.Equal(testStart) {
		t.Fatalf("stored observation = %+v, want the last transition", stored)
	}
}

func TestStaleObservationsClearFromLiveStatus(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := store.PutObservation(ctx, Observation{
		Fingerprint: testFingerprint,
		SubjectKey:  testSubjectKey,
		State:       StatePaged,
		Severity:    testSeverity,
		FirstSeenAt: testStart,
		LastSeenAt:  testStart,
	}, testStart); err != nil {
		t.Fatalf("store an observation: %v", err)
	}
	stale, err := store.StaleObservations(ctx, 5*time.Minute, testStart.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("read stale observations: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("stale = %d inside the clear window, want 0", len(stale))
	}
	stale, err = store.StaleObservations(ctx, 5*time.Minute, testStart.Add(6*time.Minute))
	if err != nil {
		t.Fatalf("read stale observations: %v", err)
	}
	if len(stale) != 1 || stale[0].Fingerprint != testFingerprint {
		t.Fatalf("stale = %+v, want the observation", stale)
	}
	if active, err := store.ActiveObservations(ctx, testSubjectKey); err != nil || !active {
		t.Fatalf("active=%v err=%v, want an active observation", active, err)
	}
	if err := store.ResolveObservation(ctx, testFingerprint, testStart.Add(6*time.Minute)); err != nil {
		t.Fatalf("resolve the observation: %v", err)
	}
	if active, err := store.ActiveObservations(ctx, testSubjectKey); err != nil || active {
		t.Fatalf("active=%v err=%v after resolving, want none", active, err)
	}
	stale, err = store.StaleObservations(ctx, 5*time.Minute, testStart.Add(20*time.Minute))
	if err != nil {
		t.Fatalf("read stale observations: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("stale = %d after resolving, want 0", len(stale))
	}
}

func TestPagedSinceAndLastPage(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := context.Background()
	record := testRecord()
	window, err := store.BeginPage(ctx, record, testStart)
	if err != nil {
		t.Fatalf("begin the page: %v", err)
	}
	count, err := store.PagedSince(ctx, testStart.Add(-time.Hour))
	if err != nil || count != 1 {
		t.Fatalf("paged since = %d err=%v, want 1", count, err)
	}
	count, err = store.PagedSince(ctx, testStart.Add(time.Minute))
	if err != nil || count != 0 {
		t.Fatalf("paged since the page = %d err=%v, want 0", count, err)
	}
	last, found, err := store.LastPage(ctx, record.ClusterID, record.SubjectKey)
	if err != nil || !found {
		t.Fatalf("last page: found=%v err=%v", found, err)
	}
	if last.Fingerprint != testFingerprint || !last.CooldownUntil.Equal(window.CooldownUntil) {
		t.Fatalf("last page = %+v, want the opened one", last)
	}
}

// TestCooldownSurvivesAReopen is the "restarting the server changes nothing"
// rule: a new process reads the same ladder level and window from the file.
func TestCooldownSurvivesAReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "history.db")
	ctx := context.Background()
	first, err := history.Open(path)
	if err != nil {
		t.Fatalf("open the history database: %v", err)
	}
	store, err := New(first.DB(), WithClock(clocktesting.NewFakeClock(testStart)))
	if err != nil {
		t.Fatalf("open the cooldown store: %v", err)
	}
	window, err := store.BeginPage(ctx, testRecord(), testStart)
	if err != nil {
		t.Fatalf("begin the page: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close the history database: %v", err)
	}

	second, err := history.Open(path)
	if err != nil {
		t.Fatalf("reopen the history database: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	reopened, err := New(second.DB(), WithClock(clocktesting.NewFakeClock(testStart)))
	if err != nil {
		t.Fatalf("reopen the cooldown store: %v", err)
	}
	stored, found, err := reopened.Get(ctx, testFingerprint)
	if err != nil || !found {
		t.Fatalf("get the cooldown after reopening: found=%v err=%v", found, err)
	}
	if stored.NextBackoffLevel != 1 || !stored.CooldownUntil.Equal(window.CooldownUntil) {
		t.Fatalf("stored cooldown = %+v, want the previous window", stored)
	}
	if _, err := reopened.BeginPage(ctx, testRecord(), testStart.Add(time.Minute)); !errors.Is(err,
		ErrNotOpen) {
		t.Fatalf("begin inside the restored cooldown: err = %v, want ErrNotOpen", err)
	}
}

// TestNowUsesTheConfiguredClock keeps the fake-clock contract explicit: a test
// that steps the clock must move every stamp the store writes.
func TestNowUsesTheConfiguredClock(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	fake, ok := store.clock.(*clocktesting.FakeClock)
	if !ok {
		t.Fatalf("clock = %T, want a fake clock", store.clock)
	}
	fake.Step(time.Hour)
	if got := store.now(); !got.Equal(testStart.Add(time.Hour)) {
		t.Fatalf("now = %s, want %s", got, testStart.Add(time.Hour))
	}
}
