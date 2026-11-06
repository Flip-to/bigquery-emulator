package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/bigquery-emulator/types"
)

// TestQueuedRequestSurvivesReadDeadline reproduces dbt-virtual#34: requests
// are serialized, and a request that waited longer than the (old) 15 s
// ReadTimeout in the queue failed reading its body with "i/o timeout". The
// timeout is shrunk to 300 ms; a slow query holds the sequential lock past it
// and a queued jobs.query must still succeed.
func TestQueuedRequestSurvivesReadDeadline(t *testing.T) {
	oldTimeout := readHeaderTimeout
	readHeaderTimeout = 300 * time.Millisecond
	defer func() { readHeaderTimeout = oldTimeout }()

	const projectID = "readtimeout"
	bqServer, err := New(TempStorage)
	if err != nil {
		t.Fatal(err)
	}
	if err := bqServer.Load(StructSource(types.NewProject(projectID, types.NewDataset("ds")))); err != nil {
		t.Fatal(err)
	}
	freeAddr := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().String()
	}
	addr, grpcAddr := freeAddr(), freeAddr()

	ctx := context.Background()
	go func() { _ = bqServer.Serve(ctx, addr, grpcAddr) }()
	defer bqServer.Stop(ctx)
	url := "http://" + addr + "/bigquery/v2/projects/" + projectID + "/queries"
	for i := 0; ; i++ {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			break
		}
		if i > 100 {
			t.Fatal("server did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}

	post := func(q string) (int, string, time.Duration, error) {
		start := time.Now()
		body := fmt.Sprintf(`{"query": %q, "useLegacySql": false}`, q)
		resp, err := http.Post(url, "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			return 0, "", time.Since(start), err
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data), time.Since(start), nil
	}

	var wg sync.WaitGroup
	var slowDur time.Duration
	var slowErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, slowDur, slowErr = post(`SELECT COUNT(*) FROM UNNEST(GENERATE_ARRAY(1, 2000000)) a`)
	}()
	time.Sleep(100 * time.Millisecond) // let the slow query take the lock

	// Pad the body past the server's 4 KiB read buffer so reading it needs a
	// socket read after the lock is acquired (a small body is already buffered).
	code, data, queuedDur, err := post("SELECT 1 AS x /*" + strings.Repeat("p", 256<<10) + "*/")
	wg.Wait()
	if slowErr != nil {
		t.Fatal(slowErr)
	}
	if slowDur < 2*readHeaderTimeout {
		t.Fatalf("slow query finished in %v, too fast to outlast the deadline; make it slower", slowDur)
	}
	if err != nil {
		t.Fatalf("queued request failed after %v: %v", queuedDur, err)
	}
	if code != http.StatusOK {
		t.Fatalf("queued request after %v: status %d: %s", queuedDur, code, data)
	}
}
