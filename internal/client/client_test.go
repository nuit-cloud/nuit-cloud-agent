package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRefusesAPlainHTTPAddressUnlessToldTo(t *testing.T) {
	if _, err := New("http://platform.example", "1", false); err == nil {
		t.Error("plain http must be refused by default")
	}
	if _, err := New("http://platform.example", "1", true); err != nil {
		t.Errorf("explicitly allowed: %v", err)
	}
	if _, err := New("https://platform.example/", "1", false); err != nil {
		t.Errorf("https: %v", err)
	}
	for _, bad := range []string{"", "not a url", "ftp://x", "https://"} {
		if _, err := New(bad, "1", true); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestEnrollSendsTheCodeAndNoCredentialAndReadsTheAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/enroll" || r.Method != "POST" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("enrolling must not send a credential")
		}
		if r.Header.Get("X-Agent-Version") != "1.2.3" {
			t.Errorf("version header %q", r.Header.Get("X-Agent-Version"))
		}
		var body map[string]string
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		if body["code"] != "nce_abc" || body["hostname"] != "box" {
			t.Errorf("body %v", body)
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"machineId":"m1","name":"box","credential":"nca_x","pollIntervalSeconds":30,"taskSigning":{"keys":[{"keyId":"k","publicKey":"p"}]}}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "1.2.3", true)
	got, err := c.Enroll(context.Background(), "nce_abc", "box")
	if err != nil {
		t.Fatal(err)
	}
	if got.MachineID != "m1" || got.Credential != "nca_x" || len(got.TaskSigning.Keys) != 1 || got.TaskSigning.Keys[0].KeyID != "k" {
		t.Errorf("%+v", got)
	}
}

func TestRoutineCallsSendTheCredentialAndANon2xxIsAStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer nca_secret" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"That is not a code or credential this platform recognises"}`))
			return
		}
		_, _ = w.Write([]byte(`{"machineId":"m1","name":"box","pollIntervalSeconds":45,"tasks":[{"keyId":"k","payload":"p","signature":"s"}]}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "1", true)
	c.Credential = "nca_wrong"
	_, err := c.CheckIn(context.Background())
	se, ok := err.(*StatusError)
	if !ok || !se.Unauthorized() || se.Message == "" {
		t.Fatalf("got %v", err)
	}
	c.Credential = "nca_secret"
	got, err := c.CheckIn(context.Background())
	if err != nil || got.PollIntervalSeconds != 45 || len(got.Tasks) != 1 || got.Tasks[0].Payload != "p" {
		t.Errorf("%+v %v", got, err)
	}
}

func TestAConflictOnReportingIsNotAnError(t *testing.T) {
	status := 409
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/tasks/t%2F1/result" && r.URL.EscapedPath() != "/api/agent/tasks/t%2F1/result" {
			t.Errorf("path %q", r.URL.EscapedPath())
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"That task has already finished"}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "1", true)
	c.Credential = "nca_x"
	if err := c.ReportTask(context.Background(), "t/1", Report{Status: "done"}); err != nil {
		t.Errorf("409 means there is nothing more to do: %v", err)
	}
	status = 404
	if err := c.ReportTask(context.Background(), "t/1", Report{Status: "done"}); err == nil {
		t.Error("a 404 is an error")
	}
}

func TestRotateReturnsTheNewCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"credential":"nca_fresh"}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "1", true)
	c.Credential = "nca_old"
	got, err := c.RotateCredential(context.Background())
	if err != nil || got != "nca_fresh" {
		t.Errorf("%q %v", got, err)
	}
}

type fakeObserver struct {
	collected []string
	version   string
	fail      bool
}

func (f *fakeObserver) Known(kind string) bool { return kind == "network" }
func (f *fakeObserver) Collect(_ context.Context, kind string) (any, string, error) {
	f.collected = append(f.collected, kind)
	if f.fail {
		return nil, "", io.ErrUnexpectedEOF
	}
	return map[string]any{"interfaces": []any{}, "neighbours": []any{}}, f.version, nil
}

// obsPlatform is a stand-in that holds the version it was last sent, and says so in its answers, as the real one does.
type obsPlatform struct {
	t      *testing.T
	have   string
	bodies []map[string]any
	extra  string
}

func (p *obsPlatform) handler(w http.ResponseWriter, r *http.Request) {
	var b map[string]any
	_ = json.NewDecoder(r.Body).Decode(&b)
	p.bodies = append(p.bodies, b)
	if list, ok := b["observations"].([]any); ok {
		for _, o := range list {
			p.have, _ = o.(map[string]any)["version"].(string)
		}
	}
	have := ""
	if p.have != "" {
		have = `,"have":"` + p.have + `"`
	}
	_, _ = io.WriteString(w, `{"pollIntervalSeconds":30,"observe":[{"kind":"network","intervalSeconds":300`+have+`},{"kind":"cpu","intervalSeconds":60}]}`)
}

func (p *obsPlatform) sentOn(i int) bool { return p.bodies[i]["observations"] != nil }

func TestAnUnchangedFactIsSentOnceAndThenNothingAtAllUntilItChanges(t *testing.T) {
	p := &obsPlatform{t: t}
	srv := httptest.NewServer(http.HandlerFunc(p.handler))
	defer srv.Close()
	c, _ := New(srv.URL, "1", true)
	obs := &fakeObserver{version: "v1"}
	c.Observer = obs
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return now }
	ctx := context.Background()
	call := func(advance time.Duration) {
		now = now.Add(advance)
		if _, err := c.CheckIn(ctx); err != nil {
			t.Fatal(err)
		}
	}

	call(0) // nothing is asked for yet: nothing is read or sent
	if p.sentOn(0) || len(obs.collected) != 0 {
		t.Fatalf("sent before being asked: %v / %v", p.bodies[0], obs.collected)
	}
	call(30 * time.Second) // asked for, obsPlatform holds none: sent, with its version, and only the kind this agent knows
	sent, _ := p.bodies[1]["observations"].([]any)
	if len(sent) != 1 || sent[0].(map[string]any)["version"] != "v1" || sent[0].(map[string]any)["kind"] != "network" {
		t.Fatalf("%v", p.bodies[1])
	}
	for i := 0; i < 6; i++ { // every interval it is read again, and found unchanged: no traffic of this kind
		call(5 * time.Minute)
		if p.sentOn(len(p.bodies) - 1) {
			t.Fatalf("an unchanged fact was sent again: %v", p.bodies[len(p.bodies)-1])
		}
	}
	if len(obs.collected) != 7 {
		t.Errorf("read %d times", len(obs.collected))
	}
	obs.version = "v2" // it changes: sent once more
	call(5 * time.Minute)
	if !p.sentOn(len(p.bodies) - 1) {
		t.Error("a changed fact must be sent")
	}
	call(5 * time.Minute)
	if p.sentOn(len(p.bodies) - 1) {
		t.Error("and then not again")
	}
}

func TestBetweenIntervalsNothingIsReadAtAll(t *testing.T) {
	p := &obsPlatform{t: t}
	srv := httptest.NewServer(http.HandlerFunc(p.handler))
	defer srv.Close()
	c, _ := New(srv.URL, "1", true)
	obs := &fakeObserver{version: "v1"}
	c.Observer = obs
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return now }
	for i := 0; i < 11; i++ { // ten check-ins in five minutes
		_, _ = c.CheckIn(context.Background())
		now = now.Add(30 * time.Second)
	}
	if len(obs.collected) != 1 {
		t.Errorf("read %d times in one interval", len(obs.collected))
	}
}

func TestWhenThePlatformDoesNotHoldWhatWasSentItIsTriedAgainOncePerIntervalNotEveryCheckIn(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		_, _ = io.WriteString(w, `{"observe":[{"kind":"network","intervalSeconds":300,"have":"other"}]}`) // always holds something else
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "1", true)
	c.Observer = &fakeObserver{version: "v1"}
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return now }
	sends := 0
	for i := 0; i < 21; i++ { // ten minutes of 30-second check-ins
		_, _ = c.CheckIn(context.Background())
		now = now.Add(30 * time.Second)
	}
	for _, b := range bodies {
		if b["observations"] != nil {
			sends++
		}
	}
	if sends != 2 {
		t.Errorf("sent %d times in ten minutes", sends)
	}
}

func TestAFailedReadIsSkippedAndTheCheckInStillHappens(t *testing.T) {
	p := &obsPlatform{t: t}
	srv := httptest.NewServer(http.HandlerFunc(p.handler))
	defer srv.Close()
	c, _ := New(srv.URL, "1", true)
	c.Observer = &fakeObserver{fail: true}
	for i := 0; i < 3; i++ {
		if _, err := c.CheckIn(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if p.sentOn(2) {
		t.Errorf("sent %v", p.bodies[2])
	}
}

func TestAnAnswerWithoutObserveChangesNothingAndAnEmptyListAsksForNone(t *testing.T) {
	answers := []string{`{"observe":[{"kind":"network","intervalSeconds":300}]}`, `{"pollIntervalSeconds":30}`, `{"observe":[]}`}
	var n int
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		_, _ = io.WriteString(w, answers[min(n, len(answers)-1)])
		n++
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "1", true)
	obs := &fakeObserver{}
	c.Observer = obs
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { now = now.Add(10 * time.Minute); return now }
	for i := 0; i < 4; i++ {
		obs.version = string(rune('a' + i)) // changes every time, so only the request decides
		if _, err := c.CheckIn(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if bodies[0]["observations"] != nil || bodies[1]["observations"] == nil {
		t.Errorf("asked for after the first answer: %v %v", bodies[0], bodies[1])
	}
	if bodies[2]["observations"] == nil { // an answer that says nothing about it leaves the request standing
		t.Error("an answer with no observe field must leave the request unchanged")
	}
	if bodies[3]["observations"] != nil { // the empty list withdrew it
		t.Error("an empty list asks for none")
	}
}

func TestTheClientMeasuresTheOffsetFromThePlatformsTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"pollIntervalSeconds":30,"serverTime":"2026-11-03T12:00:00Z"}`)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "1", true)
	if _, ok := c.ClockOffset(); ok {
		t.Error("nothing is known before the first answer")
	}
	local := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC) // this machine says two hours later
	c.Now = func() time.Time { return local }
	if _, err := c.CheckIn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d, ok := c.ClockOffset(); !ok || d != -2*time.Hour {
		t.Errorf("offset %v %v", d, ok)
	}
}

type everyKind struct{}

func (everyKind) Known(string) bool { return true }
func (everyKind) Collect(_ context.Context, kind string) (any, string, error) {
	return map[string]any{}, "v-" + kind, nil
}

// The platform asks for five kinds (network, clock, runtime, dns, task-keys): the agent must not drop the last one it is asked for.
func TestEveryKindThePlatformAsksForIsSent(t *testing.T) {
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Observations []Observation `json:"observations"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		for _, o := range b.Observations {
			sent = append(sent, o.Kind)
		}
		_, _ = io.WriteString(w, `{"observe":[{"kind":"network","intervalSeconds":300},{"kind":"clock","intervalSeconds":300},{"kind":"runtime","intervalSeconds":300},{"kind":"dns","intervalSeconds":300},{"kind":"task-keys","intervalSeconds":300}]}`)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "1", true)
	c.Observer = everyKind{}
	for i := 0; i < 2; i++ {
		if _, err := c.CheckIn(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(sent) != 5 || sent[4] != "task-keys" {
		t.Errorf("sent %v", sent)
	}
}
