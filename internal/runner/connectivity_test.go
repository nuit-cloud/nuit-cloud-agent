package runner

import (
	"context"
	"testing"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/connectivity"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
)

func TestConnectivityCheckIsInTheCatalogValidatesAgainAndReportsResultsInOrder(t *testing.T) {
	r := newRunner()
	found := false
	for _, n := range r.Names() {
		found = found || n == "connectivity-check"
	}
	if !found {
		t.Error("connectivity-check is not in the catalog")
	}
	called := false
	r.Connectivity = func(_ context.Context, ts []connectivity.Target, version string) []connectivity.Result {
		called = true
		if version != "9.9.9" {
			t.Errorf("version %q", version)
		}
		out := []connectivity.Result{}
		for i, x := range ts {
			out = append(out, connectivity.Result{ID: x.ID, OK: i == 0, Detail: "connected"})
		}
		return out
	}
	hostile := []map[string]any{
		{"targets": []any{map[string]any{"id": "a", "kind": "tcp", "host": "127.0.0.1", "port": 22}}},
		{"targets": []any{map[string]any{"id": "a", "kind": "dns", "host": "example.com", "cmd": "x"}}},
		{"targets": []any{}, "extra": true},
	}
	for _, p := range hostile {
		rep := r.Run(context.Background(), &taskverify.Task{Name: "connectivity-check", TimeoutSeconds: 30, Params: p})
		if rep.Status != "failed" || called {
			t.Errorf("hostile params %v ran: %+v", p, rep)
		}
	}
	rep := r.Run(context.Background(), &taskverify.Task{Name: "connectivity-check", TimeoutSeconds: 30, Params: map[string]any{"targets": []any{
		map[string]any{"id": "a", "kind": "tcp", "host": "192.168.1.1", "port": float64(22)},
		map[string]any{"id": "b", "kind": "dns", "host": "example.com"},
	}}})
	res, _ := rep.Result.(map[string]any)["results"].([]connectivity.Result)
	if rep.Status != "done" || len(res) != 2 || res[0].ID != "a" || res[1].ID != "b" || !res[0].OK || res[1].OK {
		t.Errorf("%+v", rep)
	}
}
