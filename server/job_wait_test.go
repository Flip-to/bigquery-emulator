package server_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestGetQueryResultsOfFinishedJobDoesNotWait checks that asking for the
// results of a job that is already done answers at once. Job.Wait used to
// sleep one 100 ms poll interval before its first look, so every
// getQueryResults call (dbt makes several per unit test) cost at least 100 ms.
func TestGetQueryResultsOfFinishedJobDoesNotWait(t *testing.T) {
	e := newGapsEnv(t)
	job, err := e.client.Query("SELECT 1 AS x").Run(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := job.Wait(e.ctx); err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("%s/projects/test/queries/%s", strings.TrimSuffix(e.url, "/"), job.ID())

	const calls = 10
	start := time.Now()
	for i := 0; i < calls; i++ {
		res, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("getQueryResults: status %d: %s", res.StatusCode, body)
		}
	}
	// With the old poll-first-then-look loop, 10 calls take at least 1 s.
	if elapsed := time.Since(start); elapsed >= calls*100*time.Millisecond/2 {
		t.Fatalf("%d getQueryResults calls on a finished job took %v; want well under %v",
			calls, elapsed, calls*100*time.Millisecond)
	}
}
