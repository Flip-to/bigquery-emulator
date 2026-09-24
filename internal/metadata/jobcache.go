package metadata

import (
	"container/list"
	"database/sql"
	"sync"
)

// maxCachedJobs bounds the recent-jobs cache. Older jobs stay in the jobs
// table and are still found there, only more slowly.
const maxCachedJobs = 4096

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
	pending map[*sql.Tx][]*Job
}

type jobKey struct {
	projectID string
	jobID     string
}

type cachedJob struct {
	key jobKey
	job *Job
}

func newJobCache() *jobCache {
	return &jobCache{
		order:   list.New(),
		items:   map[jobKey]*list.Element{},
		pending: map[*sql.Tx][]*Job{},
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

// stage records a job written in tx; it becomes visible on commit.
func (c *jobCache) stage(tx *sql.Tx, j *Job) {
	snapshot := NewJob(j.repo, j.ProjectID, j.ID, j.content, j.response, j.err)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending[tx] = append(c.pending[tx], snapshot)
}

// commit publishes the jobs staged in tx, in write order.
func (c *jobCache) commit(tx *sql.Tx) {
	c.mu.Lock()
	defer c.mu.Unlock()
	jobs := c.pending[tx]
	delete(c.pending, tx)
	for _, j := range jobs {
		c.putLocked(j)
	}
}

// discard drops the jobs staged in tx.
func (c *jobCache) discard(tx *sql.Tx) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, tx)
}

// put caches a job read from the table (already committed).
func (c *jobCache) put(j *Job) {
	snapshot := NewJob(j.repo, j.ProjectID, j.ID, j.content, j.response, j.err)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(snapshot)
}

func (c *jobCache) putLocked(snapshot *Job) {
	key := jobKey{snapshot.ProjectID, snapshot.ID}
	if elem, ok := c.items[key]; ok {
		elem.Value.(*cachedJob).job = snapshot
		c.order.MoveToFront(elem)
		return
	}
	c.items[key] = c.order.PushFront(&cachedJob{key: key, job: snapshot})
	for c.order.Len() > maxCachedJobs {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*cachedJob).key)
	}
}

func (c *jobCache) remove(projectID, jobID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := jobKey{projectID, jobID}
	if elem, ok := c.items[key]; ok {
		c.order.Remove(elem)
		delete(c.items, key)
	}
}
