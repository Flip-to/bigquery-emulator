package metadata

import (
	"fmt"
	"testing"
)

// The recent-jobs cache is bounded by the encoded size of the jobs it holds,
// not only by their count: a run of large results used to stay pinned in
// memory for the last 4096 jobs.
func TestJobCacheByteBudget(t *testing.T) {
	c := newJobCache()
	const size = 1 << 20
	n := maxCachedJobBytes/size + 50
	for i := 0; i < n; i++ {
		c.put(NewJob(nil, "p", fmt.Sprintf("j%d", i), nil, nil, nil), size)
	}
	jobs, bytes := c.stats()
	if bytes > maxCachedJobBytes {
		t.Fatalf("cache holds %d bytes, want at most %d", bytes, maxCachedJobBytes)
	}
	if jobs != maxCachedJobBytes/size {
		t.Fatalf("cache holds %d jobs, want %d", jobs, maxCachedJobBytes/size)
	}
	if c.get("p", fmt.Sprintf("j%d", n-1)) == nil {
		t.Fatal("newest job was evicted")
	}
	if c.get("p", "j0") != nil {
		t.Fatal("oldest job is still cached")
	}

	// A job larger than the whole budget is still kept until the next one.
	c.put(NewJob(nil, "p", "huge", nil, nil, nil), 2*maxCachedJobBytes)
	if c.get("p", "huge") == nil {
		t.Fatal("oversized newest job was evicted")
	}
	c.put(NewJob(nil, "p", "after", nil, nil, nil), 10)
	if jobs, bytes := c.stats(); jobs != 1 || bytes != 10 {
		t.Fatalf("after an oversized job: %d jobs, %d bytes; want 1 job, 10 bytes", jobs, bytes)
	}

	// Replacing and removing a job keep the byte count exact.
	c.put(NewJob(nil, "p", "after", nil, nil, nil), 30)
	c.remove("p", "after")
	if jobs, bytes := c.stats(); jobs != 0 || bytes != 0 {
		t.Fatalf("after remove: %d jobs, %d bytes; want empty", jobs, bytes)
	}
}
