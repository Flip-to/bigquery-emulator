package metadata

import (
	"database/sql"
	"testing"
)

func TestRowCacheInvalidation(t *testing.T) {
	c := newRowCache()
	rows := [][]any{{"a"}}

	c.put("k", rows, c.version.Load())
	if _, ok := c.get("k"); !ok {
		t.Fatal("fresh entry not served")
	}

	// A metadata write drops every entry.
	tx := new(sql.Tx)
	c.wrote(tx)
	if _, ok := c.get("k"); ok {
		t.Fatal("entry served after a metadata write")
	}

	// Rows read while the writing transaction is open may be uncommitted:
	// the end of that transaction (commit or rollback) drops them too.
	c.put("k", rows, c.version.Load())
	if _, ok := c.get("k"); !ok {
		t.Fatal("entry stored during the write tx not served")
	}
	c.txEnded(tx)
	if _, ok := c.get("k"); ok {
		t.Fatal("entry read during a write tx served after that tx ended")
	}

	// Ending a transaction that wrote nothing keeps the cache.
	c.put("k", rows, c.version.Load())
	c.txEnded(new(sql.Tx))
	if _, ok := c.get("k"); !ok {
		t.Fatal("cache dropped by a tx that wrote no metadata")
	}

	// Rows whose read overlapped a write are not stored.
	readAt := c.version.Load()
	c.wrote(nil)
	c.put("k2", rows, readAt)
	if _, ok := c.get("k2"); ok {
		t.Fatal("rows read before a concurrent write were stored")
	}
}
