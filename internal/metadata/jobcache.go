package metadata

import (
	"container/list"
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
type jobCache struct {
	mu    sync.Mutex
	order *list.List
	items map[jobKey]*list.Element
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
	return &jobCache{order: list.New(), items: map[jobKey]*list.Element{}}
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

func (c *jobCache) put(j *Job) {
	snapshot := NewJob(j.repo, j.ProjectID, j.ID, j.content, j.response, j.err)
	key := jobKey{j.ProjectID, j.ID}
	c.mu.Lock()
	defer c.mu.Unlock()
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
