package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// slowQuery runs for a noticeable time in the emulator; see
// TestLongQueryMeasure for its duration.
const slowQuery = "SELECT COUNT(*) AS n FROM UNNEST(GENERATE_ARRAY(1, 300000)) AS a CROSS JOIN UNNEST(GENERATE_ARRAY(1, 5)) AS b"

type asyncTestEnv struct {
	t    *testing.T
	base string
}

// The in-memory database is shared by the servers of a test binary: give
// each test its own job IDs.
func newAsyncTestEnv(t *testing.T) *asyncTestEnv {
	t.Helper()
	const project = "test"
	bqServer, err := New(MemoryStorage)
	if err != nil {
		t.Fatal(err)
	}
	if err := bqServer.SetProject(project); err != nil {
		t.Fatal(err)
	}
	testServer := bqServer.TestServer()
	t.Cleanup(func() {
		testServer.Close()
		_ = bqServer.Stop(context.Background())
	})
	return &asyncTestEnv{t: t, base: fmt.Sprintf("%s/bigquery/v2/projects/%s", testServer.URL, project)}
}

type apiResult struct {
	status int
	body   map[string]any
}

func (e *asyncTestEnv) do(method, path string, body any) apiResult {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.base+path, reader)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(raw, &out)
	return apiResult{status: res.StatusCode, body: out}
}

func (e *asyncTestEnv) insertQuery(jobID, query string) apiResult {
	e.t.Helper()
	return e.do(http.MethodPost, "/jobs", map[string]any{
		"jobReference":  map[string]any{"projectId": "test", "jobId": jobID},
		"configuration": map[string]any{"query": map[string]any{"query": query, "useLegacySql": false}},
	})
}

func jobState(r apiResult) string {
	st, _ := r.body["status"].(map[string]any)
	s, _ := st["state"].(string)
	return s
}

// A jobs.insert with a job ID in use is BigQuery's 409 "duplicate", the
// answer a client recovers from by fetching the existing job. It was a 400
// "failed to add job: job ... is already created".
func TestInsertExistingJobIDIsConflict(t *testing.T) {
	e := newAsyncTestEnv(t)
	first := e.insertQuery("job_dup", "SELECT 1 AS x")
	if first.status != http.StatusOK || jobState(first) != "DONE" {
		t.Fatalf("first insert: status=%d state=%q body=%v", first.status, jobState(first), first.body)
	}
	second := e.insertQuery("job_dup", "SELECT 1 AS x")
	if second.status != http.StatusConflict {
		t.Fatalf("second insert: status=%d, want 409: %v", second.status, second.body)
	}
	errBody, _ := second.body["error"].(map[string]any)
	if errBody["code"] != float64(409) || errBody["status"] != "ALREADY_EXISTS" ||
		errBody["message"] != "Already Exists: Job test:US.job_dup" {
		t.Fatalf("error body = %v", errBody)
	}
	errs, _ := errBody["errors"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["reason"] != "duplicate" {
		t.Fatalf("errors = %v, want one with reason duplicate", errs)
	}
	// What the client does next: fetch the job it could not create.
	got := e.do(http.MethodGet, "/jobs/job_dup", nil)
	if got.status != http.StatusOK || jobState(got) != "DONE" {
		t.Fatalf("jobs.get after conflict: status=%d state=%q", got.status, jobState(got))
	}
	results := e.do(http.MethodGet, "/queries/job_dup?maxResults=10", nil)
	if results.status != http.StatusOK || results.body["jobComplete"] != true {
		t.Fatalf("getQueryResults after conflict: status=%d %v", results.status, results.body)
	}
}

func shrinkJobInsertWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := jobInsertWait
	jobInsertWait = d
	t.Cleanup(func() { jobInsertWait = old })
}

// A query that outlives jobInsertWait: jobs.insert answers at once with the
// job RUNNING, and while it runs the requests a retrying client sends (the
// same insert, jobs.get, getQueryResults) are answered without waiting for
// the query. They used to queue behind it and then fail with 400 "job is
// already created".
func TestRunningJobAnswersClientRetries(t *testing.T) {
	shrinkJobInsertWait(t, 100*time.Millisecond)
	e := newAsyncTestEnv(t)

	start := time.Now()
	first := e.insertQuery("job_slow", slowQuery)
	if first.status != http.StatusOK || jobState(first) != "RUNNING" {
		t.Fatalf("insert: status=%d state=%q body=%v", first.status, jobState(first), first.body)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("insert of a long query took %s, want it to answer within jobInsertWait", d)
	}

	retryStart := time.Now()
	dup := e.insertQuery("job_slow", slowQuery)
	if dup.status != http.StatusConflict {
		t.Fatalf("insert with a running job ID: status=%d, want 409: %v", dup.status, dup.body)
	}
	got := e.do(http.MethodGet, "/jobs/job_slow", nil)
	if got.status != http.StatusOK || jobState(got) != "RUNNING" {
		t.Fatalf("jobs.get while running: status=%d state=%q", got.status, jobState(got))
	}
	notDone := e.do(http.MethodGet, "/queries/job_slow?timeoutMs=50", nil)
	if notDone.status != http.StatusOK || notDone.body["jobComplete"] != false {
		t.Fatalf("getQueryResults with a short timeout: status=%d %v", notDone.status, notDone.body)
	}
	if d := time.Since(retryStart); d > 600*time.Millisecond {
		t.Fatalf("requests about a running job took %s together, want them not to wait for the query", d)
	}

	done := e.do(http.MethodGet, "/queries/job_slow?timeoutMs=20000", nil)
	if done.status != http.StatusOK || done.body["jobComplete"] != true {
		t.Fatalf("getQueryResults: status=%d %v", done.status, done.body)
	}
	rows, _ := done.body["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want one", done.body["rows"])
	}
	got = e.do(http.MethodGet, "/jobs/job_slow", nil)
	if got.status != http.StatusOK || jobState(got) != "DONE" {
		t.Fatalf("jobs.get after the query: status=%d state=%q", got.status, jobState(got))
	}
	if dup := e.insertQuery("job_slow", slowQuery); dup.status != http.StatusConflict {
		t.Fatalf("insert with a finished job ID: status=%d, want 409", dup.status)
	}
}

// Requests that have nothing to do with a long query wait for it (the
// emulator runs one request at a time) but must not fail: every client call
// below has a deadline well below the time they would need to fail in the
// old setup, where the queued request lost its body to the read timeout.
func TestRequestsDuringLongQueryComplete(t *testing.T) {
	shrinkJobInsertWait(t, 100*time.Millisecond)
	e := newAsyncTestEnv(t)
	if r := e.insertQuery("job_slow_other", slowQuery); jobState(r) != "RUNNING" {
		t.Fatalf("insert: status=%d state=%q, want RUNNING: %v", r.status, jobState(r), r.body)
	}
	const clients = 5
	results := make(chan apiResult, clients)
	for i := 0; i < clients; i++ {
		go func(i int) {
			results <- e.insertQuery(fmt.Sprintf("job_during_%d", i), "SELECT 1 AS x")
		}(i)
	}
	for i := 0; i < clients; i++ {
		select {
		case r := <-results:
			if r.status != http.StatusOK || jobState(r) != "DONE" {
				t.Fatalf("insert during a long query: status=%d state=%q body=%v", r.status, jobState(r), r.body)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("insert during a long query did not finish")
		}
	}
}

// A query job runs after its request released the lock, so the project it
// received may be older than what requests in between added. Writing that
// copy back lost their datasets: dbt runs several workers against one
// emulator and then got 404 for a dataset it had just created.
func TestConcurrentJobsAndDatasetsKeepAllDatasets(t *testing.T) {
	e := newAsyncTestEnv(t)
	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			e.insertQuery(fmt.Sprintf("job_conc_%d", i), "SELECT 1 AS x")
		}(i)
		go func(i int) {
			defer wg.Done()
			e.do(http.MethodPost, "/datasets", map[string]any{
				"datasetReference": map[string]any{"projectId": "test", "datasetId": fmt.Sprintf("conc_ds_%d", i)},
			})
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if r := e.do(http.MethodGet, fmt.Sprintf("/datasets/conc_ds_%d", i), nil); r.status != http.StatusOK {
			t.Errorf("dataset conc_ds_%d: status=%d %v", i, r.status, r.body)
		}
	}
}
