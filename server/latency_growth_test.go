package server_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/goccy/bigquery-emulator/server"
	"github.com/goccy/bigquery-emulator/types"
	"github.com/goccy/go-json"
	bigqueryv2 "google.golang.org/api/bigquery/v2"
)

// TestQueryLatencyStaysFlat serves a few thousand varied jobs.query requests
// and checks that SELECT 1 does not get slower as the job history grows.
// Every request used to load the project's whole job history (metadata and
// results), so per-request cost grew linearly with the jobs served
// (flipto-dbt L12: SELECT 1 went from 0.25 s to 6 s after ~1,750 queries).
func TestQueryLatencyStaysFlat(t *testing.T) {
	if testing.Short() {
		t.Skip("long-running latency test")
	}
	ctx := context.Background()
	const projectID = "latency"
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
	query := func(sql string) (time.Duration, *bigqueryv2.QueryResponse) {
		body, _ := json.Marshal(map[string]any{"query": sql, "useLegacySql": false})
		start := time.Now()
		resp, err := http.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		elapsed := time.Since(start)
		var res bigqueryv2.QueryResponse
		_ = json.Unmarshal(data, &res)
		return elapsed, &res
	}
	probe := func() time.Duration {
		d, _ := query("SELECT 1")
		return d
	}
	median := func(ds []time.Duration) time.Duration {
		s := append([]time.Duration(nil), ds...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		return s[len(s)/2]
	}

	const (
		total  = 2500
		window = 15
	)
	var first, last []time.Duration
	var lastJobID string
	deadline := time.Now().Add(80 * time.Second)
	for i := 0; i < total; i++ {
		switch i % 5 {
		case 0:
			_, res := query(fmt.Sprintf("SELECT %d AS x, 'v%d' AS s", i, i))
			if res.JobReference != nil {
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
		if i == 50 {
			for k := 0; k < window; k++ {
				first = append(first, probe())
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d queries served in 80s; latency grows with the job history", i)
		}
	}
	for k := 0; k < window; k++ {
		last = append(last, probe())
	}
	firstMedian, lastMedian := median(first), median(last)
	t.Logf("SELECT 1 median: first window %v, last window %v", firstMedian, lastMedian)
	if lastMedian > 4*firstMedian+50*time.Millisecond {
		t.Fatalf("SELECT 1 latency grew from %v to %v over %d queries", firstMedian, lastMedian, total)
	}

	// Recent jobs must still be visible through jobs.get and jobs.list.
	if lastJobID == "" {
		t.Fatal("no job reference returned")
	}
	resp, err := http.Get(testServer.URL + "/bigquery/v2/projects/" + projectID + "/jobs/" + lastJobID)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("jobs.get %s: status %d", lastJobID, resp.StatusCode)
	}
	resp, err = http.Get(testServer.URL + "/bigquery/v2/projects/" + projectID + "/jobs")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var list bigqueryv2.JobList
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, j := range list.Jobs {
		if j.JobReference != nil && j.JobReference.JobId == lastJobID {
			found = true
		}
	}
	if !found {
		t.Fatalf("jobs.list (%d jobs) does not contain %s", len(list.Jobs), lastJobID)
	}
}
