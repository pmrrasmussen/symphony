// Package observability provides the narrow, safe boundary between runtime
// state and operator-visible logs.
//
// That boundary is a chokepoint, not a convention: Redact wraps any
// slog.Handler, so a record is scrubbed on its way to the sink rather than at
// the call site that built it. Every component logs through one API —
// *slog.Logger — and inherits the same guarantee whether it holds the process
// logger, one derived with With, or one a test handed it. A call site that
// forgets Text still gets the backstop; see docs/observability.md.
package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

const MaxDiagnosticBytes = 512

// sensitiveKeys is the shared key vocabulary of the two assignment forms
// below. It is one string because the forms differ only in how the value is
// delimited: a key that is sensitive quoted is sensitive bare. The optional
// separator in api[_-]?key covers the unseparated JSON spellings — `apikey`
// and, case-insensitively, `apiKey` — as well as api_key and api-key.
const sensitiveKeys = `api[_-]?key|access[_-]?token|refresh[_-]?token|token|secret|password|authorization|prompt|description|environment|env|tool(?:_arguments)?`

// Text is applied to exactly the strings most likely to carry a secret it did
// not put there: HTTP error bodies and CLI stderr. Those carry credentials as
// JSON far more often than as key=value, so both forms are matched. The two
// patterns are one rule split in two only because RE2 has no backreference
// with which a single pattern could require the value's closing quote to match
// its opening one; quoted runs first so a quoted value is masked whole,
// spaces included, instead of up to its first space.
var quotedAssignment = regexp.MustCompile(`(?i)("?\b(?:` + sensitiveKeys + `)\b"?\s*[=:]\s*)"(?:bearer\s+)?[^"]*"`)

// The bare form takes an opening quote into its prefix and stops the value at
// the next quote, so it also covers the case the quoted form cannot: a value
// whose closing quote is not there at all, which is what a body truncated
// mid-credential looks like (internal/github reads a bounded excerpt of every
// error response). Because the quote it consumes is re-emitted with the
// prefix, running it after the quoted form leaves that form's output byte for
// byte unchanged.
var bareAssignment = regexp.MustCompile(`(?i)("?\b(?:` + sensitiveKeys + `)\b"?\s*[=:]\s*"?)(?:bearer\s+)?[^\s,;"]+`)

var bearerToken = regexp.MustCompile(`(?i)\bbearer\s+[^\s,;"]+`)

// Text returns a valid UTF-8 diagnostic suitable for a log. It deliberately
// accepts only a small bounded excerpt and masks common credential forms.
func Text(value string) string {
	value = strings.ToValidUTF8(value, "?")
	value = quotedAssignment.ReplaceAllString(value, `${1}"[REDACTED]"`)
	value = bareAssignment.ReplaceAllString(value, "${1}[REDACTED]")
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

// redactingHandler applies safeAttr to every attribute of every record on its
// way to the sink. A handler failure is reported to fallback, so an
// unavailable JSON log never stops scheduling and is still visible to the
// operator; the error is still returned, because a wrapping handler is
// entitled to see it.
type redactingHandler struct {
	inner    slog.Handler
	fallback io.Writer
}

// Redact returns handler wrapped in the redaction boundary. It is idempotent:
// an already-wrapped handler is returned unchanged, so every component that
// accepts a logger may wrap it defensively — the guarantee then belongs to the
// component rather than to the one wiring site that happened to pass a
// redacted handler.
func Redact(handler slog.Handler, fallback io.Writer) slog.Handler {
	if handler == nil {
		handler = slog.Default().Handler()
	}
	if redacting, ok := handler.(*redactingHandler); ok {
		return redacting
	}
	if fallback == nil {
		fallback = os.Stderr
	}
	return &redactingHandler{inner: handler, fallback: fallback}
}

// New builds the process's root logger over handler.
//
// Debug is the opt-in, operator-enabled level for actionable diagnostics: poll
// admission/rejection detail, tool/item lifecycle transitions, and heartbeat
// records. It is gated by the configured handler level like any other level,
// so it has no effect (and no cost beyond the Enabled check) unless the
// operator has raised the log level to debug.
func New(handler slog.Handler, fallback io.Writer) *slog.Logger {
	return slog.New(Redact(handler, fallback))
}

// Logger returns logger routed through the redaction boundary, defaulting to
// the process-wide handler for a nil logger. A component calls it on whatever
// logger it is handed, so no component can be given an unredacted sink.
func Logger(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	if _, ok := logger.Handler().(*redactingHandler); ok {
		return logger
	}
	return slog.New(Redact(logger.Handler(), nil))
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	safe := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		safe.AddAttrs(safeAttr(attr))
		return true
	})
	if err := h.inner.Handle(ctx, safe); err != nil {
		_, _ = fmt.Fprintf(h.fallback, "symphony log sink failure: %s\n", Text(err.Error()))
		return err
	}
	return nil
}

// WithAttrs redacts the preserved attributes once, here, rather than on every
// record they are later attached to: a logger built with With is as bounded as
// one that passes the same attribute per call.
func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	safe := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		safe = append(safe, safeAttr(attr))
	}
	return &redactingHandler{inner: h.inner.WithAttrs(safe), fallback: h.fallback}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name), fallback: h.fallback}
}

func safeAttr(attr slog.Attr) slog.Attr {
	// A LogValuer is resolved before it is inspected: the rules below apply to
	// the value that would reach the sink, not to the wrapper standing in for
	// it.
	attr.Value = attr.Value.Resolve()
	if opaqueKey(attr.Key) {
		return slog.String(attr.Key, "[REDACTED]")
	}
	// A group is redacted member by member. Nothing about being nested makes an
	// attribute safe, and slog.Group is how a bare slog call site nests one.
	if attr.Value.Kind() == slog.KindGroup {
		members := attr.Value.Group()
		safe := make([]slog.Attr, 0, len(members))
		for _, member := range members {
			safe = append(safe, safeAttr(member))
		}
		return slog.Attr{Key: attr.Key, Value: slog.GroupValue(safe...)}
	}
	if err, ok := attr.Value.Any().(error); ok {
		return slog.String(attr.Key, Text(err.Error()))
	}
	if value, ok := attr.Value.Any().(string); ok && textKey(attr.Key) {
		return slog.String(attr.Key, Text(value))
	}
	if value := attr.Value.Any(); value != nil {
		switch value := value.(type) {
		case map[string]int64:
			// Rate-limit summaries are generated from a fixed numeric allowlist.
		case Operation:
			// The operation vocabulary is a closed set of fixed literals, so a
			// member is logged by name instead of being omitted as an opaque
			// value. Membership is checked by value, not by type: an Operation
			// converted from arbitrary text is not part of the log contract and
			// must not bypass Text's scrubbing and truncation.
			if known[value] {
				return slog.String(attr.Key, string(value))
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
	switch strings.ToLower(strings.ReplaceAll(key, "-", "_")) {
	case "prompt", "description", "message", "tool", "tools", "tool_arguments", "environment", "env", "raw", "raw_event":
		return true
	// Credential-shaped keys. No record in this repository logs one
	// deliberately, so a record carrying one is a mistake — and the mistake is
	// the whole value, whatever shape it has. Redacting by key needs no guess
	// about that shape, unlike Text, which has to recognise the assignment
	// around it. The token-usage counters are `input_tokens` and friends, so
	// the exact match below leaves them alone.
	case "token", "api_key", "apikey", "access_token", "refresh_token", "secret", "password", "authorization", "credential", "credentials":
		return true
	}
	return false
}

// textKey names the string-valued attributes carried verbatim from a provider
// response, a child process, or an error. "!BADKEY" is slog's own key for a
// malformed argument pair, which is exactly the case where the call site did
// not choose a key and so cannot have chosen a safe one either.
func textKey(key string) bool {
	return key == "error" || key == "stderr" || key == "diagnostic" || key == "!BADKEY"
}
