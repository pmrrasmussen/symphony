package observability

import (
	"bytes"
	"context"
	"errors"
	"io"
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

// TestTextRedactsJSONShapedCredentials pins the form Text is most often handed:
// Text is applied to error and stderr strings -- HTTP error bodies and CLI
// diagnostics -- which carry a credential as a JSON member far more often than
// as a shell-style assignment. Until PMR-181 the closing quote after the key
// broke the match and every case below passed through verbatim.
func TestTextRedactsJSONShapedCredentials(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value string
	}{
		{name: "json body", value: `{"token":"lin_api_do-not-log-this"}`},
		{name: "json body with space", value: `{"api_key": "sk-do-not-log-this"}`},
		{name: "camel case", value: `{"apiKey":"do-not-log-this"}`},
		{name: "unseparated key", value: `{"apikey":"do-not-log-this"}`},
		{name: "quoted bearer", value: `{"authorization": "Bearer do-not-log-this"}`},
		{name: "value with spaces", value: `secret="do-not-log-this and this too"`},
		{name: "nested in a sentence", value: `github request failed: {"message":"Bad credentials","access_token":"do-not-log-this"}`},
		{name: "single quotes", value: `--header 'authorization: do-not-log-this'`},
		{name: "bare assignment still masked", value: `password=do-not-log-this`},
		// A response body read through a byte-bounded reader can end mid-value,
		// leaving an opening quote with no closing one.
		{name: "truncated mid value", value: `{"message":"Bad credentials","token":"ghp_do-not-log-this`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := Text(testCase.value)
			if strings.Contains(got, "do-not-log-this") {
				t.Fatalf("secret survived redaction: %q", got)
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("no redaction marker in %q", got)
			}
			// The quoted and bare forms run in sequence over the same string, so
			// the second must leave the first's output alone; a diagnostic that
			// passes through Text twice must read the same either way.
			if again := Text(got); again != got {
				t.Fatalf("Text was not stable across a second pass:\n first: %q\nsecond: %q", got, again)
			}
		})
	}
}

// TestTextLeavesNonCredentialTextAlone keeps the widened pattern from eating
// the diagnostics an operator reads: the point of a bounded excerpt is that
// what survives is still legible.
func TestTextLeavesNonCredentialTextAlone(t *testing.T) {
	for _, value := range []string{
		`{"message":"Base branch was modified","status":422}`,
		"remote: Permission to owner/repo.git denied",
		`{"pr_number": 27, "state": "open"}`,
	} {
		if got := Text(value); got != value {
			t.Fatalf("Text rewrote a credential-free diagnostic\n got: %q\nwant: %q", got, value)
		}
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

func TestLoggerUsesTheGivenHandlerAndSurvivesANilLogger(t *testing.T) {
	var out bytes.Buffer
	logger := Logger(slog.New(slog.NewJSONHandler(&out, nil)))
	logger.Info("from slog event")
	if !strings.Contains(out.String(), "from slog event") {
		t.Fatalf("Logger did not forward to the given handler's writer: %s", out.String())
	}

	if logger := Logger(nil); logger == nil || logger.Handler() == nil {
		t.Fatalf("Logger(nil) did not fall back to a default handler")
	}
}

// TestRedactionIsAChokepointForBareSlog is the property the whole boundary
// rests on: redaction lives in the handler, so a component holding a plain
// *slog.Logger over that handler -- which is every component but the two that
// used to wrap it -- is covered without remembering anything. Before PMR-181
// an attr named `token` logged this way was written verbatim.
func TestRedactionIsAChokepointForBareSlog(t *testing.T) {
	var out bytes.Buffer
	shared := New(slog.NewJSONHandler(&out, nil), nil)

	bare := slog.New(shared.Handler())
	bare.Info("bare slog record", "token", "do-not-log-this", "prompt", "issue text", "error", `{"api_key":"sk-do-not-log-this"}`)
	// A logger built with With must be as bounded as one passing the same
	// attribute per call, and a group must not be a hole in the boundary.
	bare.With("prompt", "issue text").Info("derived record", slog.Group("detail", "stderr", "token=do-not-log-this"))

	got := out.String()
	if strings.Contains(got, "do-not-log-this") || strings.Contains(got, "issue text") {
		t.Fatalf("a secret-shaped attribute survived the handler boundary: %s", got)
	}
	if !strings.Contains(got, `"prompt":"[REDACTED]"`) {
		t.Fatalf("an opaque key was not redacted: %s", got)
	}
}

// TestRedactIsIdempotent lets every component defensively wrap whatever logger
// it is handed: without this, the wiring in cmd/symphony would pay for one
// redaction pass per component and a record would read `[REDACTED]` nested in
// `[REDACTED]`.
func TestRedactIsIdempotent(t *testing.T) {
	handler := Redact(slog.NewJSONHandler(io.Discard, nil), nil)
	if again := Redact(handler, nil); again != handler {
		t.Fatalf("Redact wrapped an already-redacting handler a second time")
	}
	logger := slog.New(handler)
	if again := Logger(logger); again != logger {
		t.Fatalf("Logger rewrapped a logger that already redacts")
	}
}

// TestRedactedHandlerDelegatesEnabled proves the middleware is transparent to
// the level policy it wraps: an operator's --log-level still decides what is
// written, and a debug record still costs nothing beyond the Enabled check.
func TestRedactedHandlerDelegatesEnabled(t *testing.T) {
	handler := Redact(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn}), nil)
	if handler.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("the wrapped handler reported info enabled at warn level")
	}
	if !handler.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("the wrapped handler reported error disabled at warn level")
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
