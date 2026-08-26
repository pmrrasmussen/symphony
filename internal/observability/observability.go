// Package observability provides the narrow, safe boundary between runtime
// state and operator-visible logs.
package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxDiagnosticBytes = 512

var sensitiveAssignment = regexp.MustCompile(`(?i)(\b(?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|secret|password|authorization|prompt|description|environment|env|tool(?:_arguments)?)\b\s*(?:=|:)\s*)(?:bearer\s+)?[^\s,;]+`)
var bearerToken = regexp.MustCompile(`(?i)\bbearer\s+[^\s,;]+`)

// Text returns a valid UTF-8 diagnostic suitable for a log. It deliberately
// accepts only a small bounded excerpt and masks common credential forms.
func Text(value string) string {
	value = strings.ToValidUTF8(value, "?")
	value = sensitiveAssignment.ReplaceAllString(value, "${1}[REDACTED]")
	value = bearerToken.ReplaceAllString(value, "Bearer [REDACTED]")
	if len(value) <= MaxDiagnosticBytes {
		return value
	}
	end := MaxDiagnosticBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "…[truncated]"
}

// Logger forwards structured records to a slog handler. A handler failure is
// reported to fallback, so an unavailable JSON log never stops scheduling and
// is still visible to the operator.
type Logger struct {
	handler  slog.Handler
	fallback io.Writer
}

func New(handler slog.Handler, fallback io.Writer) *Logger {
	if handler == nil {
		handler = slog.Default().Handler()
	}
	if fallback == nil {
		fallback = os.Stderr
	}
	return &Logger{handler: handler, fallback: fallback}
}

func FromSlog(logger *slog.Logger) *Logger {
	if logger == nil {
		logger = slog.Default()
	}
	return New(logger.Handler(), os.Stderr)
}

// Handler returns the underlying handler for integrations that still accept a
// standard slog.Logger.
func (l *Logger) Handler() slog.Handler { return l.handler }

// Debug is the opt-in, operator-enabled level for actionable diagnostics: poll
// admission/rejection detail, tool/item lifecycle transitions, and heartbeat
// records. It is gated by the configured handler level like any other level,
// so it has no effect (and no cost beyond the Enabled check) unless the
// operator has raised the log level to debug.
func (l *Logger) Debug(message string, args ...any) { l.log(slog.LevelDebug, message, attrs(args)...) }
func (l *Logger) Info(message string, args ...any)  { l.log(slog.LevelInfo, message, attrs(args)...) }
func (l *Logger) Warn(message string, args ...any)  { l.log(slog.LevelWarn, message, attrs(args)...) }
func (l *Logger) Error(message string, args ...any) { l.log(slog.LevelError, message, attrs(args)...) }

func attrs(args []any) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(args)/2)
	for len(args) > 0 {
		if attr, ok := args[0].(slog.Attr); ok {
			attrs = append(attrs, safeAttr(attr))
			args = args[1:]
			continue
		}
		if len(args) == 1 {
			attrs = append(attrs, slog.String("!BADKEY", Text(fmt.Sprint(args[0]))))
			break
		}
		key, ok := args[0].(string)
		if !ok {
			key = "!BADKEY"
		}
		attrs = append(attrs, safeAttr(slog.Any(key, args[1])))
		args = args[2:]
	}
	return attrs
}

func safeAttr(attr slog.Attr) slog.Attr {
	if opaqueKey(attr.Key) {
		return slog.String(attr.Key, "[REDACTED]")
	}
	if err, ok := attr.Value.Any().(error); ok {
		return slog.String(attr.Key, Text(err.Error()))
	}
	if value, ok := attr.Value.Any().(string); ok && (attr.Key == "error" || attr.Key == "stderr" || attr.Key == "diagnostic") {
		return slog.String(attr.Key, Text(value))
	}
	if value := attr.Value.Any(); value != nil {
		switch value.(type) {
		case map[string]int64:
			// Rate-limit summaries are generated from a fixed numeric allowlist.
		case Operation:
			// The operation vocabulary is a closed set of fixed literals, so a
			// member is logged by name instead of being omitted as an opaque
			// value. Membership is checked by value, not by type: an Operation
			// converted from arbitrary text is not part of the log contract and
			// must not bypass Text's scrubbing and truncation.
			if operation, ok := value.(Operation); ok && known[operation] {
				return slog.String(attr.Key, string(operation))
			}
			return slog.String(attr.Key, "[OMITTED]")
		default:
			if attr.Value.Kind() == slog.KindAny {
				if attr.Key == "event" || attr.Key == "retry_kind" || attr.Key == "reason" {
					return slog.String(attr.Key, fmt.Sprint(value))
				}
				return slog.String(attr.Key, "[OMITTED]")
			}
		}
	}
	return attr
}

func opaqueKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	return key == "prompt" || key == "description" || key == "message" || key == "tool" || key == "tools" || key == "tool_arguments" || key == "environment" || key == "env" || key == "raw" || key == "raw_event"
}

// Logger is deliberately context-free: many call sites (background poll
// loops, fallback paths, struct methods with no ctx parameter) have no
// request-scoped context in hand, and threading one through every Debug/Info/
// Warn/Error call for handlers that don't yet read it is not worth the
// churn. context.TODO() satisfies the slog.Handler contract without implying
// a real context is available.
func (l *Logger) log(level slog.Level, message string, attrs ...slog.Attr) {
	ctx := context.TODO()
	if !l.handler.Enabled(ctx, level) {
		return
	}
	record := slog.NewRecord(now(), level, message, 0)
	record.AddAttrs(attrs...)
	if err := l.handler.Handle(ctx, record); err != nil {
		_, _ = fmt.Fprintf(l.fallback, "symphony log sink failure: %s\n", Text(err.Error()))
	}
}

// now is kept as a variable so tests can assert records without relying on a
// particular clock source.
var now = func() time.Time { return time.Now() }
