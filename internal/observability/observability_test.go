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

type failingHandler struct{}

func (failingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (failingHandler) WithAttrs([]slog.Attr) slog.Handler       { return failingHandler{} }
func (failingHandler) WithGroup(string) slog.Handler            { return failingHandler{} }
func (failingHandler) Handle(context.Context, slog.Record) error {
	return errors.New("token=do-not-log-this")
}
