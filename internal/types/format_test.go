package types

import (
	"testing"

	bigqueryv2 "google.golang.org/api/bigquery/v2"
)

// TestFormatNestedTimestamps checks that useInt64Timestamp applies to
// TIMESTAMP values nested in STRUCT and ARRAY, not only to top-level
// columns. The Python client decodes nested TIMESTAMP values with the same
// int64 parser, so a seconds-since-epoch string there made it fail with
// "invalid literal for int() with base 10" (flipto-dbt probe L4).
func TestFormatNestedTimestamps(t *testing.T) {
	ts := &bigqueryv2.TableFieldSchema{Name: "v", Type: "TIMESTAMP", Mode: "NULLABLE"}
	schema := &bigqueryv2.TableSchema{Fields: []*bigqueryv2.TableFieldSchema{
		{Name: "s", Type: "RECORD", Mode: "NULLABLE", Fields: []*bigqueryv2.TableFieldSchema{ts}},
		{Name: "a", Type: "TIMESTAMP", Mode: "REPEATED"},
		{Name: "t", Type: "TIMESTAMP", Mode: "NULLABLE"},
	}}
	rows := []*TableRow{{F: []*TableCell{
		{V: TableRow{F: []*TableCell{{V: "1709164800.000000", Name: "v"}}}},
		{V: []*TableCell{{V: "1709164800.000001"}, {V: nil}}},
		{V: "1709164800.000000"},
	}}}
	got := Format(schema, rows, true)[0].F
	if v := got[0].V.(TableRow).F[0].V; v != "1709164800000000" {
		t.Errorf("struct field = %v, want 1709164800000000", v)
	}
	if name := got[0].V.(TableRow).F[0].Name; name != "v" {
		t.Errorf("struct field name = %q, want v", name)
	}
	arr := got[1].V.([]*TableCell)
	if arr[0].V != "1709164800000001" || arr[1].V != nil {
		t.Errorf("array = [%v %v], want [1709164800000001 <nil>]", arr[0].V, arr[1].V)
	}
	if got[2].V != "1709164800000000" {
		t.Errorf("top level = %v, want 1709164800000000", got[2].V)
	}
	// Without useInt64Timestamp the float-seconds form is kept.
	if v := Format(schema, rows, false)[0].F[0].V.(TableRow).F[0].V; v != "1709164800.000000" {
		t.Errorf("struct field (float form) = %v, want 1709164800.000000", v)
	}
}
