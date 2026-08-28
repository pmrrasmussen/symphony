package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/pmrrasmussen/symphony/internal/operator"
)

// Run owns no persistent connection or background process. Each refresh
// re-invokes discovery, which only rereads local launchd, status, and log data.
//
// A terminal gets the interactive program, which reads single keypresses and
// refreshes on a timer. Any other writer gets plain frames driven by
// line-buffered input, so redirected output stays free of the control
// sequences an interactive renderer needs.
func Run(ctx context.Context, input io.Reader, output io.Writer, discover Discover) error {
	instances, err := discover(ctx, operator.Options{})
	model := New(instances, time.Now())
	if err != nil {
		model.message = "initial discovery failed: " + err.Error()
		model.failed = true
	}
	if isTerminal(output) {
		return runInteractive(ctx, input, output, discover, model)
	}
	return runPlain(ctx, input, output, discover, model)
}

// isTerminal reports whether frames are going to a terminal. Only a terminal
// can use the interactive renderer: an interactive program writes cursor and
// key-protocol sequences unconditionally, and against a pipe it emits those
// sequences without ever delivering the frame text.
func isTerminal(output io.Writer) bool {
	file, ok := output.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// runPlain repaints whole frames and reads one line per action. It stays the
// path for pipes, files, and tests, none of which can use raw mode.
func runPlain(ctx context.Context, input io.Reader, output io.Writer, discover Discover, model Model) error {
	reader := bufio.NewReader(input)
	for {
		if _, err := fmt.Fprint(output, model.View(time.Now())); err != nil {
			return err
		}
		line, err := reader.ReadString('\n')
		if err != nil && len(line) == 0 {
			if err == io.EOF {
				return nil
			}
			return err
		}
		key := normalizeKey(line)
		if key == "r" {
			instances, refreshErr := discover(ctx, operator.Options{})
			model.Refresh(instances, refreshErr, time.Now())
			continue
		}
		var quit bool
		model, quit = model.Update(key)
		if quit {
			return nil
		}
		if err == io.EOF {
			return nil
		}
	}
}

// runInteractive drives the same Model from a terminal. Interruption is a
// normal way to close a read-only view, so it is not reported as a failure.
func runInteractive(ctx context.Context, input io.Reader, output io.Writer, discover Discover, model Model) error {
	model.layout = true
	model.color = colorEnabled()
	view := &app{model: model, discover: discover, ctx: ctx}
	program := tea.NewProgram(view,
		tea.WithContext(ctx),
		// Input must be passed explicitly. Left to itself the program ignores
		// a redirected stdin, opens /dev/tty, and fails outright where there
		// is no controlling terminal.
		tea.WithInput(input),
		tea.WithOutput(output),
	)
	if _, err := program.Run(); err != nil {
		if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}

// app adapts the pure Model to the interactive runtime. It contributes only
// key translation and scheduling; every state transition stays in
// Model.Update and Model.Refresh, which are testable without a terminal.
type app struct {
	model    Model
	discover Discover
	ctx      context.Context
}

// tickMsg asks for a scheduled refresh.
type tickMsg time.Time

// discoveredMsg carries the result of one completed discovery.
type discoveredMsg struct {
	instances []operator.Instance
	err       error
}

func (a *app) Init() tea.Cmd { return tickCmd() }

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(now time.Time) tea.Msg { return tickMsg(now) })
}

// discoverCmd runs discovery off the UI goroutine. Discovery shells out to
// plutil and launchctl and runs a preflight per instance, so on the UI
// goroutine it would freeze the view for the whole sweep.
func (a *app) discoverCmd() tea.Cmd {
	return func() tea.Msg {
		instances, err := a.discover(a.ctx, operator.Options{})
		return discoveredMsg{instances: instances, err: err}
	}
}

func (a *app) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tickMsg:
		return a, tea.Batch(a.discoverCmd(), tickCmd())
	case discoveredMsg:
		a.model.Refresh(message.instances, message.err, time.Now())
		return a, nil
	case tea.WindowSizeMsg:
		// The renderer sizes its rules and table to the width and budgets its
		// rows against the height.
		a.model.width, a.model.height = message.Width, message.Height
		return a, nil
	case tea.KeyPressMsg:
		return a.press(message.String())
	}
	return a, nil
}

// press maps one keystroke onto the Model's existing key vocabulary.
func (a *app) press(pressed string) (tea.Model, tea.Cmd) {
	key := normalizeKey(pressed)
	switch key {
	case "ctrl+c":
		// Raw mode suppresses the interrupt signal, so the terminal's own
		// cancel key only reaches the program as an ordinary keypress.
		return a, tea.Quit
	case "r":
		return a, a.discoverCmd()
	}
	model, quit := a.model.Update(key)
	a.model = model
	if quit {
		return a, tea.Quit
	}
	return a, nil
}

func (a *app) View() tea.View {
	frame, maxOffset := a.model.render(time.Now())
	// Correct a scroll that ran past the end, so scrolling back responds on the
	// first keypress rather than after as many as ran over.
	a.model.offset = min(a.model.offset, maxOffset)
	view := tea.NewView(frame)
	view.AltScreen = true
	return view
}
