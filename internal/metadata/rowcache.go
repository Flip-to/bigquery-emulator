package metadata

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// rowCache keeps the result rows of the metadata SELECTs that FindProject
// runs on every HTTP request (projects, datasets, tables, models, routines).
// Each of those goes through the GoogleSQL analyzer and decodes in SQLite;
// on an emulator holding many wide tables they were most of a request's
// time, for data that only changes when metadata is written.
//
// Only raw column values are cached. Callers decode them into fresh
// objects every time, so handlers that mutate a Project or Table never
// touch the cache.
//
// Invalidation is by version: every metadata write bumps it, and so does the
// end (commit or rollback) of a transaction that wrote metadata, so rows
// read while that transaction was open, possibly uncommitted, are dropped.
// Rows are stored only if the version did not move while they were read.
//
// While any transaction that wrote metadata is open the cache is bypassed
// entirely, for reads and stores: the commit happens before the
// end-of-transaction hook bumps the version, so rows cached while the
// transaction was open could otherwise be served for a moment after its
// commit, which matters once requests run concurrently.
type rowCache struct {
	version atomic.Uint64

	mu      sync.Mutex
	entries map[string][][]any
	at      uint64 // version the entries belong to

	dirtyMu sync.Mutex
	dirty   map[*sql.Tx]struct{}
	open    atomic.Int64 // len(dirty), read without dirtyMu
}

func newRowCache() *rowCache {
	return &rowCache{entries: map[string][][]any{}, dirty: map[*sql.Tx]struct{}{}}
}

// wrote records a metadata write made in tx.
func (c *rowCache) wrote(tx *sql.Tx) {
	c.version.Add(1)
	if tx == nil {
		return
	}
	c.dirtyMu.Lock()
	if _, ok := c.dirty[tx]; !ok {
		c.dirty[tx] = struct{}{}
		c.open.Add(1)
	}
	c.dirtyMu.Unlock()
}

// txEnded invalidates again when a transaction that wrote metadata ends.
func (c *rowCache) txEnded(tx *sql.Tx) {
	c.dirtyMu.Lock()
	if _, ok := c.dirty[tx]; ok {
		delete(c.dirty, tx)
		// Bump before reopening the cache, so nothing stored before the
		// commit can be served after it.
		c.version.Add(1)
		c.open.Add(-1)
	}
	c.dirtyMu.Unlock()
}

func (c *rowCache) get(key string) ([][]any, bool) {
	if c.open.Load() > 0 {
		return nil, false
	}
	v := c.version.Load()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at != v {
		return nil, false
	}
	rows, ok := c.entries[key]
	return rows, ok
}

func (c *rowCache) put(key string, rows [][]any, readAt uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.open.Load() > 0 || c.version.Load() != readAt {
		return
	}
	if c.at != readAt {
		c.entries = map[string][][]any{}
		c.at = readAt
	}
	c.entries[key] = rows
}

// selectRows runs query in tx and returns every row as ncols raw values.
// With cache set it serves and fills the row cache; pass nil inside a
// transaction that may have written metadata, whose reads must not be shared.
func selectRows(ctx context.Context, tx *sql.Tx, cache *rowCache, ncols int, query string, args ...any) ([][]any, error) {
	var key string
	var readAt uint64
	if cache != nil {
		key = cacheKey(query, args)
		if rows, ok := cache.get(key); ok {
			return rows, nil
		}
		readAt = cache.version.Load()
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		vals := make([]any, ncols)
		dest := make([]any, ncols)
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if cache != nil {
		cache.put(key, out, readAt)
	}
	return out, nil
}

func cacheKey(query string, args []any) string {
	var b strings.Builder
	b.WriteString(query)
	for _, a := range args {
		if n, ok := a.(sql.NamedArg); ok {
			fmt.Fprintf(&b, "\x00%s=%#v", n.Name, n.Value)
			continue
		}
		fmt.Fprintf(&b, "\x00%#v", a)
	}
	return b.String()
}

func asString(v any) (string, error) {
	switch s := v.(type) {
	case string:
		return s, nil
	case []byte:
		return string(s), nil
	case nil:
		return "", nil
	}
	return "", fmt.Errorf("unexpected metadata column type %T", v)
}
