package server_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/goccy/bigquery-emulator/server"
)

func TestRequestTimingLogs(t *testing.T) {
	ctx := context.Background()
	const project = "test"

	bqServer, err := server.New(server.MemoryStorage)
	if err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.DebugLevel)
	bqServer.SetLogger(zap.New(core))
	if err := bqServer.SetProject(project); err != nil {
		t.Fatal(err)
	}
	testServer := bqServer.TestServer()
	defer func() {
		testServer.Close()
		bqServer.Stop(ctx)
	}()

	url := fmt.Sprintf("%s/bigquery/v2/projects/%s/queries", testServer.URL, project)
	res, err := http.Post(url, "application/json", bytes.NewReader([]byte(`{"query":"SELECT 1 AS x"}`)))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status %d", res.StatusCode)
	}

	var found bool
	for _, e := range logs.FilterMessage("POST /bigquery/v2/projects/test/queries").All() {
		f := e.ContextMap()
		found = true
		if f["status"] != int64(200) {
			t.Errorf("status = %v, want 200", f["status"])
		}
		for _, k := range []string{"duration_ms", "lock_wait_ms", "handler_ms"} {
			v, ok := f[k].(float64)
			if !ok || v < 0 {
				t.Errorf("%s = %v, want non-negative float", k, f[k])
			}
		}
		if b, _ := f["bytes"].(int64); b <= 0 {
			t.Errorf("bytes = %v, want > 0", f["bytes"])
		}
		if f["method"] != "POST" {
			t.Errorf("method = %v", f["method"])
		}
	}
	if !found {
		t.Fatalf("no access log line; got %v", logs.All())
	}

	phases := logs.FilterMessage("jobs.query phases").All()
	if len(phases) != 1 {
		t.Fatalf("want 1 phase line, got %d", len(phases))
	}
	pf := phases[0].ContextMap()
	if !strings.HasPrefix(fmt.Sprint(pf["sql"]), "SELECT 1") || pf["job_id"] == "" {
		t.Errorf("phase fields: %v", pf)
	}
	for _, k := range []string{"prepare_ms", "execute_ms", "job_write_ms", "commit_ms", "format_ms"} {
		if _, ok := pf[k].(float64); !ok {
			t.Errorf("missing phase %s in %v", k, pf)
		}
	}
}
