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

	// While the writing transaction is open the cache is bypassed: nothing
	// is stored or served, so nothing read before its commit survives it.
	c.put("k", rows, c.version.Load())
	if _, ok := c.get("k"); ok {
		t.Fatal("cache served while a metadata write tx is open")
	}
	c.txEnded(tx)
	if _, ok := c.get("k"); ok {
		t.Fatal("entry offered during a write tx served after that tx ended")
	}
	c.put("k", rows, c.version.Load())
	if _, ok := c.get("k"); !ok {
		t.Fatal("cache not reopened after the write tx ended")
	}
	// A second write in the same tx is counted once.
	tx2 := new(sql.Tx)
	c.wrote(tx2)
	c.wrote(tx2)
	c.txEnded(tx2)
	c.put("k", rows, c.version.Load())
	if _, ok := c.get("k"); !ok {
		t.Fatal("cache still bypassed after a tx that wrote twice ended")
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
