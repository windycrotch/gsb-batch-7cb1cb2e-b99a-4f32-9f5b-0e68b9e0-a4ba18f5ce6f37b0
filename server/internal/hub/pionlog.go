package hub

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pion/logging"
)

// pionLogs routes Pion's internal logging into slog. Pion reports recoverable
// transport noise as errors, so everything is demoted by one level.
type pionLogs struct{ log *slog.Logger }

func (f pionLogs) NewLogger(scope string) logging.LeveledLogger {
	return pionLogger{f.log.With("component", "pion/"+scope)}
}

type pionLogger struct{ log *slog.Logger }

func (l pionLogger) emit(level slog.Level, msg string) {
	l.log.Log(context.Background(), level, msg)
}

func (l pionLogger) emitf(level slog.Level, format string, args []any) {
	if l.log.Enabled(context.Background(), level) {
		l.emit(level, fmt.Sprintf(format, args...))
	}
}

const levelTrace = slog.LevelDebug - 4

func (l pionLogger) Trace(msg string)                  { l.emit(levelTrace, msg) }
func (l pionLogger) Tracef(format string, args ...any) { l.emitf(levelTrace, format, args) }
func (l pionLogger) Debug(msg string)                  { l.emit(levelTrace, msg) }
func (l pionLogger) Debugf(format string, args ...any) { l.emitf(levelTrace, format, args) }
func (l pionLogger) Info(msg string)                   { l.emit(slog.LevelDebug, msg) }
func (l pionLogger) Infof(format string, args ...any)  { l.emitf(slog.LevelDebug, format, args) }
func (l pionLogger) Warn(msg string)                   { l.emit(slog.LevelInfo, msg) }
func (l pionLogger) Warnf(format string, args ...any)  { l.emitf(slog.LevelInfo, format, args) }
func (l pionLogger) Error(msg string)                  { l.emit(slog.LevelWarn, msg) }
func (l pionLogger) Errorf(format string, args ...any) { l.emitf(slog.LevelWarn, format, args) }
