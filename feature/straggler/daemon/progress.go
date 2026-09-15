package daemon

import (
	"fmt"
	"sync"
	"time"
)

const progressMaxLines = 200

// progressLine is one timestamped entry of a cycle's live stage log.
type progressLine struct {
	Ts   string `json:"ts"`
	Text string `json:"text"`
}

// progressLog is the in-memory, per-cycle stage log the web console renders
// terminal-style. It is reset at the start of every cycle, appended to as the
// cycle advances, and kept after the cycle ends so the last cycle stays visible.
type progressLog struct {
	mu       sync.Mutex
	cycle    int
	inFlight bool
	started  time.Time
	lines    []progressLine
}

func newProgressLog() *progressLog { return &progressLog{} }

// begin resets the log for a newly started cycle.
func (p *progressLog) begin(cycle int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cycle = cycle
	p.inFlight = true
	p.started = time.Now()
	p.lines = nil
}

// step appends one timestamped line.
func (p *progressLog) step(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lines = append(p.lines, progressLine{
		Ts:   time.Now().Format("15:04:05"),
		Text: fmt.Sprintf(format, args...),
	})
	if len(p.lines) > progressMaxLines {
		p.lines = p.lines[len(p.lines)-progressMaxLines:]
	}
}

// finish marks the cycle as no longer in flight (lines are retained).
func (p *progressLog) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inFlight = false
}

// progressSnapshot is the GET /daemon/progress payload.
type progressSnapshot struct {
	Cycle    int            `json:"cycle"`
	InFlight bool           `json:"in_flight"`
	Started  string         `json:"started,omitempty"`
	Lines    []progressLine `json:"lines"`
}

func (p *progressLog) snapshot() progressSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	lines := make([]progressLine, len(p.lines))
	copy(lines, p.lines)
	out := progressSnapshot{Cycle: p.cycle, InFlight: p.inFlight, Lines: lines}
	if !p.started.IsZero() {
		out.Started = p.started.Format(time.RFC3339)
	}
	return out
}
