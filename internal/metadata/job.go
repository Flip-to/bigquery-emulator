package metadata

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	internaltypes "github.com/goccy/bigquery-emulator/internal/types"
	bigqueryv2 "google.golang.org/api/bigquery/v2"
)

type Job struct {
	ID        string
	ProjectID string
	content   *bigqueryv2.Job
	response  *internaltypes.QueryResponse
	err       error
	completed bool
	mu        sync.RWMutex
	repo      *Repository
	// encodedSize is the size of the stored metadata and result when the
	// job was read from the jobs table; it is charged to the jobCache.
	encodedSize int
}

// QueryFailedError is the error a finished job recorded: its query failed.
// Wait wraps it so a caller can tell it apart from a failure to look the
// job up, and report it the way BigQuery does (invalidQuery, not retried).
type QueryFailedError struct {
	Err error
}

func (e *QueryFailedError) Error() string { return e.Err.Error() }

func (e *QueryFailedError) Unwrap() error { return e.Err }

func (j *Job) Query() string {
	return j.content.Configuration.Query.Query
}

func (j *Job) QueryParameters() []*bigqueryv2.QueryParameter {
	return j.content.Configuration.Query.QueryParameters
}

func (j *Job) SetResult(ctx context.Context, tx *sql.Tx, response *internaltypes.QueryResponse, err error) error {
	j.response = response
	j.err = err
	if err := j.repo.UpdateJob(ctx, tx, j); err != nil {
		return fmt.Errorf("failed to update job: %w", err)
	}
	return nil
}

func (j *Job) Content() *bigqueryv2.Job {
	return j.content
}

func (j *Job) Wait(ctx context.Context) (*internaltypes.QueryResponse, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	// Look before the first tick: a job is usually done by the time its
	// results are asked for (jobs.query runs inline, and a jobs.insert
	// query job started in the background is often finished already).
	// Waiting a full tick first added 100 ms to every getQueryResults call.
	// A job still running is polled every jobWaitInterval as before.
	ticker := time.NewTicker(jobWaitInterval)
	defer ticker.Stop()
	for {
		foundJob, err := j.repo.FindJob(ctx, j.ProjectID, j.ID)
		if err != nil {
			return nil, err
		}
		if foundJob != nil {
			if foundJob.err != nil {
				return foundJob.response, &QueryFailedError{Err: foundJob.err}
			}
			return foundJob.response, nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// jobWaitInterval is how often Wait polls for a job that is still running.
const jobWaitInterval = 100 * time.Millisecond

func (j *Job) Cancel(ctx context.Context) error {
	// TODO: job needs to be able to rollback
	return nil
}

func (j *Job) Insert(ctx context.Context, tx *sql.Tx) error {
	return j.repo.AddJob(ctx, tx, j)
}

func (j *Job) Delete(ctx context.Context, tx *sql.Tx) error {
	return j.repo.DeleteJob(ctx, tx, j)
}

func NewJob(repo *Repository, projectID, jobID string, content *bigqueryv2.Job, response *internaltypes.QueryResponse, err error) *Job {
	return &Job{
		ID:        jobID,
		ProjectID: projectID,
		content:   content,
		response:  response,
		err:       err,
		repo:      repo,
	}
}
