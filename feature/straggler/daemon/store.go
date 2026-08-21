package daemon

import "sync"

// store is a mutex-protected ring of recent CycleResults. It is the sole data
// source for /status (counters + last_cycle) and /straggler/results/*: only
// cycles from THIS daemon session are visible, so a restart starts with no
// history.
type store struct {
	mu     sync.Mutex
	cycles []*CycleResult
	limit  int
	total  int // cycles started this session (failed included)
	failed int // cycles that errored this session
}

func newStore(limit int) *store {
	if limit <= 0 {
		limit = 50
	}
	return &store{limit: limit}
}

// add appends a finished cycle, trimming the ring to the limit.
func (s *store) add(c *CycleResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	if c.Error != "" {
		s.failed++
	}
	s.cycles = append(s.cycles, c)
	if len(s.cycles) > s.limit {
		s.cycles = s.cycles[len(s.cycles)-s.limit:]
	}
}

// latest returns the most recent finished cycle, or nil when none.
func (s *store) latest() *CycleResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cycles) == 0 {
		return nil
	}
	return s.cycles[len(s.cycles)-1]
}

// get returns the cycle with the given id from this session, or nil.
func (s *store) get(id int) *CycleResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.cycles) - 1; i >= 0; i-- {
		if s.cycles[i].ID == id {
			return s.cycles[i]
		}
	}
	return nil
}

// list returns this session's cycles, newest first (a snapshot copy).
func (s *store) list() []*CycleResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*CycleResult, 0, len(s.cycles))
	for i := len(s.cycles) - 1; i >= 0; i-- {
		out = append(out, s.cycles[i])
	}
	return out
}

// counts returns (total, failed) cycles for this session.
func (s *store) counts() (total, failed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total, s.failed
}
