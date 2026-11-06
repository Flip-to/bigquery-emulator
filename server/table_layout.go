package server

import (
	"time"

	"github.com/goccy/googlesqlite"
	bigqueryv2 "google.golang.org/api/bigquery/v2"
)

// applyTableLayout records the PARTITION BY and CLUSTER BY of a CREATE TABLE statement as table
// metadata (timePartitioning, rangePartitioning, clustering), as BigQuery's tables.get reports them.
// dbt-bigquery reads them to decide whether it may replace a table in place.
func applyTableLayout(table *bigqueryv2.Table, spec *googlesqlite.TableSpec) {
	if p := spec.Partitioning(); p != nil {
		if p.Range {
			table.RangePartitioning = &bigqueryv2.RangePartitioning{
				Field: p.Field,
				Range: &bigqueryv2.RangePartitioningRange{
					Start:    p.RangeStart,
					End:      p.RangeEnd,
					Interval: p.RangeInterval,
				},
			}
		} else {
			table.TimePartitioning = &bigqueryv2.TimePartitioning{Type: p.Type, Field: p.Field}
		}
	}
	if len(spec.ClusterBy) > 0 {
		table.Clustering = &bigqueryv2.Clustering{Fields: append([]string(nil), spec.ClusterBy...)}
	}
}

// applyCloneDefinition records the source of a CREATE TABLE ... CLONE as the table's cloneDefinition, as
// BigQuery's tables.get reports it (the table itself keeps type TABLE). A deep copy (COPY) has none.
func applyCloneDefinition(table *bigqueryv2.Table, spec *googlesqlite.TableSpec) {
	if len(spec.CloneOf) != 3 {
		return
	}
	def := &bigqueryv2.CloneDefinition{
		BaseTableReference: &bigqueryv2.TableReference{
			ProjectId: spec.CloneOf[0],
			DatasetId: spec.CloneOf[1],
			TableId:   spec.CloneOf[2],
		},
	}
	if !spec.CloneTime.IsZero() {
		def.CloneTime = spec.CloneTime.UTC().Format(time.RFC3339Nano)
	}
	table.CloneDefinition = def
}
