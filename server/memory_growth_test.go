package server_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/goccy/bigquery-emulator/server"
	"github.com/goccy/bigquery-emulator/types"
	"github.com/goccy/go-json"
	bigqueryv2 "google.golang.org/api/bigquery/v2"
)

// TestQueryMemoryStaysBounded serves a few thousand varied jobs.query
// requests and checks that the heap stops growing. Every statement the
// driver parsed used to leave its AST in a long-lived wasm arena, so the
// process grew by about 145 MiB per 1,000 queries (flipto-dbt: 27 MiB
// fresh, 895 MiB after 6,136 queries on one container).
func TestQueryMemoryStaysBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("long-running memory test")
	}
	ctx := context.Background()
	const projectID = "memory"
	bqServer, err := server.New(server.TempStorage)
	if err != nil {
		t.Fatal(err)
	}
	if err := bqServer.Load(server.StructSource(types.NewProject(projectID, types.NewDataset("ds")))); err != nil {
		t.Fatal(err)
	}
	testServer := bqServer.TestServer()
	defer func() {
		testServer.Close()
		bqServer.Stop(ctx)
	}()

	url := testServer.URL + "/bigquery/v2/projects/" + projectID + "/queries"
	query := func(sql string) *bigqueryv2.QueryResponse {
		body, _ := json.Marshal(map[string]any{"query": sql, "useLegacySql": false})
		resp, err := http.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var res bigqueryv2.QueryResponse
		_ = json.Unmarshal(data, &res)
		return &res
	}
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	var lastJobID string
	run := func(from, to int) {
		for i := from; i < to; i++ {
			switch i % 5 {
			case 0:
				if res := query(fmt.Sprintf("SELECT %d AS x, 'v%d' AS s", i, i)); res.JobReference != nil {
					lastJobID = res.JobReference.JobId
				}
			case 1:
				query(fmt.Sprintf("SELECT no_such_column_%d", i))
			case 2:
				query(fmt.Sprintf("CREATE TEMP TABLE t%d AS SELECT %d AS a; SELECT * FROM t%d", i, i, i))
			case 3:
				query(fmt.Sprintf("SELECT ARRAY(SELECT x FROM UNNEST(GENERATE_ARRAY(1, %d)) x)", i%50+1))
			case 4:
				query(fmt.Sprintf("SELECT CURRENT_DATE() > DATE '2000-01-01', %d", i))
			}
		}
	}
	const warmup, total = 500, 3500
	start := time.Now()
	run(0, warmup)
	before := heap()
	run(warmup, total)
	after := heap()
	grown := int64(after) - int64(before)
	t.Logf("heap after %d queries: %d MiB; after %d: %d MiB (%v)", warmup, before>>20, total, after>>20, time.Since(start))
	// The leak cost ~145 MiB per 1,000 queries (over 400 MiB here).
	if grown > 64<<20 {
		t.Fatalf("heap grew by %d MiB over %d queries", grown>>20, total-warmup)
	}

	// The most recent job is still served by jobs.get and getQueryResults.
	if lastJobID == "" {
		t.Fatal("no job reference returned")
	}
	for _, path := range []string{"/jobs/" + lastJobID, "/queries/" + lastJobID} {
		resp, err := http.Get(testServer.URL + "/bigquery/v2/projects/" + projectID + path)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d: %s", path, resp.StatusCode, data)
		}
		if path[1] == 'q' {
			var res bigqueryv2.GetQueryResultsResponse
			if err := json.Unmarshal(data, &res); err != nil {
				t.Fatal(err)
			}
			if len(res.Rows) != 1 {
				t.Fatalf("getQueryResults %s: %d rows, want 1: %s", lastJobID, len(res.Rows), data)
			}
		}
	}
}
