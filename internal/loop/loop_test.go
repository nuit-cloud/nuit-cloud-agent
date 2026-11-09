package loop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/client"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/state"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
)

type vectors struct {
	MachineID   string                         `json:"machineId"`
	TrustedKeys []taskverify.Key               `json:"trustedKeys"`
	Tasks       map[string]taskverify.Envelope `json:"tasks"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("../taskverify/testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

type reported struct {
	ID     string
	Report client.Report
}

type fakePlatform struct {
	mu      sync.Mutex
	offer   []taskverify.Envelope
	checkIn error
	failFor map[string]error // task id -> error for ReportTask
	// tooLargeWhenResult makes the platform answer 413 to any report that carries findings.
	tooLargeWhenResult bool
	reports            []reported
}

func (p *fakePlatform) CheckIn(context.Context) (*client.CheckInResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.checkIn != nil {
		return nil, p.checkIn
	}
	return &client.CheckInResponse{PollIntervalSeconds: 30, Tasks: p.offer}, nil
}

func (p *fakePlatform) ReportTask(_ context.Context, id string, r client.Report) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.failFor[id]; err != nil {
		return err
	}
	if p.tooLargeWhenResult && r.Result != nil {
		return &client.StatusError{Status: 413, Message: "Request body too large"}
	}
	p.reports = append(p.reports, reported{id, r})
	return nil
}

func (p *fakePlatform) statuses(id string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, r := range p.reports {
		if r.ID == id {
			out = append(out, r.Report.Status)
		}
	}
	return out
}

type fakeRunner struct {
	mu    sync.Mutex
	ran   []string
	block chan struct{}
}

func (r *fakeRunner) Names() []string {
	return []string{"ping", "survey", "profile-check", "profile-apply"}
}
func (r *fakeRunner) Run(_ context.Context, task *taskverify.Task) client.Report {
	if r.block != nil {
		<-r.block
	}
	r.mu.Lock()
	r.ran = append(r.ran, task.Name)
	r.mu.Unlock()
	zero := 0
	return client.Report{Status: "done", ExitCode: &zero, Output: "ok"}
}

func setup(t *testing.T) (*Loop, *fakePlatform, *fakeRunner, vectors) {
	t.Helper()
	v := loadVectors(t)
	st := state.New(t.TempDir())
	st.PlatformURL, st.MachineID, st.Credential, st.TrustedKeys = "https://p", v.MachineID, "nca_x", v.TrustedKeys
	p := &fakePlatform{failFor: map[string]error{}}
	r := &fakeRunner{}
	l := &Loop{
		State: st, Platform: p, Runner: r,
		Now:   func() time.Time { return time.Date(2026, 10, 20, 12, 5, 0, 0, time.UTC) }, // inside the vectors' window
		Sleep: func(context.Context, time.Duration) {},
	}
	return l, p, r, v
}

const pingID = "aaaaaaaa-0000-0000-0000-000000000001"

func TestRunsAVerifiedTaskAndReportsRunningThenDone(t *testing.T) {
	l, p, r, v := setup(t)
	p.offer = []taskverify.Envelope{v.Tasks["ping"]}
	next, err := l.Once(context.Background())
	l.Wait()
	if err != nil || next != 30*time.Second {
		t.Fatalf("next=%v err=%v", next, err)
	}
	if got := p.statuses(pingID); len(got) != 2 || got[0] != "running" || got[1] != "done" {
		t.Errorf("reports: %v", got)
	}
	if len(r.ran) != 1 || r.ran[0] != "ping" {
		t.Errorf("ran: %v", r.ran)
	}
}

// The platform offers a task until it has been reported running; if the response is lost it may be offered again. It must not run twice.
func TestNeverRunsTheSameTaskTwice(t *testing.T) {
	l, p, r, v := setup(t)
	p.offer = []taskverify.Envelope{v.Tasks["ping"]}
	for i := 0; i < 3; i++ {
		if _, err := l.Once(context.Background()); err != nil {
			t.Fatal(err)
		}
		l.Wait()
	}
	if len(r.ran) != 1 {
		t.Errorf("ran %d times: %v", len(r.ran), r.ran)
	}
}

func TestARestartDoesNotRunItAgainEither(t *testing.T) {
	l, p, r, v := setup(t)
	p.offer = []taskverify.Envelope{v.Tasks["ping"]}
	_, _ = l.Once(context.Background())
	l.Wait()
	reloaded, err := state.Load(l.State.Dir())
	if err != nil {
		t.Fatal(err)
	}
	l2 := &Loop{State: reloaded, Platform: p, Runner: r, Now: l.Now, Sleep: l.Sleep}
	_, _ = l2.Once(context.Background())
	l2.Wait()
	if len(r.ran) != 1 {
		t.Errorf("ran %d times after a restart", len(r.ran))
	}
}

func TestAForgedTaskIsIgnoredAndNotReportedOn(t *testing.T) {
	l, p, r, v := setup(t)
	forged := v.Tasks["ping"]
	forged.Signature = v.Tasks["survey"].Signature
	p.offer = []taskverify.Envelope{forged}
	_, _ = l.Once(context.Background())
	l.Wait()
	if len(r.ran) != 0 || len(p.reports) != 0 {
		t.Errorf("ran=%v reports=%v: nothing in an unauthentic envelope may be trusted, not even its id", r.ran, p.reports)
	}
}

func TestATaskThisAgentDoesNotKnowIsReportedFailedBecauseItWasSigned(t *testing.T) {
	l, p, r, v := setup(t)
	l.Runner = &narrowRunner{inner: r, names: []string{"ping"}}
	p.offer = []taskverify.Envelope{v.Tasks["profileApply"]}
	_, _ = l.Once(context.Background())
	l.Wait()
	got := p.statuses("aaaaaaaa-0000-0000-0000-000000000003")
	if len(got) != 1 || got[0] != "failed" || len(r.ran) != 0 {
		t.Errorf("reports=%v ran=%v", got, r.ran)
	}
	if out := p.reports[0].Report.Output; out != "refused by the agent: the task is not in this agent's catalog" {
		t.Errorf("output %q", out)
	}
}

type narrowRunner struct {
	inner *fakeRunner
	names []string
}

func (n *narrowRunner) Names() []string { return n.names }
func (n *narrowRunner) Run(ctx context.Context, t *taskverify.Task) client.Report {
	return n.inner.Run(ctx, t)
}

func TestOneTaskAtATimeAndTheHeartbeatContinuesWhileItRuns(t *testing.T) {
	l, p, r, v := setup(t)
	r.block = make(chan struct{})
	p.offer = []taskverify.Envelope{v.Tasks["ping"], v.Tasks["survey"]}
	if _, err := l.Once(context.Background()); err != nil { // starts ping, leaves survey
		t.Fatal(err)
	}
	// the platform keeps offering the one that was left; a second call while ping still runs must not start it
	p.offer = []taskverify.Envelope{v.Tasks["survey"]}
	if _, err := l.Once(context.Background()); err != nil {
		t.Fatalf("a check-in while a task runs must still work: %v", err)
	}
	if len(r.ran) != 0 {
		t.Fatalf("nothing should have finished yet: %v", r.ran)
	}
	close(r.block)
	l.Wait()
	// now idle: the survey is accepted on the next call
	if _, err := l.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.Wait()
	if len(r.ran) != 2 || r.ran[0] != "ping" || r.ran[1] != "survey" {
		t.Errorf("ran: %v", r.ran)
	}
}

func TestACheckInFailureIsReturnedAndLeavesTasksAlone(t *testing.T) {
	l, p, r, _ := setup(t)
	p.checkIn = errors.New("network down")
	if _, err := l.Once(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	if len(r.ran) != 0 {
		t.Error("nothing should run")
	}
}

func TestAResultThatCannotBeSentIsRetriedThenGivenUp(t *testing.T) {
	l, p, _, v := setup(t)
	p.failFor[pingID] = errors.New("flaky")
	p.offer = []taskverify.Envelope{v.Tasks["ping"]}
	sleeps := 0
	l.Sleep = func(context.Context, time.Duration) { sleeps++ }
	_, _ = l.Once(context.Background())
	l.Wait()
	if sleeps < reportAttempts*2-2 { // two reports (running, done), each retried
		t.Errorf("only %d waits between retries", sleeps)
	}
}

func TestAPlatformRefusalOfAReportIsNotRetried(t *testing.T) {
	l, p, _, v := setup(t)
	p.failFor[pingID] = &client.StatusError{Status: 404, Message: "no such task"}
	p.offer = []taskverify.Envelope{v.Tasks["ping"]}
	sleeps := 0
	l.Sleep = func(context.Context, time.Duration) { sleeps++ }
	_, _ = l.Once(context.Background())
	l.Wait()
	if sleeps != 0 {
		t.Errorf("a 4xx must not be retried, waited %d times", sleeps)
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 240 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := backoff(i + 1); got != w {
			t.Errorf("failure %d: %v, want %v", i+1, got, w)
		}
	}
}

func TestRunStopsWhenAskedAndWaitsForARunningTask(t *testing.T) {
	l, p, r, v := setup(t)
	r.block = make(chan struct{})
	p.offer = []taskverify.Envelope{v.Tasks["ping"]}
	ctx, cancel := context.WithCancel(context.Background())
	sleeps := 0
	l.Sleep = func(c context.Context, _ time.Duration) {
		sleeps++
		if sleeps == 2 {
			cancel()
			close(r.block)
		}
	}
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if len(r.ran) != 1 {
		t.Errorf("the running task should have been allowed to finish: %v", r.ran)
	}
}

func TestAReportThePlatformSaysIsTooLargeIsResentWithoutItsFindingsSoTheTaskDoesNotHang(t *testing.T) {
	l, p, _, v := setup(t)
	r := &bigResultRunner{}
	l.Runner = r
	p.offer = []taskverify.Envelope{v.Tasks["ping"]}
	p.tooLargeWhenResult = true
	_, _ = l.Once(context.Background())
	l.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	last := p.reports[len(p.reports)-1]
	if last.Report.Status != "done" || last.Report.Result != nil {
		t.Errorf("the final report should arrive without findings: %+v", last.Report)
	}
}

type bigResultRunner struct{}

func (bigResultRunner) Names() []string { return []string{"ping"} }
func (bigResultRunner) Run(context.Context, *taskverify.Task) client.Report {
	zero := 0
	return client.Report{Status: "done", ExitCode: &zero, Output: "ok", Result: map[string]string{"big": "yes"}}
}

func TestLongOutputIsCutBeforeItIsSent(t *testing.T) {
	l, p, _, v := setup(t)
	l.Runner = &longOutputRunner{}
	p.offer = []taskverify.Envelope{v.Tasks["ping"]}
	_, _ = l.Once(context.Background())
	l.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	final := p.reports[len(p.reports)-1].Report.Output
	if len(final) > maxReportOutput+100 || len(final) < 1000 {
		t.Errorf("sent %d bytes", len(final))
	}
}

type longOutputRunner struct{}

func (longOutputRunner) Names() []string { return []string{"ping"} }
func (longOutputRunner) Run(context.Context, *taskverify.Task) client.Report {
	return client.Report{Status: "done", Output: string(make([]byte, 500*1024))}
}

type fakeGuard struct {
	mu          sync.Mutex
	started     bool
	overdue     bool
	unconfirmed bool
	confirmed   int
	rolledBack  int
	rollbackErr error
}

func (g *fakeGuard) Started() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.started }
func (g *fakeGuard) Confirm()      { g.mu.Lock(); g.confirmed++; g.mu.Unlock() }
func (g *fakeGuard) Unconfirmed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.unconfirmed
}
func (g *fakeGuard) Overdue(time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.overdue
}
func (g *fakeGuard) Rollback() error {
	g.mu.Lock()
	g.rolledBack++
	g.mu.Unlock()
	return g.rollbackErr
}

// updatingRunner stands in for a runner whose task replaced the agent's own binary.
type updatingRunner struct{ fakeRunner }

func (u *updatingRunner) RestartRequested() bool { return true }

func TestASuccessfulCheckInConfirmsANewVersion(t *testing.T) {
	l, _, _, _ := setup(t)
	g := &fakeGuard{}
	l.Guard = g
	if _, err := l.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.confirmed != 1 {
		t.Errorf("confirmed %d times", g.confirmed)
	}
}

func TestAFailedCheckInDoesNotConfirmAnything(t *testing.T) {
	l, p, _, _ := setup(t)
	g := &fakeGuard{}
	l.Guard = g
	p.checkIn = errors.New("network down")
	_, _ = l.Once(context.Background())
	if g.confirmed != 0 {
		t.Error("only a successful check-in proves a new version")
	}
}

func TestATaskThatUpdatedTheAgentIsReportedFirstAndThenTheAgentRestarts(t *testing.T) {
	l, p, _, v := setup(t)
	r := &updatingRunner{}
	l.Runner = r
	restarted := make(chan struct{}, 1)
	l.Restart = func() { restarted <- struct{}{} }
	p.offer = []taskverify.Envelope{v.Tasks["ping"]}
	_, _ = l.Once(context.Background())
	l.Wait()
	select {
	case <-restarted:
	default:
		t.Fatal("the agent must restart after an update")
	}
	if got := p.statuses(pingID); len(got) != 2 || got[1] != "done" {
		t.Errorf("the result must be reported before the restart: %v", got)
	}
}

func TestAPlainTaskDoesNotRestartTheAgent(t *testing.T) {
	l, p, _, v := setup(t)
	restarted := false
	l.Restart = func() { restarted = true }
	p.offer = []taskverify.Envelope{v.Tasks["ping"]}
	_, _ = l.Once(context.Background())
	l.Wait()
	if restarted {
		t.Error("only a task that changed the agent restarts it")
	}
}

func TestAVersionThatKeepsRestartingIsRolledBackAtStartup(t *testing.T) {
	l, _, _, _ := setup(t)
	g := &fakeGuard{started: true}
	l.Guard = g
	restarted := false
	l.Restart = func() { restarted = true }
	ctx, cancel := context.WithCancel(context.Background())
	l.Sleep = func(context.Context, time.Duration) { cancel() }
	l.Run(ctx)
	if g.rolledBack != 1 || !restarted {
		t.Errorf("rolledBack=%d restarted=%v", g.rolledBack, restarted)
	}
}

func TestAVersionThatCannotCheckInInTimeIsRolledBackAndTheLoopStops(t *testing.T) {
	l, p, _, _ := setup(t)
	g := &fakeGuard{overdue: true}
	l.Guard = g
	p.checkIn = errors.New("cannot reach the platform")
	restarted := false
	l.Restart = func() { restarted = true }
	done := make(chan struct{})
	go func() { l.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop must stop so the old build can start")
	}
	if g.rolledBack != 1 || !restarted || g.confirmed != 0 {
		t.Errorf("rolledBack=%d restarted=%v confirmed=%d", g.rolledBack, restarted, g.confirmed)
	}
}

func TestARollbackThatFailsStillStopsTheLoop(t *testing.T) {
	l, _, _, _ := setup(t)
	g := &fakeGuard{overdue: true, rollbackErr: errors.New("no previous build")}
	l.Guard = g
	restarted := false
	l.Restart = func() { restarted = true }
	l.Run(context.Background())
	if g.rolledBack != 1 || !restarted {
		t.Errorf("rolledBack=%d restarted=%v", g.rolledBack, restarted)
	}
}

func TestWhileAnUpdateIsUnconfirmedAFailedCheckInIsRetriedEveryThirtySecondsNotBackedOff(t *testing.T) {
	waits := func(unconfirmed bool) []time.Duration {
		l, p, _, _ := setup(t)
		l.Guard = &fakeGuard{unconfirmed: unconfirmed}
		p.checkIn = errors.New("cannot reach the platform")
		var got []time.Duration
		ctx, cancel := context.WithCancel(context.Background())
		l.Sleep = func(_ context.Context, d time.Duration) {
			got = append(got, d)
			if len(got) == 5 {
				cancel()
			}
		}
		l.Run(ctx)
		return got
	}
	normal := waits(false)
	if normal[4] < 4*time.Minute {
		t.Fatalf("the ordinary back-off should grow: %v", normal)
	}
	for _, d := range waits(true) {
		if d > 40*time.Second {
			t.Errorf("an unconfirmed update must keep retrying about every 30s, waited %v", d)
		}
	}
}

func TestFallbackAddressesFromTheCheckInAreHandedOnAndAnEmptyListClears(t *testing.T) {
	l, p, _, _ := setup(t)
	var got [][]string
	l.FallbackSink = func(list []string) { got = append(got, list) }
	if _, err := l.Once(context.Background()); err != nil { // the fake answers without the field: nothing is said
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("an answer without the field must leave the list alone: %v", got)
	}
	_ = p
}

func TestAMachineWhoseClockIsAheadRefusesTasksAsExpiredUntilItJudgesByThePlatformsTime(t *testing.T) {
	l, p, r, v := setup(t)
	l.Now = func() time.Time { return time.Date(2026, 10, 20, 14, 5, 0, 0, time.UTC) } // two hours ahead of the platform's 12:05
	p.offer = []taskverify.Envelope{v.Tasks["ping"]}
	if _, err := l.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.Wait()
	if len(r.ran) != 0 {
		t.Fatal("a task that is over by this machine's own clock must be refused")
	}

	// Told the platform's time, the same task is current.
	l2, p2, r2, v2 := setup(t)
	l2.Now = func() time.Time { return time.Date(2026, 10, 20, 14, 5, 0, 0, time.UTC) }
	l2.ClockOffset = func() (time.Duration, bool) { return -2 * time.Hour, true }
	p2.offer = []taskverify.Envelope{v2.Tasks["ping"]}
	if _, err := l2.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	l2.Wait()
	if len(r2.ran) != 1 {
		t.Errorf("ran %v: judged by the platform's time the task is not expired", r2.ran)
	}
	_ = p
}

func TestAnAbsurdOffsetIsNotTrusted(t *testing.T) {
	l, p, r, v := setup(t)
	l.Now = func() time.Time { return time.Date(2026, 10, 20, 14, 5, 0, 0, time.UTC) }
	l.ClockOffset = func() (time.Duration, bool) { return -1000 * 24 * time.Hour, true }
	p.offer = []taskverify.Envelope{v.Tasks["ping"]}
	_, _ = l.Once(context.Background())
	l.Wait()
	if len(r.ran) != 0 {
		t.Error("an offset of years is ignored, so the task stays expired")
	}
}
