package server_test

import (
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestCreateTableReportsPartitioningAndClustering: tables.get returns the PARTITION BY and CLUSTER BY
// of a table created with SQL. It used to return neither, so dbt-bigquery saw every partitioned
// table as unpartitioned and its "can this table be replaced in place" check failed.
func TestCreateTableReportsPartitioningAndClustering(t *testing.T) {
	e := newGapsEnv(t)
	for _, q := range []string{
		"CREATE TABLE `test`.`dataset1`.`layout_ctas` PARTITION BY d CLUSTER BY id, k AS SELECT 1 AS id, 'x' AS k, DATE '2026-01-01' AS d",
		"CREATE TABLE `test`.`dataset1`.`layout_month` (id INT64, d DATE) PARTITION BY DATE_TRUNC(d, MONTH)",
		"CREATE TABLE `test`.`dataset1`.`layout_range` (n INT64) PARTITION BY RANGE_BUCKET(n, GENERATE_ARRAY(0, 100, 10))",
		"CREATE TABLE `test`.`dataset1`.`layout_plain` (n INT64)",
	} {
		if err := e.exec(t, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	meta := func(name string) *bigquery.TableMetadata {
		t.Helper()
		md, err := e.client.Dataset("dataset1").Table(name).Metadata(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return md
	}

	md := meta("layout_ctas")
	if md.TimePartitioning == nil || md.TimePartitioning.Field != "d" || md.TimePartitioning.Type != bigquery.DayPartitioningType {
		t.Fatalf("layout_ctas timePartitioning = %+v", md.TimePartitioning)
	}
	if md.Clustering == nil || len(md.Clustering.Fields) != 2 || md.Clustering.Fields[0] != "id" || md.Clustering.Fields[1] != "k" {
		t.Fatalf("layout_ctas clustering = %+v", md.Clustering)
	}

	md = meta("layout_month")
	if md.TimePartitioning == nil || md.TimePartitioning.Field != "d" || md.TimePartitioning.Type != bigquery.MonthPartitioningType {
		t.Fatalf("layout_month timePartitioning = %+v", md.TimePartitioning)
	}
	if md.Clustering != nil {
		t.Fatalf("layout_month must not be clustered: %+v", md.Clustering)
	}

	md = meta("layout_range")
	if md.RangePartitioning == nil || md.RangePartitioning.Field != "n" ||
		md.RangePartitioning.Range.Start != 0 || md.RangePartitioning.Range.End != 100 || md.RangePartitioning.Range.Interval != 10 {
		t.Fatalf("layout_range rangePartitioning = %+v", md.RangePartitioning)
	}

	md = meta("layout_plain")
	if md.TimePartitioning != nil || md.RangePartitioning != nil || md.Clustering != nil {
		t.Fatalf("layout_plain must be unpartitioned and unclustered: %+v", md)
	}
}
