package server_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/goccy/bigquery-emulator/server"
	"github.com/goccy/bigquery-emulator/types"
	"github.com/goccy/go-json"
	bigqueryv2 "google.golang.org/api/bigquery/v2"
)

// TestCurrentTimeStableThroughJobsQuery checks through jobs.query that all
// CURRENT_* calls in one statement see one instant, as on BigQuery ("the
// current time is the same for all calls within a statement"); the
// flipto-dbt harness probe current_ts_stable returned false here.
func TestCurrentTimeStableThroughJobsQuery(t *testing.T) {
	ctx := context.Background()
	const projectID = "curtime"
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
	queries := []string{
		`SELECT CURRENT_TIMESTAMP() = CURRENT_TIMESTAMP() AS c0`,
		`SELECT CURRENT_DATETIME() = DATETIME(CURRENT_TIMESTAMP()) AS c0`,
		`SELECT CURRENT_DATE() = DATE(CURRENT_TIMESTAMP()) AS c0`,
		`SELECT CURRENT_TIME() = TIME(CURRENT_TIMESTAMP()) AS c0`,
		`SELECT LOGICAL_AND(CURRENT_TIMESTAMP() = CURRENT_TIMESTAMP()) AS c0 FROM UNNEST(GENERATE_ARRAY(1, 5000))`,
		`SELECT COUNT(DISTINCT CURRENT_TIMESTAMP()) = 1 AS c0 FROM UNNEST(GENERATE_ARRAY(1, 5000))`,
	}
	for _, q := range queries {
		for i := 0; i < 20; i++ {
			body, _ := json.Marshal(map[string]any{"query": q, "useLegacySql": false})
			resp, err := http.Post(url, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var res bigqueryv2.QueryResponse
			if err := json.Unmarshal(data, &res); err != nil {
				t.Fatalf("%s: %v: %s", q, err, data)
			}
			if len(res.Rows) != 1 || len(res.Rows[0].F) != 1 || res.Rows[0].F[0].V != "true" {
				t.Fatalf("%s (run %d): got %s, want true", q, i, data)
			}
		}
	}
}
