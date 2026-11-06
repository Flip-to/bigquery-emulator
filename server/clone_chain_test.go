package server_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const cloneChainLimitMessage = "Cannot have more than 3 chained clones or snapshots. Please use a deep copy to reset the chain."

// TestCloneChains: the BigQuery clone chain behaviours dbt-virtual depends on. A clone is table_type
// CLONE in INFORMATION_SCHEMA.TABLES with base_table_*, tables.get gives it a cloneDefinition (type stays
// TABLE), a 4th chained clone fails with BigQuery's message, and CREATE TABLE ... COPY is a deep copy
// (BASE TABLE, no cloneDefinition) that resets the chain.
func TestCloneChains(t *testing.T) {
	e := newGapsEnv(t)
	for _, q := range []string{
		"CREATE SCHEMA `test`.`cc`",
		"CREATE TABLE `test`.`cc`.`t0` (id INT64, d DATE) PARTITION BY d CLUSTER BY id",
		"INSERT INTO `test`.`cc`.`t0` (id, d) VALUES (1, DATE '2026-10-01')",
		"CREATE TABLE `test`.`cc`.`t1` CLONE `test`.`cc`.`t0`",
		"CREATE TABLE `test`.`cc`.`t2` CLONE `test`.`cc`.`t1`",
		"CREATE TABLE `test`.`cc`.`t3` CLONE `test`.`cc`.`t2`",
	} {
		if err := e.exec(t, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// INFORMATION_SCHEMA.TABLES
	got := fmt.Sprint(e.mustRows(t, "SELECT table_name, table_type, base_table_catalog, base_table_schema, base_table_name "+
		"FROM `test`.`cc`.INFORMATION_SCHEMA.TABLES ORDER BY table_name"))
	want := "[[t0 BASE TABLE <nil> <nil> <nil>] [t1 CLONE test cc t0] [t2 CLONE test cc t1] [t3 CLONE test cc t2]]"
	if got != want {
		t.Fatalf("INFORMATION_SCHEMA.TABLES = %s, want %s", got, want)
	}

	// tables.get
	_, t1 := httpJSON(t, http.MethodGet, e.url+"/bigquery/v2/projects/test/datasets/cc/tables/t1", "", nil)
	def, _ := t1["cloneDefinition"].(map[string]any)
	ref, _ := def["baseTableReference"].(map[string]any)
	if t1["type"] != "TABLE" || ref["projectId"] != "test" || ref["datasetId"] != "cc" || ref["tableId"] != "t0" || def["cloneTime"] == nil {
		t.Fatalf("tables.get of a clone: type=%v cloneDefinition=%v", t1["type"], t1["cloneDefinition"])
	}
	_, t0 := httpJSON(t, http.MethodGet, e.url+"/bigquery/v2/projects/test/datasets/cc/tables/t0", "", nil)
	if t0["cloneDefinition"] != nil {
		t.Fatalf("a base table has no cloneDefinition: %v", t0["cloneDefinition"])
	}

	// the 4th chained clone
	err := e.exec(t, "CREATE TABLE `test`.`cc`.`t4` CLONE `test`.`cc`.`t3`")
	if err == nil || !strings.Contains(err.Error(), cloneChainLimitMessage) {
		t.Fatalf("4th chained clone: err = %v, want %q", err, cloneChainLimitMessage)
	}
	if got := fmt.Sprint(e.mustRows(t, "SELECT COUNT(*) FROM `test`.`cc`.INFORMATION_SCHEMA.TABLES WHERE table_name = 't4'")); got != "[[0]]" {
		t.Fatalf("the refused clone exists: %s", got)
	}

	// COPY: a deep copy keeping the layout, a BASE TABLE, resets the chain
	for _, q := range []string{
		"CREATE TABLE `test`.`cc`.`t4c` COPY `test`.`cc`.`t3`",
		"CREATE TABLE IF NOT EXISTS `test`.`cc`.`t4c` COPY `test`.`cc`.`t3`",
		"CREATE TABLE `test`.`cc`.`t5` CLONE `test`.`cc`.`t4c`",
	} {
		if err := e.exec(t, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	got = fmt.Sprint(e.mustRows(t, "SELECT table_name, table_type, base_table_name FROM `test`.`cc`.INFORMATION_SCHEMA.TABLES WHERE table_name IN ('t4c', 't5') ORDER BY table_name"))
	if got != "[[t4c BASE TABLE <nil>] [t5 CLONE t4c]]" {
		t.Fatalf("after COPY: %s", got)
	}
	if got := fmt.Sprint(e.mustRows(t, "SELECT COUNT(*), SUM(id) FROM `test`.`cc`.`t4c`")); got != "[[1 1]]" {
		t.Fatalf("copy rows = %s", got)
	}
	_, t4c := httpJSON(t, http.MethodGet, e.url+"/bigquery/v2/projects/test/datasets/cc/tables/t4c", "", nil)
	if t4c["cloneDefinition"] != nil || t4c["timePartitioning"] == nil || t4c["clustering"] == nil {
		t.Fatalf("tables.get of a copy must keep partitioning and clustering and have no cloneDefinition: %v", t4c)
	}
}
