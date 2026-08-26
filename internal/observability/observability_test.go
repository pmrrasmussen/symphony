package observability

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestTextRedactsCredentialsAndTruncates(t *testing.T) {
	secret := "do-not-log-this"
	value := Text("token=" + secret + " Bearer another-secret " + strings.Repeat("x", MaxDiagnosticBytes+20))
	if strings.Contains(value, secret) || strings.Contains(value, "another-secret") {
		t.Fatalf("secret leaked from diagnostic %q", value)
	}
	if !strings.Contains(value, "[REDACTED]") || !strings.HasSuffix(value, "…[truncated]") {
		t.Fatalf("diagnostic was not redacted and truncated: %q", value)
	}
}

func TestLoggerReportsSinkFailureWithoutPanicking(t *testing.T) {
	fallback := new(bytes.Buffer)
	logger := New(failingHandler{}, fallback)
	logger.Error("test event", "error", errors.New("token=do-not-log-this"))
	if got := fallback.String(); !strings.Contains(got, "symphony log sink failure") || strings.Contains(got, "do-not-log-this") {
		t.Fatalf("fallback=%q", got)
	}
}

func TestDebugIsGatedByTheConfiguredHandlerLevel(t *testing.T) {
	var infoLevel bytes.Buffer
	logger := New(slog.NewJSONHandler(&infoLevel, &slog.HandlerOptions{Level: slog.LevelInfo}), nil)
	logger.Debug("opt-in detail", "key", "value")
	if infoLevel.Len() != 0 {
		t.Fatalf("debug record was emitted at the default info level: %s", infoLevel.String())
	}

	var debugLevel bytes.Buffer
	logger = New(slog.NewJSONHandler(&debugLevel, &slog.HandlerOptions{Level: slog.LevelDebug}), nil)
	logger.Debug("opt-in detail", "key", "value")
	if !strings.Contains(debugLevel.String(), "opt-in detail") {
		t.Fatalf("debug record was not emitted once the operator raised the level: %s", debugLevel.String())
	}
}

type failingHandler struct{}

func (failingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (failingHandler) WithAttrs([]slog.Attr) slog.Handler       { return failingHandler{} }
func (failingHandler) WithGroup(string) slog.Handler            { return failingHandler{} }
func (failingHandler) Handle(context.Context, slog.Record) error {
	return errors.New("token=do-not-log-this")
}

// TestLoggerPassesAUsableContextToTheHandler proves the slog.Handler
// contract is honored by every level, not merely by the accident that the
// handlers this package has installed so far ignore their context argument.
// A handler that dereferences ctx (Value/Done, as a tracing or sampling
// wrapper would) must not panic and must observe a non-nil context.
func TestLoggerPassesAUsableContextToTheHandler(t *testing.T) {
	handler := &contextCheckingHandler{}
	logger := New(handler, nil)

	logger.Debug("debug event")
	logger.Info("info event")
	logger.Warn("warn event")
	logger.Error("error event")

	if handler.calls != 4 {
		t.Fatalf("handler observed %d calls, want 4", handler.calls)
	}
}

func TestFromSlogUsesTheGivenHandlerAndSurvivesANilLogger(t *testing.T) {
	var out bytes.Buffer
	logger := FromSlog(slog.New(slog.NewJSONHandler(&out, nil)))
	logger.Info("from slog event")
	if !strings.Contains(out.String(), "from slog event") {
		t.Fatalf("FromSlog did not forward to the given handler's writer: %s", out.String())
	}

	if logger := FromSlog(nil); logger == nil || logger.Handler() == nil {
		t.Fatalf("FromSlog(nil) did not fall back to a default handler")
	}
}

// contextCheckingHandler dereferences its context argument the way a
// tracing, sampling, or request-scoped-attribute wrapper would. Enabled and
// Handle both panic on a nil context, so this handler fails the test loudly
// if Logger ever regresses to passing one.
type contextCheckingHandler struct {
	calls int
}

func (h *contextCheckingHandler) Enabled(ctx context.Context, _ slog.Level) bool {
	_ = ctx.Done()
	return true
}

func (h *contextCheckingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *contextCheckingHandler) WithGroup(string) slog.Handler      { return h }

func (h *contextCheckingHandler) Handle(ctx context.Context, _ slog.Record) error {
	_ = ctx.Value("probe")
	h.calls++
	return nil
}

// TestOperationVocabularyIsLoggedByName proves the bounded operation
// vocabulary survives the redaction boundary as its own name: it is a defined
// string type, so without an explicit allowance every `operation` field would
// be flattened to an opaque placeholder and the tracker-edge trail would lose
// the one field an operator filters on. An unbounded struct under the same key
// still stays opaque.
func TestOperationVocabularyIsLoggedByName(t *testing.T) {
	var out bytes.Buffer
	logger := New(slog.NewJSONHandler(&out, nil), nil)
	logger.Info("Linear transition", "operation", OperationReviewApproved)
	logger.Info("other record", "operation", struct{ Secret string }{Secret: "do-not-log-this"})
	if got := out.String(); !strings.Contains(got, `"operation":"review_approved"`) {
		t.Fatalf("bounded operation was not logged by name: %s", got)
	}
	if got := out.String(); strings.Contains(got, "do-not-log-this") || !strings.Contains(got, `"operation":"[OMITTED]"`) {
		t.Fatalf("an unbounded operation value was not omitted: %s", got)
	}
}

// TestUnknownOperationValueIsOmitted proves the closed set is enforced by
// value, not merely by type: an Operation converted from arbitrary text would
// otherwise bypass Text's scrubbing and truncation entirely and write whatever
// it holds — control bytes and unbounded length included — straight into a
// record.
func TestUnknownOperationValueIsOmitted(t *testing.T) {
	var out bytes.Buffer
	logger := New(slog.NewJSONHandler(&out, nil), nil)
	logger.Info("Linear transition", "operation", Operation(strings.Repeat("\x00", 200)+"\ntoken=do-not-log-this"))
	got := out.String()
	// The raw byte and its JSON escape are both checked: a NUL survives the
	// handler as the escape text, not as the byte itself.
	if strings.Contains(got, "do-not-log-this") || strings.Contains(got, "\u0000") || strings.Contains(got, `\u0000`) {
		t.Fatalf("an operation outside the closed set reached the record: %q", got)
	}
	if !strings.Contains(got, `"operation":"[OMITTED]"`) {
		t.Fatalf("an operation outside the closed set was not omitted: %q", got)
	}
	if len(got) > 256 {
		t.Fatalf("an omitted operation still produced an unbounded record of %d bytes", len(got))
	}
	for operation := range known {
		out.Reset()
		logger.Info("Linear transition", "operation", operation)
		if want := `"operation":"` + string(operation) + `"`; !strings.Contains(out.String(), want) {
			t.Fatalf("vocabulary member %q was not logged by name: %s", operation, out.String())
		}
	}
}
