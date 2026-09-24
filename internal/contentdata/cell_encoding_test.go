package contentdata

import (
	"encoding/json"
	"testing"

	bigqueryv2 "google.golang.org/api/bigquery/v2"
)

func cellJSON(t *testing.T, value interface{}, schema *bigqueryv2.TableFieldSchema) string {
	t.Helper()
	cell, err := NewRepository().convertValueToCell(value, schema)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(cell)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TIMESTAMP values use the epoch-seconds wire format at every depth, the
// same encoding BigQuery uses for top-level TIMESTAMP columns.
func TestConvertValueToCellNestedTimestamp(t *testing.T) {
	ts := &bigqueryv2.TableFieldSchema{Name: "v", Type: "TIMESTAMP", Mode: "NULLABLE"}
	rec := &bigqueryv2.TableFieldSchema{Type: "RECORD", Mode: "NULLABLE", Fields: []*bigqueryv2.TableFieldSchema{ts}}
	if got, want := cellJSON(t, []interface{}{"2024-02-29 00:00:00+00"}, rec), `{"v":{"f":[{"v":"1709164800.000000"}]}}`; got != want {
		t.Errorf("struct: got %s, want %s", got, want)
	}
	arr := &bigqueryv2.TableFieldSchema{Type: "TIMESTAMP", Mode: "REPEATED"}
	if got, want := cellJSON(t, []interface{}{"2024-02-29 00:00:00.500000+00", "1969-12-31 23:59:59.5+00"}, arr), `{"v":[{"v":"1709164800.500000"},{"v":"-1.500000"}]}`; got != want {
		t.Errorf("array: got %s, want %s", got, want)
	}
	if got, want := cellJSON(t, "1709164800.000000", ts), `{"v":"1709164800.000000"}`; got != want {
		t.Errorf("top-level: got %s, want %s", got, want)
	}
}

// BigQuery never returns a NULL array: it is sent as [] at any depth.
func TestConvertValueToCellNullArray(t *testing.T) {
	arr := &bigqueryv2.TableFieldSchema{Name: "v", Type: "STRING", Mode: "REPEATED"}
	rec := &bigqueryv2.TableFieldSchema{Type: "RECORD", Mode: "NULLABLE", Fields: []*bigqueryv2.TableFieldSchema{arr}}
	if got, want := cellJSON(t, []interface{}{nil}, rec), `{"v":{"f":[{"v":[]}]}}`; got != want {
		t.Errorf("struct: got %s, want %s", got, want)
	}
	if got, want := cellJSON(t, nil, arr), `{"v":[]}`; got != want {
		t.Errorf("top-level: got %s, want %s", got, want)
	}
	// A NULL STRUCT stays NULL.
	if got, want := cellJSON(t, nil, rec), `{"v":null}`; got != want {
		t.Errorf("null struct: got %s, want %s", got, want)
	}
}

// A NULL element in a result array is an error in BigQuery:
// SELECT ARRAY_AGG(x) AS c0 FROM UNNEST([1, NULL]) AS x fails with
// "Array cannot have a null element; error in writing field c0".
func TestConvertValueToCellNullArrayElement(t *testing.T) {
	arr := &bigqueryv2.TableFieldSchema{Name: "c0", Type: "INTEGER", Mode: "REPEATED"}
	_, err := NewRepository().convertValueToCell([]interface{}{int64(1), nil}, arr)
	if err == nil || err.Error() != "Array cannot have a null element; error in writing field c0" {
		t.Fatalf("err = %v, want BigQuery's null element error", err)
	}
	if got, want := cellJSON(t, []interface{}{int64(1), int64(2)}, arr), `{"v":[{"v":"1"},{"v":"2"}]}`; got != want {
		t.Errorf("array: got %s, want %s", got, want)
	}
}
