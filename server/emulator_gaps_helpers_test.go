package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"github.com/goccy/bigquery-emulator/server"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// gapsEnv is a running emulator plus a client, shared by the regression tests
// for BigQuery behaviours the emulator used to get wrong.
type gapsEnv struct {
	ctx    context.Context
	client *bigquery.Client
	url    string
}

func newGapsEnv(t *testing.T) *gapsEnv {
	t.Helper()
	ctx := context.Background()
	const projectName = "test"
	bqServer, err := server.New(server.TempStorage)
	if err != nil {
		t.Fatal(err)
	}
	if err := bqServer.SetProject(projectName); err != nil {
		t.Fatal(err)
	}
	if err := bqServer.Load(server.YAMLSource(filepath.Join("testdata", "data.yaml"))); err != nil {
		t.Fatal(err)
	}
	testServer := bqServer.TestServer()
	t.Cleanup(func() {
		testServer.Close()
		bqServer.Stop(ctx)
	})
	client, err := bigquery.NewClient(
		ctx,
		projectName,
		option.WithEndpoint(testServer.URL),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return &gapsEnv{ctx: ctx, client: client, url: testServer.URL}
}

func (e *gapsEnv) exec(t *testing.T, sql string) error {
	t.Helper()
	job, err := e.client.Query(sql).Run(e.ctx)
	if err != nil {
		return err
	}
	status, err := job.Wait(e.ctx)
	if err != nil {
		return err
	}
	return status.Err()
}

// rows runs a query and returns every row as a slice of values.
func (e *gapsEnv) rows(t *testing.T, sql string) ([][]bigquery.Value, *bigquery.Schema, error) {
	t.Helper()
	it, err := e.client.Query(sql).Read(e.ctx)
	if err != nil {
		return nil, nil, err
	}
	var out [][]bigquery.Value
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		out = append(out, row)
	}
	return out, &it.Schema, nil
}

func (e *gapsEnv) mustRows(t *testing.T, sql string) [][]bigquery.Value {
	t.Helper()
	rows, _, err := e.rows(t, sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return rows
}

// getJSON calls the REST API and decodes the JSON body into out (when
// non-nil and the status is 200). It returns the HTTP status.
func (e *gapsEnv) getJSON(t *testing.T, path string, out interface{}) int {
	t.Helper()
	res, err := http.Get(e.url + path)
	if err != nil {
		t.Fatal(err)
	}
	return e.readJSON(t, "GET", path, res, out)
}

func (e *gapsEnv) postJSON(t *testing.T, path string, body interface{}, out interface{}) int {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(e.url+path, "application/json", strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	return e.readJSON(t, "POST", path, res, out)
}

func (e *gapsEnv) readJSON(t *testing.T, method, path string, res *http.Response, out interface{}) int {
	t.Helper()
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Logf("%s %s -> %d: %s", method, path, res.StatusCode, strings.TrimSpace(string(data)))
	} else if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, data)
		}
	}
	return res.StatusCode
}
