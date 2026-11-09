package runner

import (
	"context"
	"testing"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/servicelog"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
)

func TestServiceLogIsInTheCatalogValidatesAgainAndRunsNothingOnHostileParams(t *testing.T) {
	r := newRunner()
	found := false
	for _, n := range r.Names() {
		found = found || n == "service-log"
	}
	if !found {
		t.Error("service-log is not in the catalog")
	}
	called := false
	var argv []string
	r.JournalExec = func(_ context.Context, a []string) (string, string, error) {
		called = true
		argv = a
		return "2026-10-20T11:00:00+0000 h s[1]: hello\n", "", nil
	}
	for _, p := range []map[string]any{
		{"unit": "sshd", "since": "2026-10-20T10:00:00Z"},
		{"unit": "probe", "since": "2026-10-20T10:00:00Z", "contains": "a;b"},
		{"unit": "probe", "since": "2026-10-20T10:00:00Z", "extra": 1},
		{"unit": "probe", "since": "2026-10-10T10:00:00Z"},
	} {
		rep := r.Run(context.Background(), &taskverify.Task{Name: "service-log", TimeoutSeconds: 30, Params: p})
		if rep.Status != "failed" || called {
			t.Errorf("hostile params %v ran: %+v", p, rep)
		}
	}
	rep := r.Run(context.Background(), &taskverify.Task{Name: "service-log", TimeoutSeconds: 30, Params: map[string]any{"unit": "tls-front", "since": "2026-10-20T10:00:00Z"}})
	res, _ := rep.Result.(servicelog.Result)
	if rep.Status != "done" || len(res.Lines) != 1 || res.Unit != "nginx.service" || argv[6] != "nginx.service" || argv[8] != "2026-10-20 10:00:00" || argv[10] != "2026-10-20 12:00:00" {
		t.Errorf("%+v %v", rep, argv)
	}
	r.JournalExec = func(context.Context, []string) (string, string, error) {
		return "No journal files were found.\n", "", nil
	}
	rep = r.Run(context.Background(), &taskverify.Task{Name: "service-log", TimeoutSeconds: 30, Params: map[string]any{"unit": "probe", "since": "2026-10-20T10:00:00Z"}})
	res, _ = rep.Result.(servicelog.Result)
	if rep.Status != "done" || res.Note != "no-journal" || len(res.Lines) != 0 {
		t.Errorf("%+v", rep)
	}
}
