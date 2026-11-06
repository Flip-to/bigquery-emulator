package logger

import (
	"context"

	"go.uber.org/zap"
)

type loggerKey struct{}

func WithLogger(ctx context.Context, logger *zap.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}

// FromContext returns the logger stored in ctx, or a no-op logger when there
// is none (handlers called outside the HTTP middleware chain).
func FromContext(ctx context.Context) *zap.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*zap.Logger); ok && l != nil {
		return l
	}
	return zap.NewNop()
}

func Logger(ctx context.Context) *zap.Logger {
	return ctx.Value(loggerKey{}).(*zap.Logger)
}
