package tools

import (
	"context"
	"sync"
	"time"
)

// MemoryStore is an in-process evidence store. It is the store the unit suites
// use; pagerless-server backs the same interface with the SQLite history file so
// that a restart does not lose a citation.
type MemoryStore struct {
	mu       sync.Mutex
	records  map[string]Record
	served   map[string]int
	bytes    map[string]int64
	refusals map[string]map[string]int
	now      func() time.Time
}

// NewMemoryStore returns an empty in-process evidence store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		records:  map[string]Record{},
		served:   map[string]int{},
		bytes:    map[string]int64{},
		refusals: map[string]map[string]int{},
		now:      time.Now,
	}
}

// Record stores one call, replacing an earlier record with the same identity so
// that an idempotent retry is not counted twice.
func (s *MemoryStore) Record(_ context.Context, record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := record.PageUID + "\x00" + record.CallID
	if _, exists := s.records[key]; !exists && record.Outcome == "ok" {
		s.served[record.PageUID]++
		s.bytes[record.PageUID] += record.Bytes
	}
	s.records[key] = record
	return nil
}

// Get returns one stored call.
func (s *MemoryStore) Get(_ context.Context, pageUID, callID string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[pageUID+"\x00"+callID]
	return record, ok, nil
}

// Served returns the number of calls the Page has made.
func (s *MemoryStore) Served(_ context.Context, pageUID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.served[pageUID], nil
}

// Bytes returns the bytes of results the Page has been served.
func (s *MemoryStore) Bytes(_ context.Context, pageUID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes[pageUID], nil
}

// Refusals increments and returns the count of out-of-catalogue attempts.
func (s *MemoryStore) Refusals(_ context.Context, pageUID, tool string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refusals[pageUID] == nil {
		s.refusals[pageUID] = map[string]int{}
	}
	s.refusals[pageUID][tool]++
	return s.refusals[pageUID][tool], nil
}
