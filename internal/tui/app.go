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
// re-invokes discovery, which only rereads local launchd, status, and log data
// -- the one part of a sweep that is not such a read, the preflight's agent CLI
// authentication probe, is held in a cache this view owns for its lifetime, so
// only a first sweep, a changed plist or workflow, or an explicit refresh pays
// for it.
//
// The context is cancelled on return so a sweep still running when the view
// closes takes its probes down with it, rather than leaving a keychain prompt
// behind for a dashboard that no longer exists.
//
// A terminal gets the interactive program, which reads single keypresses and
// refreshes on a timer. Any other writer gets plain frames driven by
// line-buffered input, so redirected output stays free of the control
// sequences an interactive renderer needs.
func Run(ctx context.Context, input io.Reader, output io.Writer, discover Discover) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	options := operator.Options{Preflight: &operator.PreflightCache{}}
	instances, err := discover(ctx, options)
	model := New(instances, time.Now())
	if err != nil {
		model.message = "initial discovery failed: " + err.Error()
		model.failed = true
	}
	if isTerminal(output) {
		return runInteractive(ctx, input, output, discover, options, model)
	}
	return runPlain(ctx, input, output, discover, options, model)
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
func runPlain(ctx context.Context, input io.Reader, output io.Writer, discover Discover, options operator.Options, model Model) error {
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
			instances, refreshErr := discover(ctx, refreshOptions(options))
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
func runInteractive(ctx context.Context, input io.Reader, output io.Writer, discover Discover, options operator.Options, model Model) error {
	model.layout = true
	model.color = colorEnabled()
	view := &app{model: model, discover: discover, ctx: ctx, options: options}
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
	// options carry the preflight cache shared by every sweep this view runs.
	options operator.Options
	// dispatched counts sweeps started, and stamps each one, so a result that
	// comes back after a newer one can be recognized and dropped.
	dispatched uint64
	// applied is the stamp of the newest sweep that reached the model.
	applied uint64
	// inFlight counts sweeps started but not yet returned. A tick skips its
	// sweep while any is outstanding: sweeps that take longer than the refresh
	// interval would otherwise pile up, each re-running the whole discovery.
	inFlight int
}

// tickMsg asks for a scheduled refresh.
type tickMsg time.Time

// discoveredMsg carries the result of one completed discovery, stamped with the
// sweep that produced it.
type discoveredMsg struct {
	sweep     uint64
	instances []operator.Instance
	err       error
}

// refreshOptions are the options for a sweep the operator asked for by name.
// Only that sweep re-probes: an agent CLI logged in since the last sweep
// changes neither the plist nor the workflow the cache is keyed on.
func refreshOptions(options operator.Options) operator.Options {
	options.RefreshPreflight = true
	return options
}

func (a *app) Init() tea.Cmd { return tickCmd() }

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(now time.Time) tea.Msg { return tickMsg(now) })
}

// discoverCmd runs discovery off the UI goroutine. Discovery shells out to
// launchctl and may run a preflight per instance, so on the UI goroutine it
// would freeze the view for the whole sweep. The bookkeeping is done here,
// where Update is still on the UI goroutine, and only the sweep itself runs in
// the returned command.
func (a *app) discoverCmd(options operator.Options) tea.Cmd {
	a.dispatched++
	a.inFlight++
	sweep := a.dispatched
	return func() tea.Msg {
		instances, err := a.discover(a.ctx, options)
		return discoveredMsg{sweep: sweep, instances: instances, err: err}
	}
}

func (a *app) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tickMsg:
		if a.inFlight > 0 {
			// The previous sweep is still out. Re-arm the timer only: a second
			// concurrent sweep would repeat every probe the first is already
			// blocked on, and could only report older data.
			return a, tickCmd()
		}
		return a, tea.Batch(a.discoverCmd(a.options), tickCmd())
	case discoveredMsg:
		a.inFlight--
		if message.sweep < a.applied {
			// An older sweep finishing after a newer one. Applying it would
			// replace the current frame with data the view has already moved
			// past, and stamp it with the time it landed rather than the time
			// it was read.
			return a, nil
		}
		a.applied = message.sweep
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
		// An explicit refresh is never skipped, in-flight sweep or not: it is
		// the operator's way to ask for the probes again.
		return a, a.discoverCmd(refreshOptions(a.options))
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
