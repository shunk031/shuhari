package progress

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

func decode(t *testing.T, buffer *bytes.Buffer) []Event {
	t.Helper()
	var events []Event
	for _, line := range strings.Split(strings.TrimSpace(buffer.String()), "\n") {
		if line == "" {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("line is not JSON: %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func TestStartedPairsWithFinish(t *testing.T) {
	buffer := &bytes.Buffer{}
	reporter := New(buffer)
	reporter.SetTotal(PhaseRun, 2)

	finish := reporter.Started(Event{Phase: PhaseRun, Case: "demo", Trial: 1, Variant: "with_skill"})
	finish("ok", nil)

	events := decode(t, buffer)
	if len(events) != 2 {
		t.Fatalf("got %d events, want a start and a finish", len(events))
	}
	if events[0].Event != EventStart || events[1].Event != EventFinish {
		t.Fatalf("events are not a start/finish pair: %+v", events)
	}
	// A consumer pairs them on the identifiers, so they must survive.
	for _, event := range events {
		if event.Case != "demo" || event.Trial != 1 || event.Variant != "with_skill" {
			t.Fatalf("identifiers lost: %+v", event)
		}
		if event.Timestamp == "" {
			t.Fatalf("event carries no timestamp: %+v", event)
		}
	}
	if events[1].Completed != 1 || events[1].Total != 2 {
		t.Fatalf("finish did not report progress: completed=%d total=%d", events[1].Completed, events[1].Total)
	}
}

func TestFinishCarriesTheError(t *testing.T) {
	buffer := &bytes.Buffer{}
	reporter := New(buffer)

	finish := reporter.Started(Event{Phase: PhaseGrade, Case: "demo", Trial: 2})
	finish("error", errors.New("judge refused"))

	events := decode(t, buffer)
	last := events[len(events)-1]
	if last.Status != "error" || last.Error != "judge refused" {
		t.Fatalf("failure not reported: %+v", last)
	}
}

func TestDiscardWritesNothing(t *testing.T) {
	reporter := Discard()
	// The zero-cost path still has to return a usable finish function, or every
	// call site would need a nil check.
	reporter.Started(Event{Phase: PhaseRun})("ok", nil)
	reporter.Emit(Event{Phase: PhaseRun})
}

func TestConcurrentEmitsDoNotInterleave(t *testing.T) {
	buffer := &bytes.Buffer{}
	reporter := New(buffer)
	reporter.SetTotal(PhaseRun, 50)

	var group sync.WaitGroup
	for index := range 50 {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			reporter.Started(Event{Phase: PhaseRun, Case: "demo", Trial: index + 1})("ok", nil)
		}(index)
	}
	group.Wait()

	// Under --jobs the emitters are concurrent; a torn line would make the
	// stream unparseable, which is the whole point of the format.
	events := decode(t, buffer)
	if len(events) != 100 {
		t.Fatalf("got %d events, want 100", len(events))
	}
	seen := map[int]bool{}
	for _, event := range events {
		if event.Event == EventFinish {
			seen[event.Completed] = true
		}
	}
	for n := 1; n <= 50; n++ {
		if !seen[n] {
			t.Fatalf("completed count %d never appeared; counter is not serialized", n)
		}
	}
}
