package loop

import (
	"context"
	"testing"
	"time"
)

type activityReporter struct {
	NopReporter
	started []string
	done    int
}

func (r *activityReporter) Activity(label string, elapsed time.Duration) {
	if elapsed == 0 {
		r.started = append(r.started, label)
	}
}

func (r *activityReporter) ActivityDone() { r.done++ }

// The caller is blocked while a fixer, test, hook or push runs. Progress has
// to start before that work and keep ticking without relying on the command to
// produce output of its own.
func TestAwaitReportsImmediatelyAndTicksUntilWorkEnds(t *testing.T) {
	release := make(chan struct{})
	ticks := make(chan time.Duration, 1)
	done := make(chan error, 1)

	go func() {
		done <- await(context.Background(), time.Millisecond, func(elapsed time.Duration) {
			select {
			case ticks <- elapsed:
			default:
			}
		}, func() error {
			<-release
			return nil
		})
	}()

	if elapsed := <-ticks; elapsed != 0 {
		t.Fatalf("first progress update = %s, want immediate zero", elapsed)
	}
	select {
	case elapsed := <-ticks:
		if elapsed <= 0 {
			t.Fatalf("later progress update = %s, want elapsed time", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked work produced no progress tick")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
