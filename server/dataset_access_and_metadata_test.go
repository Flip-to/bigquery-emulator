package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestDatasetsPatchStoresAccessAndIsPersisted: datasets.patch and datasets.update used to change
// only an in-memory copy, so a later datasets.get did not see the change. An `access` list (an
// authorized dataset for views, as `bq`/dbt-bigquery add it) must be stored and returned.
func TestDatasetsPatchStoresAccessAndIsPersisted(t *testing.T) {
	e := newGapsEnv(t)
	if err := e.exec(t, "CREATE SCHEMA `test`.`phys`; CREATE SCHEMA `test`.`env_view`"); err != nil {
		t.Fatal(err)
	}
	url := e.url + "/bigquery/v2/projects/test/datasets/phys"

	reader := map[string]any{"role": "READER", "userByEmail": "analyst@example.com"}
	authorized := map[string]any{"dataset": map[string]any{
		"dataset":     map[string]any{"projectId": "test", "datasetId": "env_view"},
		"targetTypes": []any{"VIEWS"},
	}}
	body := func(access ...any) string {
		return fmt.Sprintf(`{"access": %s}`, mustJSON(t, access))
	}

	code, got := httpJSON(t, http.MethodPatch, url, body(reader), nil)
	if code != http.StatusOK {
		t.Fatalf("patch: %d %v", code, got)
	}
	if _, get := httpJSON(t, http.MethodGet, url, "", nil); !reflect.DeepEqual(get["access"], []any{reader}) {
		t.Fatalf("access after the first patch = %v", get["access"])
	}

	// The dbt-bigquery flow: read the list, append the authorized dataset, patch it back.
	code, _ = httpJSON(t, http.MethodPatch, url, body(reader, authorized), nil)
	if code != http.StatusOK {
		t.Fatalf("second patch: %d", code)
	}
	if _, get := httpJSON(t, http.MethodGet, url, "", nil); !reflect.DeepEqual(get["access"], []any{reader, authorized}) {
		t.Fatalf("access after the second patch = %v", get["access"])
	}

	// A patch without `access` leaves the list alone, and the other fields persist too.
	code, _ = httpJSON(t, http.MethodPatch, url, `{"description":"physical tables"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("description patch: %d", code)
	}
	_, get := httpJSON(t, http.MethodGet, url, "", nil)
	if get["description"] != "physical tables" || !reflect.DeepEqual(get["access"], []any{reader, authorized}) {
		t.Fatalf("after the description patch: description=%v access=%v", get["description"], get["access"])
	}

	// datasets.update replaces the resource, including the list.
	put := fmt.Sprintf(`{"datasetReference":{"projectId":"test","datasetId":"phys"},"access": %s}`, mustJSON(t, []any{authorized}))
	code, _ = httpJSON(t, http.MethodPut, url, put, nil)
	if code != http.StatusOK {
		t.Fatalf("update: %d", code)
	}
	_, get = httpJSON(t, http.MethodGet, url, "", nil)
	if !reflect.DeepEqual(get["access"], []any{authorized}) {
		t.Fatalf("access after update = %v", get["access"])
	}
	if get["description"] != nil {
		t.Fatalf("datasets.update must replace the resource, description = %v", get["description"])
	}
}

// TestCreateViewAndTableOptionsAreTableMetadata: OPTIONS(description, friendly_name, labels) of
// CREATE VIEW / CREATE TABLE ... AS SELECT are returned by tables.get (they were accepted and dropped).
func TestCreateViewAndTableOptionsAreTableMetadata(t *testing.T) {
	e := newGapsEnv(t)
	if err := e.exec(t, "CREATE SCHEMA `test`.`meta`; CREATE TABLE `test`.`meta`.`t` (id INT64, amount FLOAT64)"); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(t, "CREATE OR REPLACE VIEW `test`.`meta`.`v` OPTIONS(description='Gross orders: \"quoted\"; it\\'s', "+
		"friendly_name='Orders', labels=[('team','data'),('tier','gold')]) AS SELECT * FROM `test`.`meta`.`t`"); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(t, "CREATE TABLE `test`.`meta`.`t2` OPTIONS(description=\"table doc\", labels=[('a','b')]) AS SELECT 1 AS x"); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(t, "CREATE OR REPLACE VIEW `test`.`meta`.`plain` AS SELECT * FROM `test`.`meta`.`t`"); err != nil {
		t.Fatal(err)
	}

	tableURL := func(name string) string {
		return e.url + "/bigquery/v2/projects/test/datasets/meta/tables/" + name
	}
	_, view := httpJSON(t, http.MethodGet, tableURL("v"), "", nil)
	if view["type"] != "VIEW" || view["description"] != "Gross orders: \"quoted\"; it's" || view["friendlyName"] != "Orders" {
		t.Fatalf("view = %v", view)
	}
	if !reflect.DeepEqual(view["labels"], map[string]any{"team": "data", "tier": "gold"}) {
		t.Fatalf("view labels = %v", view["labels"])
	}
	_, table := httpJSON(t, http.MethodGet, tableURL("t2"), "", nil)
	if table["description"] != "table doc" || !reflect.DeepEqual(table["labels"], map[string]any{"a": "b"}) {
		t.Fatalf("table = %v", table)
	}
	_, plain := httpJSON(t, http.MethodGet, tableURL("plain"), "", nil)
	if plain["description"] != nil || plain["labels"] != nil {
		t.Fatalf("a view without OPTIONS carries metadata: %v", plain)
	}

	// Column descriptions go through tables.patch (what dbt-bigquery's update_columns does) and are kept.
	patch := `{"schema":{"fields":[{"name":"id","type":"INTEGER","description":"Order id"},{"name":"amount","type":"FLOAT"}]}}`
	if code, got := httpJSON(t, http.MethodPatch, tableURL("v"), patch, nil); code != http.StatusOK {
		t.Fatalf("tables.patch: %d %v", code, got)
	}
	_, view = httpJSON(t, http.MethodGet, tableURL("v"), "", nil)
	fields := view["schema"].(map[string]any)["fields"].([]any)
	if fields[0].(map[string]any)["description"] != "Order id" || view["description"] != "Gross orders: \"quoted\"; it's" {
		t.Fatalf("after the column patch: %v", view)
	}
}

// TestCreateTableClone: CREATE TABLE ... CLONE copies the source (even across datasets) and the clone
// outlives its source; IF NOT EXISTS on an existing clone changes nothing.
func TestCreateTableClone(t *testing.T) {
	e := newGapsEnv(t)
	if err := e.exec(t, "CREATE SCHEMA `test`.`src`; CREATE SCHEMA `test`.`dst`"); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(t, "CREATE TABLE `test`.`src`.`t` (id INT64, d DATE) PARTITION BY d"); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(t, "INSERT INTO `test`.`src`.`t` (id, d) VALUES (1, DATE '2026-10-01'), (2, DATE '2026-10-02')"); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(t, "CREATE TABLE IF NOT EXISTS `test`.`dst`.`t` CLONE `test`.`src`.`t`"); err != nil {
		t.Fatal(err)
	}
	_, table := httpJSON(t, http.MethodGet, e.url+"/bigquery/v2/projects/test/datasets/dst/tables/t", "", nil)
	if table["type"] != "TABLE" || table["schema"] == nil {
		t.Fatalf("the clone is not registered as a table: %v", table)
	}
	if got := fmt.Sprint(e.mustRows(t, "SELECT COUNT(*), SUM(id) FROM `test`.`dst`.`t`")); got != "[[2 3]]" {
		t.Fatalf("clone rows = %s", got)
	}
	if err := e.exec(t, "INSERT INTO `test`.`src`.`t` (id, d) VALUES (3, DATE '2026-10-03')"); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(t, "CREATE TABLE IF NOT EXISTS `test`.`dst`.`t` CLONE `test`.`src`.`t`"); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(t, "DROP TABLE `test`.`src`.`t`"); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(e.mustRows(t, "SELECT COUNT(*) FROM `test`.`dst`.`t`")); got != "[[2]]" {
		t.Fatalf("the clone changed with its source or lost its data: %s", got)
	}
	// IF NOT EXISTS ... AS SELECT on an existing table is a no-op as well.
	if err := e.exec(t, "CREATE TABLE IF NOT EXISTS `test`.`dst`.`t` AS SELECT 9 AS id, DATE '2026-01-01' AS d"); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(e.mustRows(t, "SELECT COUNT(*) FROM `test`.`dst`.`t`")); got != "[[2]]" {
		t.Fatalf("IF NOT EXISTS replaced the table: %s", got)
	}
}

// TestDropSchema: DROP SCHEMA [IF EXISTS] [CASCADE] drops the dataset (it was "currently unsupported").
// Without CASCADE a dataset that holds tables is refused; its name can be used again afterwards.
func TestDropSchema(t *testing.T) {
	e := newGapsEnv(t)
	datasetURL := func(name string) string { return e.url + "/bigquery/v2/projects/test/datasets/" + name }
	if err := e.exec(t, "CREATE SCHEMA `test`.`env_pr_7`; CREATE VIEW `test`.`env_pr_7`.`v` AS SELECT 1 AS x; CREATE TABLE `test`.`env_pr_7`.`t` (id INT64)"); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(t, "DROP SCHEMA `test`.`env_pr_7`"); err == nil {
		t.Fatal("DROP SCHEMA of a dataset with tables must be refused without CASCADE")
	}
	if code, _ := httpJSON(t, http.MethodGet, datasetURL("env_pr_7"), "", nil); code != http.StatusOK {
		t.Fatalf("the refused DROP SCHEMA removed the dataset: %d", code)
	}
	if err := e.exec(t, "DROP SCHEMA IF EXISTS `test`.`env_pr_7` CASCADE"); err != nil {
		t.Fatal(err)
	}
	if code, _ := httpJSON(t, http.MethodGet, datasetURL("env_pr_7"), "", nil); code != http.StatusNotFound {
		t.Fatalf("dataset after DROP SCHEMA CASCADE: %d, want 404", code)
	}
	if err := e.exec(t, "SELECT * FROM `test`.`env_pr_7`.`t`"); err == nil {
		t.Fatal("the table of a dropped dataset is still queryable")
	}
	// IF EXISTS on a missing dataset is a no-op, without it an error.
	if err := e.exec(t, "DROP SCHEMA IF EXISTS `test`.`env_pr_7` CASCADE"); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(t, "DROP SCHEMA `test`.`env_pr_7`"); err == nil {
		t.Fatal("DROP SCHEMA of a missing dataset must fail without IF EXISTS")
	}
	// An empty dataset drops without CASCADE, and the name is free for a new dataset with the same table.
	if err := e.exec(t, "CREATE SCHEMA `test`.`env_pr_8`; DROP SCHEMA `test`.`env_pr_8`"); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(t, "CREATE SCHEMA `test`.`env_pr_7`; CREATE TABLE `test`.`env_pr_7`.`t` (id INT64)"); err != nil {
		t.Fatalf("recreating the dropped dataset: %v", err)
	}
}
