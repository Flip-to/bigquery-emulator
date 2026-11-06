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
	"runtime"
	"strings"
	"sync"

	"github.com/gorilla/mux"
	"go.uber.org/zap"

	"github.com/goccy/bigquery-emulator/internal/logger"
)

// methodOverrideMiddleware honors the X-HTTP-Method-Override header on POST
// requests. The Java BigQuery client tunnels PATCH (and other verbs) through
// POST with this header by default; without translating it here the request
// would never match the method-specific route registered with the router.
func methodOverrideMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if override := strings.TrimSpace(r.Header.Get("X-HTTP-Method-Override")); override != "" {
				r.Method = strings.ToUpper(override)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// accessMode is how much of the emulator a request locks.
type accessMode int

const (
	accessNone accessMode = iota
	// accessShared lets the request run alongside other shared requests.
	// Only requests that read SQLite take it: metadata GETs and query jobs
	// whose statement is a single SELECT (see sharedQuery).
	accessShared
	// accessExclusive runs the request alone: DDL, DML, loads, inserts,
	// patches, deletes and anything not known to be read-only.
	accessExclusive
)

// accessGuard is the lock one request holds. It lives in the request
// context so a shared query job can promote itself to exclusive once its
// SELECT has run, to record the job (and its anonymous results table).
type accessGuard struct {
	mu   *sync.RWMutex
	mode accessMode
	// jobWrites is held while a shared request records its job; see
	// reopenForWrite.
	jobWrites *sync.Mutex
}

type accessGuardKey struct{}

func (g *accessGuard) acquire(mode accessMode) {
	switch mode {
	case accessShared:
		g.mu.RLock()
	case accessExclusive:
		g.mu.Lock()
	}
	g.mode = mode
}

func (g *accessGuard) release() {
	if g.jobWrites != nil {
		g.jobWrites.Unlock()
		g.jobWrites = nil
	}
	switch g.mode {
	case accessShared:
		g.mu.RUnlock()
	case accessExclusive:
		g.mu.Unlock()
	}
	g.mode = accessNone
}

// promote turns a shared lock into an exclusive one and reports whether it
// did. The two are not atomic: another writer may run in between, so the
// caller must have ended every SQLite transaction it opened under the
// shared lock (an open read transaction would make that writer fail with
// SQLITE_LOCKED) and must not rely on what it read still being current.
func (g *accessGuard) promote() bool {
	if g.mode != accessShared {
		return false
	}
	g.mu.RUnlock()
	g.mode = accessNone
	g.acquire(accessExclusive)
	return true
}

// accessLockMiddleware replaces the former single global mutex with a
// read/write lock. SQLite runs in shared-cache mode, where a connection
// that writes a table makes concurrent readers of it fail with
// SQLITE_LOCKED instead of waiting, so every request that writes must run
// alone; requests that only read run concurrently. Everything else the
// requests share is safe for concurrent use: the googlesqlite catalog and
// the googlesql wasm module serialize themselves, every pooled connection
// has its own analyzer, and the metadata job cache has its own mutex.
func accessLockMiddleware() func(http.Handler) http.Handler {
	var mu sync.RWMutex
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mode, err := classifyAccess(r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			g := &accessGuard{mu: &mu}
			g.acquire(mode)
			defer g.release()
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accessGuardKey{}, g)))
		})
	}
}

// classifyAccess picks the lock a request needs. A query job is inspected
// before any lock is taken, so its body is read (and gunzipped) here and
// handed on to the handler.
func classifyAccess(r *http.Request) (accessMode, error) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return accessShared, nil
	case http.MethodPost:
	default:
		return accessExclusive, nil
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	isJobsInsert := strings.HasSuffix(path, "/jobs") && !strings.Contains(path, "/upload/")
	isJobsQuery := strings.HasSuffix(path, "/queries")
	if !isJobsInsert && !isJobsQuery {
		return accessExclusive, nil
	}
	body, err := readRequestBody(r)
	if err != nil {
		return accessExclusive, err
	}
	if sharedQuery(body, isJobsInsert) {
		return accessShared, nil
	}
	return accessExclusive, nil
}

// readRequestBody reads the whole body, gunzipping it, and replaces it
// with the decoded bytes so the handler reads the same content.
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

// sharedQuery reports whether a jobs.insert (isJobsInsert) or jobs.query
// body runs one read-only SELECT with no destination table. statementType
// classifies the text lexically: one statement led by SELECT, WITH,
// FROM, VALUES or a parenthesis. BigQuery has no data-modifying WITH, so
// such a statement cannot write.
func sharedQuery(body []byte, isJobsInsert bool) bool {
	var query string
	if isJobsInsert {
		var job struct {
			Configuration *struct {
				Query *struct {
					Query            string          `json:"query"`
					DestinationTable json.RawMessage `json:"destinationTable"`
				} `json:"query"`
			} `json:"configuration"`
		}
		if err := json.Unmarshal(body, &job); err != nil || job.Configuration == nil || job.Configuration.Query == nil {
			return false
		}
		if len(job.Configuration.Query.DestinationTable) != 0 && string(job.Configuration.Query.DestinationTable) != "null" {
			return false
		}
		query = job.Configuration.Query.Query
	} else {
		var req struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return false
		}
		query = req.Query
	}
	return strings.TrimSpace(query) != "" && statementType(query) == "SELECT"
}

func recoveryMiddleware(s *Server) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					ctx := logger.WithLogger(r.Context(), s.logger)
					errorResponse(ctx, w, errInternalError(fmt.Sprintf("%+v", err)))
					var frame int = 1
					for {
						_, file, line, ok := runtime.Caller(frame)
						if !ok {
							break
						}
						s.logger.Error(fmt.Sprintf("%d: %v:%d", frame, file, line))
						frame++
					}
					return
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func loggerMiddleware(s *Server) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			next.ServeHTTP(w, r.WithContext(logger.WithLogger(ctx, s.logger)))
		})
	}
}

func accessLogMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger.Logger(r.Context()).Info(
				fmt.Sprintf("%s %s", r.Method, r.URL.Path),
				zap.String("query", r.URL.RawQuery),
			)
			next.ServeHTTP(w, r)
		})
	}
}

const (
	contentEncoding  = "Content-Encoding"
	encodingTypeGzip = "gzip"
)

func decompressMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(contentEncoding) != encodingTypeGzip {
				next.ServeHTTP(w, r)
				return
			}
			ctx := r.Context()
			reader, err := gzip.NewReader(r.Body)
			if err != nil {
				errorResponse(ctx, w, errInvalid(fmt.Sprintf("failed to decode gzip content: %s", err)))
				return
			}
			defer reader.Close()
			r.Body = reader
			next.ServeHTTP(w, r)
		})
	}
}

func withServerMiddleware(s *Server) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(
				w,
				r.WithContext(withServer(r.Context(), s)),
			)
		})
	}
}

func projectIDFromParams(params map[string]string) (string, bool) {
	projectID, exists := params["projectId"]
	if exists {
		return projectID, true
	}
	projectID, exists = params["projectsId"]
	return projectID, exists
}

// isDatasetsInsert reports whether r is POST /projects/{projectId}/datasets.
func isDatasetsInsert(r *http.Request, params map[string]string) bool {
	if _, ok := datasetIDFromParams(params); ok {
		return false
	}
	return r.Method == http.MethodPost && strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "/datasets")
}

func datasetIDFromParams(params map[string]string) (string, bool) {
	datasetID, exists := params["datasetId"]
	if exists {
		return datasetID, true
	}
	datasetID, exists = params["datasetsId"]
	return datasetID, exists
}

func jobIDFromParams(params map[string]string) (string, bool) {
	jobID, exists := params["jobId"]
	if exists {
		return jobID, true
	}
	jobID, exists = params["jobsId"]
	return jobID, exists
}

func tableIDFromParams(params map[string]string) (string, bool) {
	tableID, exists := params["tableId"]
	if exists {
		return tableID, true
	}
	tableID, exists = params["tablesId"]
	return tableID, exists
}

func modelIDFromParams(params map[string]string) (string, bool) {
	modelID, exists := params["modelId"]
	if exists {
		return modelID, true
	}
	modelID, exists = params["modelsId"]
	return modelID, exists
}

func routineIDFromParams(params map[string]string) (string, bool) {
	routineID, exists := params["routineId"]
	if exists {
		return routineID, true
	}
	routineID, exists = params["routinesId"]
	return routineID, exists
}

func withProjectMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			params := mux.Vars(r)
			projectID, exists := projectIDFromParams(params)
			if exists {
				server := serverFromContext(ctx)
				project, err := server.metaRepo.FindProject(ctx, projectID)
				if err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprintln(w, err)
					return
				}
				if project == nil && isDatasetsInsert(r, params) {
					// datasets.insert into a project the emulator was not started with
					// creates it, so a caller can seed tables in more than one project.
					if err := server.SetProject(projectID); err != nil {
						w.WriteHeader(http.StatusInternalServerError)
						fmt.Fprintln(w, err)
						return
					}
					project, err = server.metaRepo.FindProject(ctx, projectID)
					if err != nil {
						w.WriteHeader(http.StatusInternalServerError)
						fmt.Fprintln(w, err)
						return
					}
				}
				if project == nil {
					errorResponse(ctx, w, errNotFound(fmt.Sprintf("project %s is not found", projectID)))
					return
				}
				ctx = withProject(ctx, project)
			}
			next.ServeHTTP(
				w,
				r.WithContext(ctx),
			)
		})
	}
}

func withDatasetMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			params := mux.Vars(r)
			datasetID, exists := datasetIDFromParams(params)
			if exists {
				project := projectFromContext(ctx)
				dataset := project.Dataset(datasetID)
				if dataset == nil {
					errorResponse(ctx, w, errNotFound(fmt.Sprintf("dataset %s is not found", datasetID)))
					return
				}
				ctx = withDataset(ctx, dataset)
			}
			next.ServeHTTP(
				w,
				r.WithContext(ctx),
			)
		})
	}
}

func withJobMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			params := mux.Vars(r)
			jobID, exists := jobIDFromParams(params)
			if exists {
				project := projectFromContext(ctx)
				job := project.Job(jobID)
				if job == nil {
					errorResponse(ctx, w, errNotFound(fmt.Sprintf("job %s is not found", jobID)))
					return
				}
				ctx = withJob(ctx, job)
			}
			next.ServeHTTP(
				w,
				r.WithContext(ctx),
			)
		})
	}
}

func withTableMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			params := mux.Vars(r)
			tableID, exists := tableIDFromParams(params)
			if exists {
				dataset := datasetFromContext(ctx)
				table := dataset.Table(tableID)
				if table == nil {
					errorResponse(ctx, w, errNotFound(fmt.Sprintf("table %s is not found", tableID)))
					return
				}
				ctx = withTable(ctx, table)
			}
			next.ServeHTTP(
				w,
				r.WithContext(ctx),
			)
		})
	}
}

func withModelMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			params := mux.Vars(r)
			modelID, exists := modelIDFromParams(params)
			if exists {
				dataset := datasetFromContext(ctx)
				model := dataset.Model(modelID)
				if model == nil {
					errorResponse(ctx, w, errNotFound(fmt.Sprintf("model %s is not found", modelID)))
					return
				}
				ctx = withModel(ctx, model)
			}
			next.ServeHTTP(
				w,
				r.WithContext(ctx),
			)
		})
	}
}

func withRoutineMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			params := mux.Vars(r)
			routineID, exists := routineIDFromParams(params)
			if exists {
				dataset := datasetFromContext(ctx)
				routine := dataset.Routine(routineID)
				if routine == nil {
					errorResponse(ctx, w, errNotFound(fmt.Sprintf("routine %s is not found", routineID)))
					return
				}
				ctx = withRoutine(ctx, routine)
			}
			next.ServeHTTP(
				w,
				r.WithContext(ctx),
			)
		})
	}
}
