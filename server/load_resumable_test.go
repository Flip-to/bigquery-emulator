package server_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/goccy/bigquery-emulator/server"
	"github.com/goccy/bigquery-emulator/types"
	"github.com/google/go-cmp/cmp"
)

// resumableLoad runs a load job the way the Python client does: POST the job
// metadata with uploadType=resumable, then PUT the payload to the session.
// It returns the final job resource from jobs.get.
func resumableLoad(t *testing.T, baseURL, jobID, loadJSON, payload string) map[string]any {
	t.Helper()
	jobJSON := fmt.Sprintf(`{"jobReference":{"projectId":"test","jobId":%q},"configuration":{"load":%s}}`, jobID, loadJSON)
	code, resp := httpJSON(t, http.MethodPost,
		baseURL+"/upload/bigquery/v2/projects/test/jobs?uploadType=resumable", jobJSON, nil)
	if code != http.StatusOK {
		t.Fatalf("start resumable upload: %d (%v)", code, resp)
	}
	code, resp = httpJSON(t, http.MethodPut,
		baseURL+"/upload/bigquery/v2/projects/test/jobs?uploadType=resumable&upload_id="+jobID, payload,
		map[string]string{"Content-Type": "application/octet-stream"})
	if code != http.StatusOK {
		t.Fatalf("upload payload: expected 200, got %d (%v)", code, resp)
	}
	code, resp = httpJSON(t, http.MethodGet, baseURL+"/projects/test/jobs/"+jobID, "", nil)
	if code != http.StatusOK {
		t.Fatalf("jobs.get: %d (%v)", code, resp)
	}
	return resp
}

func jobErrorResult(job map[string]any) any {
	status, _ := job["status"].(map[string]any)
	return status["errorResult"]
}

func newLoadTestServer(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	bqServer, err := server.New(server.TempStorage)
	if err != nil {
		t.Fatal(err)
	}
	if err := bqServer.Load(server.StructSource(types.NewProject("test", types.NewDataset("ds")))); err != nil {
		t.Fatal(err)
	}
	ts := bqServer.TestServer()
	t.Cleanup(func() {
		ts.Close()
		bqServer.Stop(ctx)
	})
	return ts.URL
}

func queryAll(t *testing.T, baseURL, sql string) [][]string {
	t.Helper()
	code, resp := httpJSON(t, http.MethodPost, baseURL+"/projects/test/queries",
		fmt.Sprintf(`{"query":%q,"useLegacySql":false}`, sql), nil)
	if code != http.StatusOK {
		t.Fatalf("query %s: %d (%v)", sql, code, resp)
	}
	return queryRows(t, resp)
}

func tableFieldTypes(t *testing.T, baseURL, table string) [][]string {
	t.Helper()
	code, resp := httpJSON(t, http.MethodGet, baseURL+"/projects/test/datasets/ds/tables/"+table, "", nil)
	if code != http.StatusOK {
		t.Fatalf("tables.get: %d (%v)", code, resp)
	}
	schema, _ := resp["schema"].(map[string]any)
	fields, _ := schema["fields"].([]any)
	var out [][]string
	for _, f := range fields {
		m := f.(map[string]any)
		out = append(out, []string{m["name"].(string), m["type"].(string)})
	}
	return out
}

const seedCSV = "name,n,ok,score,day\nalpha,1,true,1.5,2024-01-02\n\"b,\"\"q\"\"\",2,false,,2024-02-03\n"

func TestResumableLoadCSVTypeNames(t *testing.T) {
	baseURL := newLoadTestServer(t)
	for _, tc := range []struct {
		name   string
		fields string
	}{
		// dbt-bigquery seeds: lowercase standard names, no sourceFormat.
		{"lower_standard", `{"name":"name","type":"string"},{"name":"n","type":"int64"},{"name":"ok","type":"bool"},{"name":"score","type":"float64"},{"name":"day","type":"date"}`},
		// Python client SchemaField defaults: legacy names with a mode.
		{"legacy", `{"name":"name","type":"STRING","mode":"NULLABLE"},{"name":"n","type":"INTEGER","mode":"NULLABLE"},{"name":"ok","type":"BOOLEAN","mode":"nullable"},{"name":"score","type":"FLOAT","mode":"NULLABLE"},{"name":"day","type":"DATE"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			load := fmt.Sprintf(`{"skipLeadingRows":"1","fieldDelimiter":",","schema":{"fields":[%s]},`+
				`"destinationTable":{"projectId":"test","datasetId":"ds","tableId":%q}}`, tc.fields, tc.name)
			job := resumableLoad(t, baseURL, "job_"+tc.name, load, seedCSV)
			if e := jobErrorResult(job); e != nil {
				t.Fatalf("unexpected job error: %v", e)
			}
			got := queryAll(t, baseURL, "SELECT name, n + 1, ok, score, EXTRACT(YEAR FROM day) FROM ds."+tc.name+" ORDER BY n")
			want := [][]string{{"alpha", "2", "true", "1.5", "2024"}, {`b,"q"`, "3", "false", "<nil>", "2024"}}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("rows mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResumableLoadCSVAutodetect(t *testing.T) {
	baseURL := newLoadTestServer(t)
	load := `{"sourceFormat":"CSV","skipLeadingRows":"1","autodetect":true,` +
		`"destinationTable":{"projectId":"test","datasetId":"ds","tableId":"auto"}}`
	job := resumableLoad(t, baseURL, "job_auto", load, seedCSV)
	if e := jobErrorResult(job); e != nil {
		t.Fatalf("unexpected job error: %v", e)
	}
	wantTypes := [][]string{{"name", "STRING"}, {"n", "INTEGER"}, {"ok", "BOOLEAN"}, {"score", "FLOAT"}, {"day", "DATE"}}
	if diff := cmp.Diff(wantTypes, tableFieldTypes(t, baseURL, "auto")); diff != "" {
		t.Fatalf("schema mismatch (-want +got):\n%s", diff)
	}
}

func TestResumableLoadCSVWriteTruncateRerun(t *testing.T) {
	baseURL := newLoadTestServer(t)
	load := `{"skipLeadingRows":"1","writeDisposition":"WRITE_TRUNCATE","schema":{"fields":[` +
		`{"name":"name","type":"string"},{"name":"n","type":"int64"}]},` +
		`"destinationTable":{"projectId":"test","datasetId":"ds","tableId":"trunc"}}`
	resumableLoad(t, baseURL, "job_trunc1", load, "name,n\na,1\nb,2\n")
	job := resumableLoad(t, baseURL, "job_trunc2", load, "name,n\nc,3\n")
	if e := jobErrorResult(job); e != nil {
		t.Fatalf("unexpected job error: %v", e)
	}
	if diff := cmp.Diff([][]string{{"c", "3"}}, queryAll(t, baseURL, "SELECT name, n FROM ds.trunc")); diff != "" {
		t.Fatalf("rows mismatch (-want +got):\n%s", diff)
	}
}

func TestResumableLoadCSVNullMarkerAndQuotes(t *testing.T) {
	baseURL := newLoadTestServer(t)
	load := `{"skipLeadingRows":"1","nullMarker":"NA","quote":"'","allowQuotedNewlines":true,"fieldDelimiter":"|",` +
		`"schema":{"fields":[{"name":"s","type":"STRING"},{"name":"n","type":"INTEGER"}]},` +
		`"destinationTable":{"projectId":"test","datasetId":"ds","tableId":"nulls"}}`
	payload := "s|n\n'multi\nline|x'|1\nNA|NA\n|3\n'it''s'|4\n"
	job := resumableLoad(t, baseURL, "job_nulls", load, payload)
	if e := jobErrorResult(job); e != nil {
		t.Fatalf("unexpected job error: %v", e)
	}
	got := queryAll(t, baseURL, "SELECT s, n FROM ds.nulls ORDER BY n NULLS FIRST")
	want := [][]string{{"<nil>", "<nil>"}, {"multi\nline|x", "1"}, {"", "3"}, {"it's", "4"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("rows mismatch (-want +got):\n%s", diff)
	}
}

func TestResumableLoadCSVBadDataIsJobError(t *testing.T) {
	baseURL := newLoadTestServer(t)
	load := `{"skipLeadingRows":"1","schema":{"fields":[{"name":"n","type":"INT64"}]},` +
		`"destinationTable":{"projectId":"test","datasetId":"ds","tableId":"bad"}}`
	job := resumableLoad(t, baseURL, "job_bad", load, "n\n1,2,3\n")
	if jobErrorResult(job) == nil {
		t.Fatalf("expected status.errorResult on the job, got %v", job["status"])
	}
}
