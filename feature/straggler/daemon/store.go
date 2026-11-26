package daemon

import "sync"

// store is a mutex-protected slice of recent CycleResults. It is the sole data
// source for /status (counters + last_cycle) and /straggler/results/*: only
// cycles from THIS daemon session are visible, so a restart starts with no
// history. The slice is unbounded — every finished cycle of the session is
// kept, so history can list all of this session's collection/analysis runs.
type store struct {
	mu     sync.Mutex
	cycles []*CycleResult
	total  int // cycles started this session (failed included)
	failed int // cycles that errored this session
}

func newStore() *store {
	return &store{}
}

// add appends a finished cycle. History is unbounded: no trimming.
func (s *store) add(c *CycleResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	if c.Error != "" {
		s.failed++
	}
	s.cycles = append(s.cycles, c)
}

// latest returns a snapshot of the most recent finished cycle, or nil when none.
func (s *store) latest() *CycleResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cycles) == 0 {
		return nil
	}
	c := *s.cycles[len(s.cycles)-1]
	return &c
}

// get returns a snapshot of the cycle with the given id from this session, or nil.
func (s *store) get(id int) *CycleResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.cycles) - 1; i >= 0; i-- {
		if s.cycles[i].ID == id {
			c := *s.cycles[i]
			return &c
		}
	}
	return nil
}

// list returns snapshots of this session's cycles, newest first.
func (s *store) list() []*CycleResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*CycleResult, 0, len(s.cycles))
	for i := len(s.cycles) - 1; i >= 0; i-- {
		c := *s.cycles[i]
		out = append(out, &c)
	}
	return out
}

// setProfilerData updates the stored cycle's profiler-data fields under the
// store lock. Readers receive snapshots, so post-publish updates are safe.
func (s *store) setProfilerData(id int, res profilerDataResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.cycles) - 1; i >= 0; i-- {
		if s.cycles[i].ID != id {
			continue
		}
		c := s.cycles[i]
		c.DumpProfilerData = res.Dump
		c.ProfilerDataStatus = res.Status
		c.ProfilerDataBytes = res.Bytes
		c.ProfilerDataSourceBytes = res.SourceBytes
		c.ProfilerDataError = res.Error
		return
	}
}

// counts returns (total, failed) cycles for this session.
func (s *store) counts() (total, failed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total, s.failed
}
