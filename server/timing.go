package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/goccy/bigquery-emulator/internal/logger"
)

// requestTiming carries timestamps from sequentialAccessMiddleware to the
// access log: when the request arrived and when it got the global lock.
type requestTiming struct {
	arrived  time.Time
	acquired time.Time
}

type requestTimingKey struct{}

func withRequestTiming(ctx context.Context, t *requestTiming) context.Context {
	return context.WithValue(ctx, requestTimingKey{}, t)
}

func requestTimingFromContext(ctx context.Context) *requestTiming {
	t, _ := ctx.Value(requestTimingKey{}).(*requestTiming)
	return t
}

// statusRecorder records the status code and the number of body bytes.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// phaseTimer logs per-phase durations of a query job at Debug level. It does
// nothing when Debug is disabled.
type phaseTimer struct {
	log    *zap.Logger
	last   time.Time
	fields []zap.Field
}

func newPhaseTimer(ctx context.Context, sql string) *phaseTimer {
	l := logger.FromContext(ctx)
	if !l.Core().Enabled(zap.DebugLevel) {
		return &phaseTimer{}
	}
	sql = strings.Join(strings.Fields(sql), " ")
	// sql_hash identifies the whole statement, so repeats can be counted
	// from the log (the prefix alone is ambiguous: every dbt query starts
	// with the same /* {"app": "dbt", ...} */ comment).
	sum := sha256.Sum256([]byte(sql))
	fields := []zap.Field{
		zap.String("sql", sqlPrefix(sql, 80)),
		zap.String("sql_hash", hex.EncodeToString(sum[:6])),
		zap.Int("sql_len", len(sql)),
	}
	return &phaseTimer{log: l, last: time.Now(), fields: fields}
}

// sqlPrefix returns the first n bytes of sql after any leading block
// comments, which clients such as dbt prepend to every statement.
func sqlPrefix(sql string, n int) string {
	for strings.HasPrefix(sql, "/*") {
		end := strings.Index(sql, "*/")
		if end < 0 {
			break
		}
		sql = strings.TrimSpace(sql[end+2:])
	}
	if len(sql) > n {
		sql = sql[:n]
	}
	return sql
}

// setJobID records the job ID once it is known.
func (p *phaseTimer) setJobID(id string) {
	if p.log != nil {
		p.fields = append(p.fields, zap.String("job_id", id))
	}
}

// mark closes the phase that started at the previous mark.
func (p *phaseTimer) mark(name string) {
	if p.log == nil {
		return
	}
	now := time.Now()
	p.fields = append(p.fields, zap.Float64(name+"_ms", ms(now.Sub(p.last))))
	p.last = now
}

func (p *phaseTimer) flush(msg string) {
	if p.log != nil {
		p.log.Debug(msg, p.fields...)
	}
}
