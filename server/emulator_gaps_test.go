package server_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	bigqueryv2 "google.golang.org/api/bigquery/v2"
)

// TestAlterTableAddColumn: ALTER TABLE ... ADD COLUMN [IF NOT EXISTS] adds
// the column to the table metadata and to the storage. It used to return OK
// and add nothing, so a later SELECT failed with "Unrecognized name".
func TestAlterTableAddColumn(t *testing.T) {
	e := newGapsEnv(t)
	for _, q := range []string{
		"CREATE TABLE `test`.`dataset1`.`alter_t` (id INT64)",
		"INSERT INTO `test`.`dataset1`.`alter_t` (id) VALUES (1)",
		"ALTER TABLE `test`.`dataset1`.`alter_t` ADD COLUMN IF NOT EXISTS schema_key STRING",
		"ALTER TABLE `test`.`dataset1`.`alter_t` ADD COLUMN note STRING",
	} {
		if err := e.exec(t, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// Storage: the column can be selected and written.
	rows := e.mustRows(t, "SELECT id, schema_key, note FROM `test`.`dataset1`.`alter_t`")
	if len(rows) != 1 || rows[0][0] != int64(1) || rows[0][1] != nil || rows[0][2] != nil {
		t.Fatalf("rows = %v", rows)
	}
	if err := e.exec(t, "INSERT INTO `test`.`dataset1`.`alter_t` (id, schema_key) VALUES (2, 'k')"); err != nil {
		t.Fatal(err)
	}
	rows = e.mustRows(t, "SELECT id FROM `test`.`dataset1`.`alter_t` WHERE schema_key = 'k'")
	if len(rows) != 1 || rows[0][0] != int64(2) {
		t.Fatalf("filtered rows = %v", rows)
	}

	// Metadata: tables.get lists the new columns.
	md, err := e.client.Dataset("dataset1").Table("alter_t").Metadata(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range md.Schema {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, ","); got != "id,schema_key,note" {
		t.Fatalf("table metadata schema = %s", got)
	}

	// Adding an existing column without IF NOT EXISTS fails; with it, it is a no-op.
	if err := e.exec(t, "ALTER TABLE `test`.`dataset1`.`alter_t` ADD COLUMN schema_key STRING"); err == nil {
		t.Fatal("expected an error for an existing column")
	}
	if err := e.exec(t, "ALTER TABLE `test`.`dataset1`.`alter_t` ADD COLUMN IF NOT EXISTS schema_key STRING"); err != nil {
		t.Fatal(err)
	}
}

// TestRoutinesFromSQL: a routine created with CREATE FUNCTION is returned by
// routines.get and routines.list (404 and an empty list before), and appears in
// INFORMATION_SCHEMA.ROUTINES (table not found before).
func TestRoutinesFromSQL(t *testing.T) {
	e := newGapsEnv(t)
	for _, q := range []string{
		"CREATE FUNCTION `test`.`dataset1`.`add_one`(x INT64) AS (x + 1)",
		"CREATE TABLE FUNCTION `test`.`dataset1`.`two_rows`(n INT64) AS SELECT n AS v UNION ALL SELECT n + 1",
	} {
		if err := e.exec(t, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	base := "/bigquery/v2/projects/test/datasets/dataset1/routines"
	var got bigqueryv2.Routine
	if code := e.getJSON(t, base+"/add_one", &got); code != http.StatusOK {
		t.Fatalf("routines.get = %d", code)
	}
	if got.RoutineReference == nil || got.RoutineReference.RoutineId != "add_one" ||
		got.RoutineReference.DatasetId != "dataset1" || got.RoutineReference.ProjectId != "test" {
		t.Fatalf("routine reference = %+v", got.RoutineReference)
	}
	if got.RoutineType != "SCALAR_FUNCTION" || got.Language != "SQL" {
		t.Fatalf("routine = %+v", got)
	}
	if len(got.Arguments) != 1 || got.Arguments[0].Name != "x" || got.Arguments[0].DataType.TypeKind != "INT64" {
		t.Fatalf("arguments = %+v", got.Arguments)
	}
	if got.ReturnType == nil || got.ReturnType.TypeKind != "INT64" {
		t.Fatalf("return type = %+v", got.ReturnType)
	}

	var list bigqueryv2.ListRoutinesResponse
	if code := e.getJSON(t, base, &list); code != http.StatusOK {
		t.Fatalf("routines.list = %d", code)
	}
	ids := map[string]string{}
	for _, r := range list.Routines {
		ids[r.RoutineReference.RoutineId] = r.RoutineType
	}
	if len(ids) != 2 || ids["add_one"] != "SCALAR_FUNCTION" || ids["two_rows"] != "TABLE_VALUED_FUNCTION" {
		t.Fatalf("routines.list = %v", ids)
	}

	rows := e.mustRows(t, "SELECT routine_name, routine_type, routine_schema FROM `test`.`dataset1`.INFORMATION_SCHEMA.ROUTINES ORDER BY routine_name")
	want := [][]bigquery.Value{
		{"add_one", "FUNCTION", "dataset1"},
		{"two_rows", "TABLE FUNCTION", "dataset1"},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("INFORMATION_SCHEMA.ROUTINES = %v, want %v", rows, want)
	}

	// Dropping the function removes it from the metadata again.
	if err := e.exec(t, "DROP FUNCTION `test`.`dataset1`.`add_one`"); err != nil {
		t.Fatal(err)
	}
	if code := e.getJSON(t, base+"/add_one", nil); code != http.StatusNotFound {
		t.Fatalf("routines.get after DROP = %d, want 404", code)
	}
	list = bigqueryv2.ListRoutinesResponse{}
	e.getJSON(t, base, &list)
	if len(list.Routines) != 1 || list.Routines[0].RoutineReference.RoutineId != "two_rows" {
		t.Fatalf("routines.list after DROP = %+v", list.Routines)
	}
}

// TestRoutinesInsertedByRESTAreListed: a routine created with routines.insert
// is found by routines.get and routines.list.
func TestRoutinesInsertedByRESTAreListed(t *testing.T) {
	e := newGapsEnv(t)
	body := map[string]interface{}{
		"routineReference": map[string]string{"projectId": "test", "datasetId": "dataset1", "routineId": "rest_fn"},
		"routineType":      "SCALAR_FUNCTION",
		"language":         "SQL",
		"definitionBody":   "x * 2",
		"arguments":        []map[string]interface{}{{"name": "x", "dataType": map[string]string{"typeKind": "INT64"}}},
		"returnType":       map[string]string{"typeKind": "INT64"},
	}
	if code := e.postJSON(t, "/bigquery/v2/projects/test/datasets/dataset1/routines", body, nil); code != http.StatusOK {
		t.Fatalf("routines.insert = %d", code)
	}
	var got bigqueryv2.Routine
	if code := e.getJSON(t, "/bigquery/v2/projects/test/datasets/dataset1/routines/rest_fn", &got); code != http.StatusOK {
		t.Fatalf("routines.get = %d", code)
	}
	if got.DefinitionBody != "x * 2" {
		t.Fatalf("routine = %+v", got)
	}
	var list bigqueryv2.ListRoutinesResponse
	if code := e.getJSON(t, "/bigquery/v2/projects/test/datasets/dataset1/routines", &list); code != http.StatusOK ||
		len(list.Routines) != 1 || list.Routines[0].RoutineReference.RoutineId != "rest_fn" {
		t.Fatalf("routines.list = %d %+v", code, list.Routines)
	}
	rows := e.mustRows(t, "SELECT `test`.`dataset1`.`rest_fn`(21)")
	if len(rows) != 1 || rows[0][0] != int64(42) {
		t.Fatalf("calling the routine = %v", rows)
	}
}

// TestInformationSchemaPartitionsAndLegacyTables: INFORMATION_SCHEMA.PARTITIONS
// and `dataset.__TABLES__` used to fail with "Table not found".
func TestInformationSchemaPartitionsAndLegacyTables(t *testing.T) {
	e := newGapsEnv(t)
	for _, q := range []string{
		"CREATE TABLE `test`.`dataset1`.`part_t` (id INT64)",
		"INSERT INTO `test`.`dataset1`.`part_t` (id) VALUES (1), (2), (3)",
		"CREATE VIEW `test`.`dataset1`.`part_v` AS SELECT id FROM `test`.`dataset1`.`part_t`",
	} {
		if err := e.exec(t, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	rows := e.mustRows(t, "SELECT total_rows FROM `test`.`dataset1`.INFORMATION_SCHEMA.PARTITIONS WHERE table_name = 'part_t'")
	if len(rows) != 1 || rows[0][0] != int64(3) {
		t.Fatalf("PARTITIONS rows = %v", rows)
	}
	rows = e.mustRows(t, "SELECT table_name FROM `test`.`dataset1`.INFORMATION_SCHEMA.PARTITIONS WHERE table_name = 'part_v'")
	if len(rows) != 0 {
		t.Fatalf("a view has no partitions, got %v", rows)
	}

	rows = e.mustRows(t, "SELECT table_id, row_count, type FROM `test`.`dataset1`.__TABLES__ WHERE table_id LIKE 'part_%' ORDER BY table_id")
	want := [][]bigquery.Value{{"part_t", int64(3), int64(1)}, {"part_v", int64(0), int64(2)}}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("__TABLES__ = %v, want %v", rows, want)
	}
	// The dataset's own tables from the test data are listed too.
	rows = e.mustRows(t, "SELECT COUNT(*) FROM `test`.`dataset1`.__TABLES__")
	if n, _ := rows[0][0].(int64); n < 3 {
		t.Fatalf("__TABLES__ count = %v", rows)
	}
}

// TestCreateSchemaCreatesDataset: CREATE SCHEMA [IF NOT EXISTS] used to
// return OK without creating the dataset.
func TestCreateSchemaCreatesDataset(t *testing.T) {
	e := newGapsEnv(t)
	if err := e.exec(t, "CREATE SCHEMA IF NOT EXISTS `test`.`reg_eu` OPTIONS (location='EU')"); err != nil {
		t.Fatal(err)
	}
	var ds bigqueryv2.Dataset
	if code := e.getJSON(t, "/bigquery/v2/projects/test/datasets/reg_eu", &ds); code != http.StatusOK {
		t.Fatalf("datasets.get = %d", code)
	}
	if ds.Location != "EU" || ds.DatasetReference == nil || ds.DatasetReference.DatasetId != "reg_eu" {
		t.Fatalf("dataset = %+v", ds)
	}
	md, err := e.client.Dataset("reg_eu").Metadata(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.Location != "EU" {
		t.Fatalf("location = %q", md.Location)
	}
	// Objects can be created in it right away, in the same job or later.
	if err := e.exec(t, "CREATE TABLE `test`.`reg_eu`.`t` (id INT64)"); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(t, "CREATE SCHEMA `test`.`plain_ds`; CREATE TABLE `test`.`plain_ds`.`t` (id INT64)"); err != nil {
		t.Fatal(err)
	}
	if code := e.getJSON(t, "/bigquery/v2/projects/test/datasets/plain_ds/tables/t", nil); code != http.StatusOK {
		t.Fatalf("table in the new dataset: %d", code)
	}

	// IF NOT EXISTS on an existing dataset keeps it (and its location) as is.
	if err := e.exec(t, "CREATE SCHEMA IF NOT EXISTS `test`.`reg_eu` OPTIONS (location='US')"); err != nil {
		t.Fatal(err)
	}
	md, err = e.client.Dataset("reg_eu").Metadata(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.Location != "EU" {
		t.Fatalf("location after IF NOT EXISTS = %q, want EU", md.Location)
	}
	// Without IF NOT EXISTS an existing dataset is an error.
	if err := e.exec(t, "CREATE SCHEMA `test`.`reg_eu`"); err == nil {
		t.Fatal("expected an error: dataset already exists")
	}
}

// TestInformationSchemaTablesCreationTimeIsTimestamp: creation_time used to be
// a STRING, so comparing it with a TIMESTAMP failed to analyze.
func TestInformationSchemaTablesCreationTimeIsTimestamp(t *testing.T) {
	e := newGapsEnv(t)
	if err := e.exec(t, "CREATE TABLE `test`.`dataset1`.`fresh_t` (id INT64)"); err != nil {
		t.Fatal(err)
	}
	rows := e.mustRows(t, "SELECT table_name FROM `test`.`dataset1`.INFORMATION_SCHEMA.TABLES "+
		"WHERE creation_time > TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 HOUR)")
	found := false
	for _, r := range rows {
		if r[0] == "fresh_t" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fresh_t not among the tables created in the last hour: %v", rows)
	}
	rows = e.mustRows(t, "SELECT table_name FROM `test`.`dataset1`.INFORMATION_SCHEMA.TABLES "+
		"WHERE table_name = 'fresh_t' AND creation_time < TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 HOUR)")
	if len(rows) != 0 {
		t.Fatalf("fresh_t is not older than an hour: %v", rows)
	}

	rows, schema, err := e.rows(t, "SELECT creation_time FROM `test`.`dataset1`.INFORMATION_SCHEMA.TABLES WHERE table_name = 'fresh_t'")
	if err != nil {
		t.Fatal(err)
	}
	if len(*schema) != 1 || (*schema)[0].Type != bigquery.TimestampFieldType {
		t.Fatalf("creation_time column type = %+v, want TIMESTAMP", schema)
	}
	created, ok := rows[0][0].(time.Time)
	if !ok {
		t.Fatalf("creation_time value is %T, want time.Time", rows[0][0])
	}
	if time.Since(created) > time.Hour || created.After(time.Now().Add(time.Minute)) {
		t.Fatalf("creation_time = %v", created)
	}
}

// TestInformationSchemaPartitionsPerPartition: a PARTITION BY table reports one
// INFORMATION_SCHEMA.PARTITIONS row per partition, the shape dbt-virtual's
// partition-coverage check reads.
func TestInformationSchemaPartitionsPerPartition(t *testing.T) {
	e := newGapsEnv(t)
	for _, q := range []string{
		"CREATE TABLE `test`.`dataset1`.`cov_cand` (d DATE, v INT64) PARTITION BY d",
		"INSERT INTO `test`.`dataset1`.`cov_cand` VALUES (DATE '2026-10-01', 1)",
		"CREATE TABLE `test`.`dataset1`.`cov_base` (d DATE, v INT64) PARTITION BY d",
		"INSERT INTO `test`.`dataset1`.`cov_base` VALUES (DATE '2026-10-01', 1), (DATE '2026-10-02', 2), (NULL, 3)",
		"CREATE TABLE `test`.`dataset1`.`cov_plain` (id INT64)",
		"INSERT INTO `test`.`dataset1`.`cov_plain` VALUES (1)",
	} {
		if err := e.exec(t, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	rows := e.mustRows(t, "SELECT table_name, partition_id, total_rows FROM `test`.`dataset1`.INFORMATION_SCHEMA.PARTITIONS WHERE table_name LIKE 'cov_%' ORDER BY table_name, partition_id")
	want := [][]bigquery.Value{
		{"cov_base", "20261001", int64(1)}, {"cov_base", "20261002", int64(1)}, {"cov_base", "__NULL__", int64(1)},
		{"cov_cand", "20261001", int64(1)}, {"cov_plain", nil, int64(1)},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("PARTITIONS = %v, want %v", rows, want)
	}
	rows = e.mustRows(t, "SELECT b.partition_id FROM `test`.`dataset1`.INFORMATION_SCHEMA.PARTITIONS b "+
		"LEFT JOIN `test`.`dataset1`.INFORMATION_SCHEMA.PARTITIONS c ON c.table_name = 'cov_cand' AND c.partition_id = b.partition_id "+
		"WHERE b.table_name = 'cov_base' AND b.partition_id != '__NULL__' AND c.partition_id IS NULL")
	if fmt.Sprint(rows) != "[[20261002]]" {
		t.Fatalf("missing partitions = %v", rows)
	}
}
