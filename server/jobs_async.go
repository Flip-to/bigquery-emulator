package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	bigqueryv2 "google.golang.org/api/bigquery/v2"

	"github.com/goccy/bigquery-emulator/internal/logger"
)

// Query jobs created by jobs.insert run in the background, as in BigQuery:
// the call returns the job in state RUNNING when the query outlives
// jobInsertWait, jobs.get reports it while it runs, jobs.getQueryResults
// waits for it, and inserting the same job ID again is a 409 duplicate.
// Without this a client whose jobs.insert timed out retried it with the same
// job ID and failed with "job is already created" once the first run ended.

// jobInsertWait is how long jobs.insert waits for a query job before
// answering with the job still RUNNING. It is a variable so tests can
// shrink it.
var jobInsertWait = 5 * time.Second

const (
	// defaultQueryResultsTimeout and maxQueryResultsTimeout bound how long
	// jobs.getQueryResults waits for a running job (its timeoutMs
	// parameter) before answering jobComplete=false.
	defaultQueryResultsTimeout = 10 * time.Second
	maxQueryResultsTimeout     = 30 * time.Second
)

type runningJob struct {
	// job is the RUNNING representation jobs.insert and jobs.get return.
	job  *bigqueryv2.Job
	done chan struct{}
	// result and err are set before done is closed.
	result *bigqueryv2.Job
	err    error
}

type runningJobs struct {
	mu   sync.Mutex
	jobs map[string]*runningJob
}

func runningJobKey(projectID, jobID string) string {
	return projectID + "/" + jobID
}

func (r *runningJobs) get(projectID, jobID string) *runningJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[runningJobKey(projectID, jobID)]
}

// add registers a job and reports whether it did; when the ID is in use it
// returns false.
func (r *runningJobs) add(projectID, jobID string, job *bigqueryv2.Job) (*runningJob, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := runningJobKey(projectID, jobID)
	if existing, ok := r.jobs[key]; ok {
		return existing, false
	}
	if r.jobs == nil {
		r.jobs = map[string]*runningJob{}
	}
	rj := &runningJob{job: job, done: make(chan struct{})}
	r.jobs[key] = rj
	return rj, true
}

func (r *runningJobs) finish(projectID, jobID string, rj *runningJob, result *bigqueryv2.Job, err error) {
	r.mu.Lock()
	rj.result, rj.err = result, err
	delete(r.jobs, runningJobKey(projectID, jobID))
	r.mu.Unlock()
	close(rj.done)
}

func jobLocation(job *bigqueryv2.Job) string {
	if job.JobReference != nil && job.JobReference.Location != "" {
		return job.JobReference.Location
	}
	return "US"
}

// errJobAlreadyExists is the answer of BigQuery to a jobs.insert whose job
// ID is in use. Clients treat the 409 as "the job exists" and fetch it.
func errJobAlreadyExists(projectID, location, jobID string) *ServerError {
	return errDuplicate(fmt.Sprintf("Already Exists: Job %s:%s.%s", projectID, location, jobID))
}

// newRunningJob is the job as BigQuery reports it while its query runs.
func newRunningJob(job *bigqueryv2.Job, projectID, selfAddr string) *bigqueryv2.Job {
	content := *job
	content.Kind = "bigquery#job"
	content.Status = &bigqueryv2.JobStatus{State: "RUNNING"}
	content.JobReference = &bigqueryv2.JobReference{
		ProjectId: projectID,
		JobId:     job.JobReference.JobId,
		Location:  job.JobReference.Location,
	}
	content.SelfLink = fmt.Sprintf("http://%s/bigquery/v2/projects/%s/jobs/%s", selfAddr, projectID, job.JobReference.JobId)
	if job.Configuration != nil {
		cfg := *job.Configuration
		cfg.JobType = "QUERY"
		content.Configuration = &cfg
	}
	return &content
}

func (s *Server) listenAddr() string {
	if s.httpServer != nil {
		return s.httpServer.Addr
	}
	return ""
}

// startQueryJob runs the query job in the background and answers jobs.insert:
// with the finished job when it ends within jobInsertWait, otherwise with the
// job RUNNING. The caller holds the request lock; it is released here, and
// the background run takes it for the time the query runs.
func (h *jobsInsertHandler) startQueryJob(ctx context.Context, w http.ResponseWriter, r *jobsInsertRequest) {
	job := r.job
	if job.JobReference == nil {
		job.JobReference = &bigqueryv2.JobReference{}
	}
	if job.JobReference.JobId == "" {
		job.JobReference.JobId = randomID() // generate job id
	}
	jobID := job.JobReference.JobId
	projectID := r.project.ID
	rj, added := r.server.running.add(projectID, jobID, newRunningJob(job, projectID, r.server.listenAddr()))
	if !added {
		releaseSequentialAccess(ctx)
		errorResponse(ctx, w, errJobAlreadyExists(projectID, jobLocation(job), jobID))
		return
	}
	// The run outlives the request, so it needs a context of its own. It is
	// cancelled when the run ends, as the request context was when the job
	// ran inside the request: that ends any transaction or connection the
	// statement left open.
	bgCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		var (
			result *bigqueryv2.Job
			err    error
		)
		// The run takes the request lock itself: shared for a read-only
		// SELECT, exclusive otherwise. Handle finds it in bgCtx and, for a
		// shared run, records the job as reopenForWrite describes.
		g := &accessGuard{mu: &r.server.accessMu}
		if r.server.walMode && sharedJob(job) {
			g.acquire(accessShared)
		} else {
			g.acquire(accessExclusive)
		}
		bgCtx := context.WithValue(bgCtx, accessGuardKey{}, g)
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("job %s panicked: %v", jobID, rec)
			}
			cancel()
			r.server.running.finish(projectID, jobID, rj, result, err)
			g.release()
		}()
		// An ID that is in the jobs table already (a retry after the
		// original finished) is a duplicate; running it again would run
		// the statement a second time.
		if existing, ferr := r.server.metaRepo.FindJob(bgCtx, projectID, jobID); ferr != nil {
			err = ferr
			return
		} else if existing != nil {
			err = errJobAlreadyExists(projectID, jobLocation(job), jobID)
			return
		}
		// r.project was read when the request arrived, before other requests
		// ran. Handle writes the project back (datasets, jobs), so work on a
		// fresh copy: a stale one would drop what they added.
		fresh, ferr := r.server.metaRepo.FindProject(bgCtx, projectID)
		if ferr != nil {
			err = ferr
			return
		}
		if fresh == nil {
			err = errNotFound(fmt.Sprintf("project %s is not found", projectID))
			return
		}
		result, err = h.Handle(bgCtx, &jobsInsertRequest{server: r.server, project: fresh, job: job})
	}()
	releaseSequentialAccess(ctx)

	timer := time.NewTimer(jobInsertWait)
	defer timer.Stop()
	select {
	case <-rj.done:
		writeJobInsertResult(ctx, w, rj.result, rj.err)
	case <-timer.C:
		encodeResponse(ctx, w, rj.job)
	case <-ctx.Done():
	}
}

func writeJobInsertResult(ctx context.Context, w http.ResponseWriter, res *bigqueryv2.Job, err error) {
	if err != nil {
		// Preserve typed *ServerError (e.g. a 404 notFound for a missing
		// destination table under CREATE_NEVER) so the client sees the
		// real HTTP status rather than a blanket 400 jobInternalError.
		var serr *ServerError
		if errors.As(err, &serr) {
			errorResponse(ctx, w, serr)
			return
		}
		errorResponse(ctx, w, errJobInternalError(err.Error()))
		return
	}
	encodeResponse(ctx, w, res)
}

// runningJobMiddleware answers requests about a job that is still running
// without waiting for the sequential-access lock, which the job's own query
// holds: jobs.get, jobs.getQueryResults, and a jobs.insert that reuses the
// job's ID. Every other request, and those about finished jobs, pass through.
func runningJobMiddleware(s *Server) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// This runs before loggerMiddleware, which errorResponse needs.
			r = r.WithContext(logger.WithLogger(r.Context(), s.logger))
			params := mux.Vars(r)
			projectID, hasProject := projectIDFromParams(params)
			if !hasProject {
				next.ServeHTTP(w, r)
				return
			}
			ctx := r.Context()
			if jobID, ok := jobIDFromParams(params); ok && r.Method == http.MethodGet {
				rj := s.running.get(projectID, jobID)
				if rj == nil {
					next.ServeHTTP(w, r)
					return
				}
				if !strings.Contains(r.URL.Path, "/queries/") {
					encodeResponse(ctx, w, rj.job) // jobs.get
					return
				}
				// jobs.getQueryResults: wait for the job, up to timeoutMs.
				timeout := defaultQueryResultsTimeout
				if ms, err := strconv.ParseInt(r.URL.Query().Get("timeoutMs"), 10, 64); err == nil && ms >= 0 {
					timeout = time.Duration(ms) * time.Millisecond
				}
				if timeout > maxQueryResultsTimeout {
					timeout = maxQueryResultsTimeout
				}
				timer := time.NewTimer(timeout)
				defer timer.Stop()
				select {
				case <-rj.done:
					next.ServeHTTP(w, r)
				case <-timer.C:
					encodeResponse(ctx, w, map[string]interface{}{
						"kind":         "bigquery#getQueryResultsResponse",
						"jobReference": rj.job.JobReference,
						"jobComplete":  false,
					})
				case <-ctx.Done():
				}
				return
			}
			if r.Method == http.MethodPost && isJobsInsertPath(r) {
				body, err := readRequestBody(r)
				if err != nil {
					errorResponse(ctx, w, errInvalid(err.Error()))
					return
				}
				var job struct {
					JobReference struct {
						JobID string `json:"jobId"`
					} `json:"jobReference"`
				}
				if json.Unmarshal(body, &job) == nil && job.JobReference.JobID != "" {
					if s.running.get(projectID, job.JobReference.JobID) != nil {
						errorResponse(ctx, w, errJobAlreadyExists(projectID, "US", job.JobReference.JobID))
						return
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isJobsInsertPath reports whether r is POST .../projects/{p}/jobs (not the
// upload variant).
func isJobsInsertPath(r *http.Request) bool {
	path := strings.TrimSuffix(r.URL.Path, "/")
	return strings.HasSuffix(path, "/jobs") && !strings.Contains(path, "/upload/")
}

// readRequestBody reads the whole body, gunzipping it, and replaces it with
// the decoded bytes so the handler reads the same content. Reading it before
// the request waits for the sequential-access lock also keeps a queued
// request from timing out while the client's upload sits unread.
func readRequestBody(r *http.Request) ([]byte, error) {
	var reader io.Reader = r.Body
	if r.Header.Get(contentEncoding) == encodingTypeGzip {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to decode gzip content: %s", err)
		}
		defer gz.Close()
		reader = gz
		r.Header.Del(contentEncoding)
	}
	body, err := io.ReadAll(reader)
	// A gzip body flushed but not closed delivers all its content yet ends
	// with ErrUnexpectedEOF; the handler's json.Unmarshal validates it.
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}
