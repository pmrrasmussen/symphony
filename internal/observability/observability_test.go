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
