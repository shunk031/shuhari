// Package progress reports what a long evaluation is doing while it does it.
//
// An evaluation runs for tens of minutes and, without this, prints nothing
// until it finishes. That leaves an operator unable to tell a slow run from a
// stuck one, and leaves an agent driving Shuhari with nothing to read but
// process tables and half-written artifact directories.
//
// Events are JSON Lines on stderr, one object per line, so stdout keeps
// carrying only the verdict. Keys are stable and flat: a consumer can filter on
// `phase` and `event` without parsing prose.
package progress

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Phase names the stage of an evaluation an event belongs to.
type Phase string

const (
	// PhaseRun is one agent invocation for one case, trial, and variant.
	PhaseRun Phase = "run"
	// PhaseGrade is one blinded judgement of one side of one trial.
	PhaseGrade Phase = "grade"
	// PhaseCompare is the blind A/B comparison for one trial.
	PhaseCompare Phase = "compare"
)

// Event kinds. A `start` is always followed by exactly one `finish` for the
// same identifiers, so a consumer can pair them without heuristics.
const (
	EventStart  = "start"
	EventFinish = "finish"
)

// Event is one line of progress output.
//
// Optional fields are omitted rather than zeroed, so a consumer can distinguish
// "not applicable to this phase" from "zero".
type Event struct {
	Timestamp  string `json:"ts"`
	Phase      Phase  `json:"phase"`
	Event      string `json:"event"`
	Case       string `json:"case,omitempty"`
	Trial      int    `json:"trial,omitempty"`
	Variant    string `json:"variant,omitempty"`
	Side       string `json:"side,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Status     string `json:"status,omitempty"`
	Error      string `json:"error,omitempty"`
	Completed  int    `json:"completed,omitempty"`
	Total      int    `json:"total,omitempty"`
}

// Reporter writes progress events. The zero value discards them, so callers
// never need a nil check and disabling progress costs nothing at the call site.
type Reporter struct {
	mutex   sync.Mutex
	writer  io.Writer
	counts  map[Phase]int
	totals  map[Phase]int
	nowFunc func() time.Time
}

// New returns a Reporter writing to w. A nil writer discards events.
func New(w io.Writer) *Reporter {
	return &Reporter{writer: w, counts: map[Phase]int{}, totals: map[Phase]int{}, nowFunc: time.Now}
}

// Discard returns a Reporter that writes nothing.
func Discard() *Reporter { return New(nil) }

// SetTotal records how many units a phase expects, so each finish can report
// completed-of-total rather than leaving a consumer to infer the denominator.
func (r *Reporter) SetTotal(phase Phase, total int) {
	if r == nil || r.writer == nil {
		return
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.totals[phase] = total
}

// Emit writes one event. Concurrent calls are serialized so lines never
// interleave, which is what makes the stream parseable under `--jobs`.
func (r *Reporter) Emit(event Event) {
	if r == nil || r.writer == nil {
		return
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()

	event.Timestamp = r.nowFunc().UTC().Format(time.RFC3339)
	if event.Event == EventFinish {
		r.counts[event.Phase]++
		event.Completed = r.counts[event.Phase]
		event.Total = r.totals[event.Phase]
	}

	encoded, err := json.Marshal(event)
	if err != nil {
		return
	}
	_, _ = r.writer.Write(append(encoded, '\n'))
}

// Started emits a start event and returns the function that emits its finish,
// so a caller cannot report a start without also reporting its end.
func (r *Reporter) Started(event Event) func(status string, err error) {
	event.Event = EventStart
	r.Emit(event)
	started := time.Now()
	if r != nil && r.writer != nil {
		started = r.nowFunc()
	}

	return func(status string, err error) {
		finish := event
		finish.Event = EventFinish
		finish.Status = status
		finish.DurationMS = time.Since(started).Milliseconds()
		if r != nil && r.writer != nil {
			finish.DurationMS = r.nowFunc().Sub(started).Milliseconds()
		}
		if err != nil {
			finish.Error = err.Error()
		}
		r.Emit(finish)
	}
}
