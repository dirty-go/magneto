package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"unicode"

	alog "github.com/anacrolix/log"
	"golang.org/x/term"
)

// Statuses the renderer recognises to pick which lines stay visible when the
// block is taller than the terminal; any other status counts as active.
const (
	statusQueued = "queued"
	statusFailed = "failed"
	statusDone   = "100.00% | done"
)

// defaultRows is assumed when the terminal height is unknown.
const defaultRows = 24

// progress owns all output while downloads run: one status line per torrent
// plus event messages. On a terminal the status lines form a block that is
// redrawn in place, with events printed above it; otherwise every update and
// event is printed as a plain line. A single mutex orders all writes.
//
// All text is passed through stripCtl, so torrent metadata can't inject
// escape sequences or line breaks (which would also desync drawn).
type progress struct {
	mu     sync.Mutex
	out    io.Writer // status lines and events
	errOut io.Writer // log output
	tty    bool
	rows   func() int // terminal height; nil or <= 0 means unknown
	labels []string   // "[n]", then "[n] <torrent name>" once known
	status []string
	drawn  int // physical block lines currently on screen
}

func newProgress(out, errOut io.Writer, tty bool, n int) *progress {
	p := &progress{out: out, errOut: errOut, tty: tty, labels: make([]string, n), status: make([]string, n)}
	for i := range n {
		p.labels[i] = fmt.Sprintf("[%d]", i+1)
		p.status[i] = statusQueued
	}
	return p
}

// isTerminal reports whether f is an interactive terminal that understands
// cursor movement, i.e. not a pipe, file, /dev/null or TERM=dumb.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd())) && os.Getenv("TERM") != "dumb"
}

// terminalRows returns f's height in rows, or 0 if unknown.
func terminalRows(f *os.File) func() int {
	return func() int {
		_, h, err := term.GetSize(int(f.Fd()))
		if err != nil {
			return 0
		}
		return h
	}
}

// stripCtl makes untrusted text safe to print on one terminal line: invalid
// UTF-8 is replaced and every control character (C0, DEL, C1, so ESC, CSI,
// BEL, \r, \n, \t, ...) is dropped, as are format characters (bidi
// overrides, zero-width characters, BOM, tags) and line/paragraph separators,
// which could visually spoof a line.
func stripCtl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
}

// Update sets torrent i's line to label followed by status.
func (p *progress) Update(i int, label, status string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.labels[i], p.status[i] = stripCtl(label), stripCtl(status)
	if !p.tty {
		_, _ = fmt.Fprintf(p.out, "%s %s\n", p.labels[i], p.status[i]) // nowhere to report a stdout failure
		return
	}
	p.redraw(nil, false)
}

// Fail marks torrent i's line as failed. Plain output gets no extra line: the
// failure itself is logged.
func (p *progress) Fail(i int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status[i] = statusFailed
	if p.tty {
		p.redraw(nil, false)
	}
}

// Printf prints an event line to out, above the block.
func (p *progress) Printf(format string, args ...any) {
	p.event(false, fmt.Sprintf(format, args...))
}

// slogger returns a logger for the torrent client's own logging that writes
// warnings and above through p, so they are sanitized and land above the block.
func (p *progress) slogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(logWriter{p}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// alogger returns an anacrolix/log Logger feeding p.slogger, wired the same way
// as Client.getLoggers. It replaces alog.Default, which some dependencies
// (e.g. the DHT server's bad-packet log) write to directly, bypassing the
// client config. The Debug default level is required: SlogHandlerAsHandler
// panics on records with no level, and it also keeps level-less messages
// below the Warn filter.
func (p *progress) alogger() alog.Logger {
	l := alog.NewLogger().WithDefaultLevel(alog.Debug)
	l.SetHandlers(alog.SlogHandlerAsHandler{SlogHandler: p.slogger().Handler()})
	return l
}

// logWriter routes log output to errOut through p, so it lands above the
// block instead of corrupting it.
type logWriter struct{ p *progress }

func (l logWriter) Write(b []byte) (int, error) {
	l.p.event(true, string(b))
	return len(b), nil
}

// event prints msg as exactly one line, to errOut if toErr, else to out.
func (p *progress) event(toErr bool, msg string) {
	line := []byte(stripCtl(msg) + "\n")
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.tty {
		w := p.out
		if toErr {
			w = p.errOut
		}
		_, _ = w.Write(line) // nowhere to report a stdout/stderr failure
		return
	}
	p.redraw(line, toErr)
}

// redraw erases the block, writes msg (if any) in its place, to errOut if
// toErr, else to out, then draws the block below it, leaving the cursor on the
// line after the block. Callers must hold p.mu.
//
// With stderr redirected but stdout a terminal, a log line just erases and
// redraws the block with nothing visible in between. When out and errOut are
// different fds, ordering is only as good as the terminal's; any interleaving
// glitch is cosmetic.
func (p *progress) redraw(msg []byte, toErr bool) {
	var b bytes.Buffer
	maxLines := p.maxLines()
	// Clamped: if the terminal shrank, cursor-up can't go past the top anyway.
	if n := min(p.drawn, maxLines); n > 0 {
		// \r first: a ^C echoed by the terminal may have moved the cursor right.
		fmt.Fprintf(&b, "\r\x1b[%dA\x1b[J", n)
	}
	switch {
	case msg == nil:
	case !toErr:
		b.Write(msg)
	default: // erase on out, message on errOut, then redraw on out
		_, _ = p.out.Write(b.Bytes())
		b.Reset()
		_, _ = p.errOut.Write(msg)
	}
	lines := p.block(maxLines)
	// Autowrap off while drawing: an overlong line is clipped at the terminal
	// edge instead of wrapping and throwing off the cursor-up count.
	b.WriteString("\x1b[?7l")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	b.WriteString("\x1b[?7h")
	_, _ = p.out.Write(b.Bytes())
	p.drawn = len(lines)
}

// maxLines returns how many block lines fit in the terminal with a row to
// spare (cursor-up can't reach above the screen).
func (p *progress) maxLines() int {
	if p.rows != nil {
		if r := p.rows(); r > 0 {
			return r - 1
		}
	}
	return defaultRows - 1
}

// block returns the lines to draw: every torrent if they fit in maxLines,
// otherwise as many as fit, active and failed ones first, plus a summary of
// the rest.
func (p *progress) block(maxLines int) []string {
	n := len(p.labels)
	if n <= maxLines {
		lines := make([]string, n)
		for i := range n {
			lines[i] = p.labels[i] + " " + p.status[i]
		}
		return lines
	}

	slots := max(maxLines-1, 0) // one row for the summary
	show := make([]bool, n)
	for _, urgent := range []bool{true, false} {
		for i := 0; i < n && slots > 0; i++ {
			st := p.status[i]
			if !show[i] && (st != statusQueued && st != statusDone) == urgent {
				show[i] = true
				slots--
			}
		}
	}
	var lines []string
	var hidden, active, failed, done, queued int
	for i := range n {
		if show[i] {
			lines = append(lines, p.labels[i]+" "+p.status[i])
			continue
		}
		hidden++
		switch p.status[i] {
		case statusQueued:
			queued++
		case statusDone:
			done++
		case statusFailed:
			failed++
		default:
			active++
		}
	}
	var parts []string
	for _, c := range []struct {
		n    int
		name string
	}{{active, "active"}, {failed, "failed"}, {done, "done"}, {queued, "queued"}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.name))
		}
	}
	return append(lines, fmt.Sprintf("… %d more (%s)", hidden, strings.Join(parts, ", ")))
}
