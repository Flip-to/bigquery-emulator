package server_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/goccy/bigquery-emulator/server"
	"github.com/goccy/bigquery-emulator/types"
)

const concurrencyProject = "conc"

// concurrencyClient drives the emulator over plain HTTP, the way dbt and
// the client libraries do.
type concurrencyClient struct {
	base string
	http *http.Client
}

func newConcurrencyServer(t testing.TB, rows int) (*concurrencyClient, func()) {
	t.Helper()
	bqServer, err := server.New(server.TempStorage)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]map[string]any, 0, rows)
	for i := 0; i < rows; i++ {
		data = append(data, map[string]any{"id": int64(i), "grp": int64(i % 7), "name": fmt.Sprintf("n%d", i)})
	}
	if err := bqServer.Load(server.StructSource(types.NewProject(concurrencyProject, types.NewDataset("ds",
		types.NewTable("base", []*types.Column{
			types.NewColumn("id", types.INT64),
			types.NewColumn("grp", types.INT64),
			types.NewColumn("name", types.STRING),
		}, types.Data(data)),
	)))); err != nil {
		t.Fatal(err)
	}
	ts := bqServer.TestServer()
	c := &concurrencyClient{
		base: ts.URL + "/bigquery/v2/projects/" + concurrencyProject,
		http: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}},
	}
	return c, func() {
		ts.Close()
		_ = bqServer.Stop(context.Background())
	}
}

func (c *concurrencyClient) do(method, path string, body any) (int, map[string]any, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	var out map[string]any
	if len(data) != 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			return resp.StatusCode, nil, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, data)
		}
	}
	if resp.StatusCode >= 300 {
		return resp.StatusCode, out, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, data)
	}
	return resp.StatusCode, out, nil
}

// query runs sql through jobs.query and renders its rows as one string.
func (c *concurrencyClient) query(sql string) (string, error) {
	_, out, err := c.do("POST", "/queries", map[string]any{"query": sql, "useLegacySql": false})
	if err != nil {
		return "", err
	}
	return renderRows(out["rows"]), nil
}

// insertJob runs sql through jobs.insert and reads it back through
// getQueryResults, as the Python client (and so dbt) does.
func (c *concurrencyClient) insertJob(sql string) (string, error) {
	_, out, err := c.do("POST", "/jobs", map[string]any{
		"jobReference":  map[string]any{"projectId": concurrencyProject},
		"configuration": map[string]any{"query": map[string]any{"query": sql, "useLegacySql": false}},
	})
	if err != nil {
		return "", err
	}
	ref, _ := out["jobReference"].(map[string]any)
	jobID, _ := ref["jobId"].(string)
	if jobID == "" {
		return "", fmt.Errorf("jobs.insert returned no job id: %v", out)
	}
	if status, _ := out["status"].(map[string]any); status["errorResult"] != nil {
		return "", fmt.Errorf("job %s failed: %v", jobID, status["errorResult"])
	}
	if _, _, err := c.do("GET", "/jobs/"+jobID, nil); err != nil {
		return "", err
	}
	_, res, err := c.do("GET", "/queries/"+jobID, nil)
	if err != nil {
		return "", err
	}
	return renderRows(res["rows"]), nil
}

func (c *concurrencyClient) tableRows(dataset, table string) (string, error) {
	_, out, err := c.do("GET", "/datasets/"+dataset+"/tables/"+table, nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprint(out["numRows"]), nil
}

func renderRows(v any) string {
	rows, _ := v.([]any)
	var sb strings.Builder
	for _, r := range rows {
		f, _ := r.(map[string]any)["f"].([]any)
		for i, cell := range f {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprint(&sb, cell.(map[string]any)["v"])
		}
		sb.WriteByte(';')
	}
	return sb.String()
}

// concurrencyWorkload is what one client does: reads of the shared table
// through every read path, plus a table of its own that it creates, reads,
// lists and drops. It returns one line per step, which must be the same
// whether the clients run one after another or all at once.
func concurrencyWorkload(c *concurrencyClient, worker, rounds int) ([]string, error) {
	var out []string
	add := func(step string, v string, err error) error {
		if err != nil {
			return fmt.Errorf("worker %d %s: %w", worker, step, err)
		}
		out = append(out, step+"="+v)
		return nil
	}
	for round := 0; round < rounds; round++ {
		own := fmt.Sprintf("w%d_r%d", worker, round)
		steps := []struct {
			name string
			run  func() (string, error)
		}{
			{"agg", func() (string, error) {
				return c.query(fmt.Sprintf("SELECT grp, COUNT(*), SUM(id) FROM ds.base WHERE id >= %d GROUP BY grp ORDER BY grp", worker))
			}},
			{"tables.get", func() (string, error) { return c.tableRows("ds", "base") }},
			{"create", func() (string, error) {
				return c.query(fmt.Sprintf("CREATE TABLE ds.%s AS SELECT id, name FROM ds.base WHERE MOD(id, %d) = 0", own, worker+2))
			}},
			{"job", func() (string, error) {
				return c.insertJob(fmt.Sprintf("SELECT COUNT(*), MAX(name) FROM ds.%s", own))
			}},
			{"own.get", func() (string, error) { return c.tableRows("ds", own) }},
			{"join", func() (string, error) {
				return c.insertJob(fmt.Sprintf("SELECT COUNT(*) FROM ds.base b JOIN ds.%s o USING (id) WHERE b.grp = %d", own, round%7))
			}},
			{"list", func() (string, error) {
				_, res, err := c.do("GET", "/datasets/ds/tables", nil)
				if err != nil {
					return "", err
				}
				// Other workers' tables come and go; only ours is stable.
				found := strings.Contains(fmt.Sprint(res["tables"]), own)
				return fmt.Sprint(found), nil
			}},
			{"insert", func() (string, error) {
				return c.query(fmt.Sprintf("INSERT INTO ds.%s (id, name) VALUES (-1, 'x')", own))
			}},
			{"count", func() (string, error) { return c.query(fmt.Sprintf("SELECT COUNT(*) FROM ds.%s", own)) }},
			{"drop", func() (string, error) { return c.query(fmt.Sprintf("DROP TABLE ds.%s", own)) }},
		}
		for _, s := range steps {
			v, err := s.run()
			if err := add(fmt.Sprintf("r%d.%s", round, s.name), v, err); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

// TestConcurrentClients runs clients that mix SELECTs, jobs.insert +
// getQueryResults, tables.get, tables.list and CREATE / INSERT / DROP
// against one emulator, first one client at a time and then all at once,
// and checks every client saw the same results both ways. Run it with
// -race: read-only requests share the emulator's lock and run in parallel.
func TestConcurrentClients(t *testing.T) {
	const (
		workers = 8
		rounds  = 3
	)
	run := func(concurrent bool) map[int][]string {
		c, stop := newConcurrencyServer(t, 500)
		defer stop()
		results := make(map[int][]string, workers)
		var (
			mu   sync.Mutex
			wg   sync.WaitGroup
			errs []error
		)
		for w := 0; w < workers; w++ {
			work := func(w int) {
				out, err := concurrencyWorkload(c, w, rounds)
				mu.Lock()
				defer mu.Unlock()
				results[w] = out
				if err != nil {
					errs = append(errs, err)
				}
			}
			if !concurrent {
				work(w)
				continue
			}
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				work(w)
			}(w)
		}
		wg.Wait()
		for _, err := range errs {
			t.Error(err)
		}
		return results
	}
	serial := run(false)
	if t.Failed() {
		t.FailNow()
	}
	concurrent := run(true)
	for w := 0; w < workers; w++ {
		want, got := strings.Join(serial[w], "\n"), strings.Join(concurrent[w], "\n")
		if want != got {
			t.Errorf("worker %d: concurrent results differ from serial\nserial:\n%s\nconcurrent:\n%s", w, want, got)
		}
	}
}

// BenchmarkConcurrentSelect measures jobs.query SELECT throughput as the
// number of concurrent clients grows. With the former global mutex the
// throughput was flat; read-only queries now share the lock.
func BenchmarkConcurrentSelect(b *testing.B) {
	c, stop := newConcurrencyServer(b, 20000)
	defer stop()
	const sql = "SELECT grp, COUNT(DISTINCT name), SUM(id * id) FROM ds.base GROUP BY grp ORDER BY grp"
	want, err := c.query(sql)
	if err != nil {
		b.Fatal(err)
	}
	for _, clients := range []int{1, 2, 4, 8} {
		b.Run(fmt.Sprintf("clients=%d", clients), func(b *testing.B) {
			var (
				wg   sync.WaitGroup
				next = make(chan struct{})
				errs = make(chan error, clients)
			)
			start := time.Now()
			for i := 0; i < clients; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range next {
						got, err := c.query(sql)
						if err == nil && got != want {
							err = fmt.Errorf("got %s, want %s", got, want)
						}
						if err != nil {
							errs <- err
							return
						}
					}
				}()
			}
			for i := 0; i < b.N; i++ {
				select {
				case next <- struct{}{}:
				case err := <-errs:
					b.Fatal(err)
				}
			}
			close(next)
			wg.Wait()
			select {
			case err := <-errs:
				b.Fatal(err)
			default:
			}
			b.ReportMetric(float64(b.N)/time.Since(start).Seconds(), "queries/s")
		})
	}
}

