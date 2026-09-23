package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/goccy/bigquery-emulator/server"
)

// TestNestedWireFormat checks the raw jobs.query response for values
// nested in STRUCT and ARRAY, where clients (e.g. the Python client)
// parse the REST wire format directly:
//   - anonymous STRUCT fields are named _field_N (1-based position);
//   - nested TIMESTAMP uses the epoch-seconds encoding of top-level columns;
//   - a NULL array is returned as [] (BigQuery results never hold NULL arrays).
func TestNestedWireFormat(t *testing.T) {
	ctx := context.Background()
	bqServer, err := server.New(server.TempStorage)
	if err != nil {
		t.Fatal(err)
	}
	if err := bqServer.SetProject("test"); err != nil {
		t.Fatal(err)
	}
	ts := bqServer.TestServer()
	defer func() {
		ts.Close()
		bqServer.Stop(ctx)
	}()

	query := func(t *testing.T, q string) (schema, rows string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"query": q, "useLegacySql": false})
		resp, err := http.Post(ts.URL+"/bigquery/v2/projects/test/queries", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m struct {
			Schema json.RawMessage `json:"schema"`
			Rows   json.RawMessage `json:"rows"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		return string(m.Schema), string(m.Rows)
	}

	for _, tc := range []struct {
		name, sql, schema, rows string
	}{
		{
			name:   "anonymous struct fields",
			sql:    "SELECT ARRAY(SELECT AS STRUCT 1, 2)",
			schema: `{"fields":[{"fields":[{"mode":"NULLABLE","name":"_field_1","type":"INTEGER"},{"mode":"NULLABLE","name":"_field_2","type":"INTEGER"}],"mode":"REPEATED","name":"f0_","type":"RECORD"}]}`,
			rows:   `[{"f":[{"v":[{"v":{"f":[{"v":"1"},{"v":"2"}]}}]}]}]`,
		},
		{
			name:   "nested anonymous struct fields",
			sql:    "SELECT STRUCT(1, STRUCT(2 AS a, 3), 'x' AS b)",
			schema: `{"fields":[{"fields":[{"mode":"NULLABLE","name":"_field_1","type":"INTEGER"},{"fields":[{"mode":"NULLABLE","name":"a","type":"INTEGER"},{"mode":"NULLABLE","name":"_field_2","type":"INTEGER"}],"mode":"NULLABLE","name":"_field_2","type":"RECORD"},{"mode":"NULLABLE","name":"b","type":"STRING"}],"mode":"NULLABLE","name":"f0_","type":"RECORD"}]}`,
			rows:   `[{"f":[{"v":{"f":[{"v":"1"},{"v":{"f":[{"v":"2"},{"v":"3"}]}},{"v":"x"}]}}]}]`,
		},
		{
			name: "nested timestamp",
			sql:  "SELECT STRUCT(TIMESTAMP '2024-02-29 00:00:00+00' AS v), TIMESTAMP '2024-02-29 00:00:00+00', [TIMESTAMP '2024-02-29 00:00:00.5+00']",
			rows: `[{"f":[{"v":{"f":[{"v":"1709164800.000000"}]}},{"v":"1709164800.000000"},{"v":[{"v":"1709164800.500000"}]}]}]`,
		},
		{
			name: "null array",
			sql:  "SELECT STRUCT(SPLIT('a', CAST(NULL AS STRING)) AS v), SPLIT('a', CAST(NULL AS STRING))",
			rows: `[{"f":[{"v":{"f":[{"v":[]}]}},{"v":[]}]}]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema, rows := query(t, tc.sql)
			if tc.schema != "" && schema != tc.schema {
				t.Errorf("schema:\n got %s\nwant %s", schema, tc.schema)
			}
			if rows != tc.rows {
				t.Errorf("rows:\n got %s\nwant %s", rows, tc.rows)
			}
		})
	}
}
