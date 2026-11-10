package server_test

import (
	"fmt"
	"strings"
	"testing"
)

// cloneBackstopScript is the clone script dbt-virtual sends to BigQuery: CLONE, and on the chain-limit
// error a COPY, any other error raised again.
func cloneBackstopScript(target, source string) string {
	return fmt.Sprintf("BEGIN\n  CREATE TABLE IF NOT EXISTS `test`.`sx`.`%[1]s`\n  CLONE `test`.`sx`.`%[2]s`;\n"+
		"EXCEPTION WHEN ERROR THEN\n"+
		"  IF CONTAINS_SUBSTR(@@error.message, 'chained clones') THEN\n"+
		"    CREATE TABLE IF NOT EXISTS `test`.`sx`.`%[1]s` COPY `test`.`sx`.`%[2]s`;\n"+
		"  ELSE\n    RAISE USING MESSAGE = @@error.message;\n  END IF;\nEND", target, source)
}

// TestScriptExceptionHandler: BEGIN ... EXCEPTION WHEN ERROR THEN ... END runs its handler with
// @@error.message set, IF / RAISE run inside it, and the rows of the handler's last SELECT come back,
// through jobs.query as dbt-bigquery sends it (Flip-to/googlesqlite#41).
func TestScriptExceptionHandler(t *testing.T) {
	e := newGapsEnv(t)
	for _, q := range []string{
		"CREATE SCHEMA `test`.`sx`",
		"CREATE TABLE `test`.`sx`.`t0` (id INT64)",
		"INSERT INTO `test`.`sx`.`t0` (id) VALUES (5)",
		"CREATE TABLE `test`.`sx`.`t1` CLONE `test`.`sx`.`t0`",
		"CREATE TABLE `test`.`sx`.`t2` CLONE `test`.`sx`.`t1`",
		"CREATE TABLE `test`.`sx`.`t3` CLONE `test`.`sx`.`t2`",
	} {
		if err := e.exec(t, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// The body runs: a clone below the chain limit stays a clone.
	if err := e.exec(t, cloneBackstopScript("c1", "t0")); err != nil {
		t.Fatalf("clone script: %v", err)
	}
	got := fmt.Sprint(e.mustRows(t, "SELECT table_type, base_table_name FROM `test`.`sx`.INFORMATION_SCHEMA.TABLES WHERE table_name = 'c1'"))
	if got != "[[CLONE t0]]" {
		t.Fatalf("c1 = %s, want a clone of t0", got)
	}

	// The 4th chained clone fails in the body; the handler copies.
	if err := e.exec(t, cloneBackstopScript("c4", "t3")); err != nil {
		t.Fatalf("clone script at the chain limit: %v", err)
	}
	got = fmt.Sprint(e.mustRows(t, "SELECT table_type, base_table_name FROM `test`.`sx`.INFORMATION_SCHEMA.TABLES WHERE table_name = 'c4'"))
	if got != "[[BASE TABLE <nil>]]" {
		t.Fatalf("c4 = %s, want a deep copy", got)
	}
	if got := fmt.Sprint(e.mustRows(t, "SELECT id FROM `test`.`sx`.`c4`")); got != "[[5]]" {
		t.Fatalf("c4 rows = %s", got)
	}

	// Any other error is raised again with its own message and nothing is created.
	err := e.exec(t, cloneBackstopScript("c5", "no_such_table"))
	if err == nil || !strings.Contains(err.Error(), "no_such_table") {
		t.Fatalf("clone of a missing table: err = %v, want the original error", err)
	}
	if got := fmt.Sprint(e.mustRows(t, "SELECT COUNT(*) FROM `test`.`sx`.INFORMATION_SCHEMA.TABLES WHERE table_name = 'c5'")); got != "[[0]]" {
		t.Fatalf("a failed clone left a table: %s", got)
	}

	// RAISE USING MESSAGE in the body reaches the handler; its last SELECT is the result.
	rows := e.mustRows(t, "BEGIN RAISE USING MESSAGE = 'boom'; EXCEPTION WHEN ERROR THEN SELECT @@error.message AS m; END")
	if got := fmt.Sprint(rows); got != "[[boom]]" {
		t.Fatalf("handler rows = %s, want [[boom]]", got)
	}

	// A handler's own RAISE is the error the client sees.
	err = e.exec(t, "BEGIN RAISE USING MESSAGE = 'first'; EXCEPTION WHEN ERROR THEN RAISE USING MESSAGE = CONCAT('second after ', @@error.message); END")
	if err == nil || !strings.Contains(err.Error(), "second after first") {
		t.Fatalf("handler RAISE: err = %v", err)
	}
}
