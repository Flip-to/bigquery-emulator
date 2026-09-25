package metadata

import (
	"container/list"
	"database/sql"
	"sync"
)

// maxCachedJobs and maxCachedJobBytes bound the recent-jobs cache, by count
// and by the encoded size of the cached jobs (metadata plus result), so a
// run of large results cannot pin gigabytes of rows in memory. Older jobs
// stay in the jobs table and are still found there (jobs.get,
// getQueryResults, jobs.list keep working, as BigQuery keeps results for
// about a day), only more slowly.
const (
	maxCachedJobs     = 4096
	maxCachedJobBytes = 64 << 20
)

// jobCache keeps the most recently written or read jobs by (projectID, id).
//
// A job lookup by ID cannot use the jobs table's primary key: googlesqlite
// compiles `=` to a function call, so SQLite scans the whole table (whose
// rows also hold each job's full result). With the job history growing by one
// row per query, every jobs.query / jobs.get / getQueryResults became
// O(jobs served so far). Jobs are written through this cache, so recent jobs
// are found without touching the table.
//
// A job written inside a transaction is staged and only enters the cache
// when that transaction commits (commit); a rollback drops it (discard).
// Otherwise a job whose transaction failed would stay visible, and the
// client's retry with the same job ID would be rejected as a duplicate.
type jobCache struct {
	mu      sync.Mutex
	order   *list.List
	items   map[jobKey]*list.Element
	pending map[*sql.Tx][]cachedJob
	bytes   int
}

type jobKey struct {
	projectID string
	jobID     string
}

type cachedJob struct {
	key  jobKey
	job  *Job
	size int
}

func newJobCache() *jobCache {
	return &jobCache{
		order:   list.New(),
		items:   map[jobKey]*list.Element{},
		pending: map[*sql.Tx][]cachedJob{},
	}
}

// get returns a fresh Job carrying the cached state, as a table read would.
func (c *jobCache) get(projectID, jobID string) *Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.items[jobKey{projectID, jobID}]
	if !ok {
		return nil
	}
	c.order.MoveToFront(elem)
	j := elem.Value.(*cachedJob).job
	return NewJob(j.repo, j.ProjectID, j.ID, j.content, j.response, j.err)
}

// stage records a job written in tx; it becomes visible on commit. size is
// the job's encoded size (metadata plus result), charged to the byte budget.
func (c *jobCache) stage(tx *sql.Tx, j *Job, size int) {
	snapshot := NewJob(j.repo, j.ProjectID, j.ID, j.content, j.response, j.err)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending[tx] = append(c.pending[tx], cachedJob{key: jobKey{j.ProjectID, j.ID}, job: snapshot, size: size})
}

// commit publishes the jobs staged in tx, in write order.
func (c *jobCache) commit(tx *sql.Tx) {
	c.mu.Lock()
	defer c.mu.Unlock()
	jobs := c.pending[tx]
	delete(c.pending, tx)
	for _, j := range jobs {
		c.putLocked(j.job, j.size)
	}
}

// discard drops the jobs staged in tx.
func (c *jobCache) discard(tx *sql.Tx) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, tx)
}

// put caches a job read from the table (already committed).
func (c *jobCache) put(j *Job, size int) {
	snapshot := NewJob(j.repo, j.ProjectID, j.ID, j.content, j.response, j.err)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(snapshot, size)
}

func (c *jobCache) putLocked(snapshot *Job, size int) {
	key := jobKey{snapshot.ProjectID, snapshot.ID}
	if elem, ok := c.items[key]; ok {
		entry := elem.Value.(*cachedJob)
		c.bytes += size - entry.size
		entry.job, entry.size = snapshot, size
		c.order.MoveToFront(elem)
	} else {
		c.items[key] = c.order.PushFront(&cachedJob{key: key, job: snapshot, size: size})
		c.bytes += size
	}
	// Evict the least recently used jobs; the newest one always stays, so a
	// job whose result alone exceeds the budget is still served from memory
	// until the next job arrives.
	for c.order.Len() > 1 && (c.order.Len() > maxCachedJobs || c.bytes > maxCachedJobBytes) {
		c.removeLocked(c.order.Back())
	}
}

func (c *jobCache) removeLocked(elem *list.Element) {
	entry := elem.Value.(*cachedJob)
	c.order.Remove(elem)
	delete(c.items, entry.key)
	c.bytes -= entry.size
}

// stats reports the number of cached jobs and their encoded size.
func (c *jobCache) stats() (jobs, bytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len(), c.bytes
}

func (c *jobCache) remove(projectID, jobID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.items[jobKey{projectID, jobID}]; ok {
		c.removeLocked(elem)
	}
}
