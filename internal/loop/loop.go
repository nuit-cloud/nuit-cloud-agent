// Package loop is the agent's heartbeat: call the platform, check what it hands out, run what is allowed, report.
package loop

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/client"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/profiles"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/state"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
)

// Platform is the part of the platform's API the loop uses.
type Platform interface {
	CheckIn(ctx context.Context) (*client.CheckInResponse, error)
	ReportTask(ctx context.Context, taskID string, report client.Report) error
}

// TaskRunner performs a verified task.
type TaskRunner interface {
	Names() []string
	Run(ctx context.Context, task *taskverify.Task) client.Report
}

const (
	defaultInterval = 30 * time.Second
	maxBackoff      = 5 * time.Minute
	// reportAttempts is how often a result is sent before the agent gives up on it (the platform then fails the task itself).
	reportAttempts = 5
)

// Guard watches a freshly installed version (update.Guard).
type Guard interface {
	// Started is called once when the agent starts; true means the version running has failed to prove itself too often.
	Started() bool
	// Confirm is called after a successful check-in.
	Confirm()
	// Unconfirmed reports whether the running version is an update that has not yet proved itself.
	Unconfirmed() bool
	// Overdue reports whether an unconfirmed update has run out of time.
	Overdue(now time.Time) bool
	// Rollback puts the previous build back.
	Rollback() error
}

// Loop is one agent's heartbeat.
type Loop struct {
	State    *state.State
	Platform Platform
	Runner   TaskRunner
	Now      func() time.Time
	Log      func(format string, args ...any)
	// Sleep waits for d or until ctx ends. Injectable so tests do not wait.
	Sleep func(ctx context.Context, d time.Duration)
	// Guard, if set, confirms or rolls back a freshly installed version.
	Guard Guard
	// Restart stops the loop so the service manager can start the agent again (after an update or a rollback).
	Restart func()
	// FallbackSink is told the fixed addresses the platform names for reaching it when its name does not work (an empty list clears
	// them). It is not called when the platform says nothing about them.
	FallbackSink func(list []string)
	// Route says how the platform was last reached, for the log.
	Route func() string
	// ClockOffset is the platform's time minus this machine's, if it has been measured. A task's expiry is judged by this machine's time plus it.
	ClockOffset func() (time.Duration, bool)

	mu      sync.Mutex
	running bool
	wg      sync.WaitGroup
}

func (l *Loop) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// platformNow is the time a task's expiry is judged by: the platform's, as far as it has told this machine.
func (l *Loop) platformNow() time.Time {
	now := l.now()
	if l.ClockOffset != nil {
		if d, ok := l.ClockOffset(); ok && d > -365*24*time.Hour && d < 365*24*time.Hour {
			return now.Add(d)
		}
	}
	return now
}

func (l *Loop) logf(format string, args ...any) {
	if l.Log != nil {
		l.Log(format, args...)
	}
}

// Wait blocks until a task that is running has finished (used by tests and at shutdown).
func (l *Loop) Wait() { l.wg.Wait() }

// Once makes one call to the platform and starts whatever tasks it handed out and the agent may run. It returns how long to
// wait before the next call. A long task runs in the background, so the agent keeps checking in while it works; one task runs
// at a time, and any others are left with the platform, which offers them again.
func (l *Loop) Once(ctx context.Context) (time.Duration, error) {
	resp, err := l.Platform.CheckIn(ctx)
	if err != nil {
		return 0, err
	}
	if l.Guard != nil {
		l.Guard.Confirm() // a successful check-in is what a new version has to prove
	}
	if l.FallbackSink != nil && resp.FallbackAddresses != nil {
		l.FallbackSink(*resp.FallbackAddresses)
	}
	interval := time.Duration(resp.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = defaultInterval
	}
	for _, env := range resp.Tasks {
		task, err := taskverify.Verify(env, l.State.TrustedKeys, taskverify.Context{
			MachineID:   l.State.MachineID,
			Now:         l.platformNow(),
			AlreadySeen: l.State.HasSeen,
			KnownTasks:  l.Runner.Names(),
		})
		if err != nil {
			l.refused(ctx, err)
			continue
		}
		if !l.start(ctx, task) {
			l.logf("busy: task %s (%s) left for the next check-in", task.ID, task.Name)
		}
	}
	return interval, nil
}

// refused handles a task that must not run. If the signature verified, the platform is told why; if it did not, nothing in
// the envelope is trusted, so it is only logged.
func (l *Loop) refused(ctx context.Context, err error) {
	l.logf("refused a task: %v", err)
	var r *taskverify.Refusal
	if errors.As(err, &r) && r.Task != nil {
		one := 1
		if rerr := l.Platform.ReportTask(ctx, r.Task.ID, client.Report{Status: "failed", ExitCode: &one, Output: "refused by the agent: " + r.Reason}); rerr != nil {
			l.logf("could not report the refusal of %s: %v", r.Task.ID, rerr)
		}
	}
}

func (l *Loop) start(ctx context.Context, task *taskverify.Task) bool {
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		return false
	}
	l.running = true
	l.mu.Unlock()

	// Remembered on disk BEFORE it runs: a crash in the middle can never lead to it running twice.
	if err := l.State.MarkSeen(task.ID, l.now()); err != nil {
		l.logf("could not save state before running %s, so not running it: %v", task.ID, err)
		l.mu.Lock()
		l.running = false
		l.mu.Unlock()
		return true
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer func() {
			l.mu.Lock()
			l.running = false
			l.mu.Unlock()
		}()
		l.report(ctx, task.ID, client.Report{Status: "running"})
		l.logf("running %s (%s)", task.Name, task.ID)
		final := l.Runner.Run(ctx, task)
		l.report(ctx, task.ID, final)
		l.logf("finished %s (%s): %s", task.Name, task.ID, final.Status)
		// A task that changed the agent itself (an update) is reported first, then the agent restarts to become the new one.
		if rr, ok := l.Runner.(interface{ RestartRequested() bool }); ok && rr.RestartRequested() && l.Restart != nil {
			l.logf("restarting to finish %s", task.Name)
			l.Restart()
		}
	}()
	return true
}

// maxReportOutput is how much of a task's output is sent: the platform keeps 64 KB, so sending more is only waste.
const maxReportOutput = 60 * 1024

func (l *Loop) report(ctx context.Context, id string, r client.Report) {
	r.Output = profiles.Truncate(r.Output, maxReportOutput)
	var err error
	for attempt := 1; attempt <= reportAttempts; attempt++ {
		if err = l.Platform.ReportTask(ctx, id, r); err == nil {
			return
		}
		var se *client.StatusError
		if errors.As(err, &se) && se.Status == http.StatusRequestEntityTooLarge && r.Result != nil {
			// Too big for the platform: send what matters without the findings, so the task does not hang until it times out.
			l.logf("the report for task %s was too large; sending it without its findings", id)
			r.Result = nil
			r.Output = profiles.Truncate(r.Output, maxReportOutput/2) + "\n(the findings were too large to send)"
			continue
		}
		if errors.As(err, &se) && se.Status >= 400 && se.Status < 500 {
			break // the platform said no; asking again will not change that
		}
		l.sleep(ctx, time.Duration(attempt)*2*time.Second)
	}
	l.logf("could not report %s for task %s: %v", r.Status, id, err)
}

func (l *Loop) sleep(ctx context.Context, d time.Duration) {
	if l.Sleep != nil {
		l.Sleep(ctx, d)
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// Run calls the platform until ctx ends. A failed call is retried with a growing wait (and some jitter, so a fleet that lost
// the platform does not return in step); a 401 means the agent is not, or no longer, accepted, so it waits long between tries.
func (l *Loop) Run(ctx context.Context) {
	if l.Guard != nil && l.Guard.Started() {
		l.rollback("it has restarted too many times without proving itself")
	}
	failures := 0
	var outageSince time.Time
	for ctx.Err() == nil {
		if l.Guard != nil && l.Guard.Overdue(l.now()) {
			l.rollback("it could not check in with the platform in time")
			break
		}
		next, err := l.Once(ctx)
		if err != nil {
			failures++
			if outageSince.IsZero() {
				outageSince = l.now()
			}
			var se *client.StatusError
			if errors.As(err, &se) && se.Unauthorized() {
				l.logf("the platform does not accept this agent (%v); will try again in %v", err, maxBackoff)
				next = maxBackoff
			} else {
				next = backoff(failures)
				if l.Guard != nil && l.Guard.Unconfirmed() {
					next = defaultInterval // an update that cannot check in is on the clock: keep trying, and keep watching the clock
				}
				l.logf("could not reach the platform (%v) for %v; trying again in %v", err, l.now().Sub(outageSince).Round(time.Second), next)
			}
		} else {
			if failures > 0 {
				route := ""
				if l.Route != nil {
					route = ", " + l.Route()
				}
				l.logf("reached the platform again after %v%s", l.now().Sub(outageSince).Round(time.Second), route)
			}
			failures = 0
			outageSince = time.Time{}
		}
		l.sleep(ctx, jitter(next))
	}
	l.Wait()
}

func backoff(failures int) time.Duration {
	d := defaultInterval
	for i := 1; i < failures && d < maxBackoff; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

func jitter(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int63n(int64(d)/10+1))
}

// rollback puts the previous build back and asks to be restarted into it.
func (l *Loop) rollback(why string) {
	l.logf("rolling back the update: %s", why)
	if err := l.Guard.Rollback(); err != nil {
		l.logf("the rollback could not be completed: %v", err)
	}
	if l.Restart != nil {
		l.Restart()
	}
}
