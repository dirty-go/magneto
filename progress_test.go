package main

import (
	"bytes"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	alog "github.com/anacrolix/log"
)

var ansiSeq = regexp.MustCompile(`\r|\x1b\[(\??\d*)([AJhl])`)

// screen replays out as a minimal terminal that understands exactly the
// sequences progress emits, returning the visible lines and the cursor row.
func screen(t *testing.T, out string) (lines []string, row int) {
	t.Helper()
	lines = []string{""}
	put := func(text string) {
		for i, part := range strings.Split(text, "\n") {
			if i > 0 {
				row++
				if row == len(lines) {
					lines = append(lines, "")
				}
			}
			lines[row] += part
		}
	}
	last := 0
	for _, m := range ansiSeq.FindAllStringSubmatchIndex(out, -1) {
		put(out[last:m[0]])
		last = m[1]
		switch seq := out[m[0]:m[1]]; {
		case seq == "\r":
			lines[row] = "" // the only use of \r is right before an erase
		case strings.HasSuffix(seq, "A"):
			var n int
			_, _ = fmt.Sscanf(out[m[2]:m[3]], "%d", &n)
			row -= n
		case strings.HasSuffix(seq, "J"):
			lines = lines[:row+1]
			lines[row] = ""
		}
	}
	put(out[last:])
	return lines, row
}

func TestProgressTTY(t *testing.T) {
	var out, errOut bytes.Buffer
	p := newProgress(&out, &errOut, true, 3)
	p.Update(0, "[1]", "fetching metadata...")
	p.Update(1, "[2]", "fetching metadata...")
	p.Printf("[1] Show - 01 downloading: a.mkv")
	p.Update(0, "[1] Show - 01", " 42.10% | 295/701 MB | peers: 12")
	log.New(logWriter{p}, "", 0).Print("[3] failed: boom")
	p.Fail(2)
	p.Update(1, "[2] Show - 02", "100.00% | done")
	p.Printf("Interrupted, progress saved")

	// Every erase moves up exactly the block height: lines are stable.
	for _, m := range regexp.MustCompile(`\x1b\[(\d+)A`).FindAllStringSubmatch(out.String(), -1) {
		if m[1] != "3" {
			t.Fatalf("cursor-up by %s lines, want 3", m[1])
		}
	}
	if got := errOut.String(); got != "[3] failed: boom\n" {
		t.Errorf("stderr = %q", got)
	}

	// stderr is a separate buffer here, so its line is absent from stdout's screen.
	lines, row := screen(t, out.String())
	want := []string{
		"[1] Show - 01 downloading: a.mkv",
		"Interrupted, progress saved",
		"[1] Show - 01  42.10% | 295/701 MB | peers: 12",
		"[2] Show - 02 100.00% | done",
		"[3] failed",
		"",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("screen:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if row != len(want)-1 {
		t.Errorf("cursor on row %d, want %d (below the block)", row, len(want)-1)
	}
}

func TestProgressPlain(t *testing.T) {
	var out, errOut bytes.Buffer
	p := newProgress(&out, &errOut, false, 2)
	p.Update(0, "[1]", "fetching metadata...")
	p.Printf("[1] Show downloading: a.mkv")
	p.Update(0, "[1] Show", " 42.10% | 295/701 MB | peers: 12")
	p.Update(0, "[1] Show", " 50.00% | 350/701 MB | peers: 12")
	p.Fail(1)
	log.New(logWriter{p}, "", 0).Print("[2] failed: boom")

	if strings.Contains(out.String()+errOut.String(), "\x1b") {
		t.Fatalf("ANSI escape in plain output: %q", out.String())
	}
	want := "[1] fetching metadata...\n" +
		"[1] Show downloading: a.mkv\n" +
		"[1] Show  42.10% | 295/701 MB | peers: 12\n" +
		"[1] Show  50.00% | 350/701 MB | peers: 12\n"
	if out.String() != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", out.String(), want)
	}
	if errOut.String() != "[2] failed: boom\n" {
		t.Errorf("stderr = %q", errOut.String())
	}
}

// TestProgressConcurrent is for -race: concurrent updates, events and log
// writes must not race and must leave every line in its final state.
func TestProgressConcurrent(t *testing.T) {
	var out bytes.Buffer
	const n = 4
	p := newProgress(&out, &out, true, n)
	logger := log.New(logWriter{p}, "", 0)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			tag := fmt.Sprintf("[%d]", i+1)
			for pct := range 100 {
				p.Update(i, tag, fmt.Sprintf("%6.2f%%", float64(pct)))
				if pct%25 == 0 {
					p.Printf("%s event %d", tag, pct)
					logger.Printf("%s log %d", tag, pct)
				}
			}
			p.Update(i, tag, "100.00% | done")
		})
	}
	wg.Wait()

	lines, _ := screen(t, out.String())
	block := lines[len(lines)-1-n : len(lines)-1]
	for i, l := range block {
		if want := fmt.Sprintf("[%d] 100.00%% | done", i+1); l != want {
			t.Errorf("block line %d = %q, want %q", i, l, want)
		}
	}
	if got, want := len(lines)-1-n, n*4*2; got != want {
		t.Errorf("%d event lines above the block, want %d", got, want)
	}
}

// cursorUps returns the line counts of every cursor-up the renderer emitted.
func cursorUps(out string) []string {
	var ups []string
	for _, m := range regexp.MustCompile(`\x1b\[(\d+)A`).FindAllStringSubmatch(out, -1) {
		ups = append(ups, m[1])
	}
	return ups
}

// TestProgressControlChars feeds hostile torrent metadata (OSC 52 clipboard
// write, line breaks, C1 CSI, invalid UTF-8, bidi override, line separator)
// through every entry point: none
// of it may reach the output, and each message must stay one line.
func TestProgressControlChars(t *testing.T) {
	const evil = "a\x1b]52;c;AAAA\x07b\nc\rd\u009b2J\xffe\u202ef\u2028g"
	const clean = "a]52;c;AAAAbcd2J\uFFFDefg"
	own := regexp.MustCompile(`\r\x1b\[\d+A\x1b\[J|\x1b\[\?7[lh]`) // the renderer's escapes

	for _, tty := range []bool{true, false} {
		t.Run(fmt.Sprintf("tty=%v", tty), func(t *testing.T) {
			var out, errOut bytes.Buffer
			p := newProgress(&out, &errOut, tty, 2)
			p.Update(0, "[1] "+evil, evil)
			p.Printf("%s", evil)
			log.New(logWriter{p}, "", 0).Print(evil)
			p.Update(1, "[2]", evil)

			for name, s := range map[string]string{"stdout": own.ReplaceAllString(out.String(), ""), "stderr": errOut.String()} {
				if strings.ContainsAny(s, "\x1b\x07\r\u009b\u202e\u2028") || !utf8.ValidString(s) {
					t.Errorf("%s leaks control bytes: %q", name, s)
				}
			}
			if got := errOut.String(); got != clean+"\n" {
				t.Errorf("stderr = %q, want %q", got, clean+"\n")
			}
			if !tty {
				want := "[1] " + clean + " " + clean + "\n" + clean + "\n[2] " + clean + "\n"
				if out.String() != want {
					t.Errorf("stdout = %q, want %q", out.String(), want)
				}
				return
			}
			for _, n := range cursorUps(out.String()) {
				if n != "2" {
					t.Fatalf("cursor-up by %s lines, want 2", n)
				}
			}
			lines, _ := screen(t, out.String())
			want := []string{clean, "[1] " + clean + " " + clean, "[2] " + clean, ""}
			if strings.Join(lines, "\n") != strings.Join(want, "\n") {
				t.Errorf("screen = %q, want %q", lines, want)
			}
		})
	}
}

// TestProgressSlogger checks the torrent client's logging, through both its
// slog and legacy anacrolix/log paths (wired as Client.getLoggers does when
// only ClientConfig.Slogger is set), drops debug and info and prints warnings
// as one sanitized line on errOut.
func TestProgressSlogger(t *testing.T) {
	const evil = "a\x1b]52;c;AAAA\x07b\nc\u202ed\u2028e"
	for name, logf := range map[string]func(p *progress){
		"slog": func(p *progress) {
			l := p.slogger()
			l.Debug("debug")
			l.Info("info")
			l.Warn(evil)
		},
		"anacrolix": func(p *progress) {
			l := p.alogger()
			l.Printf("debug")
			l.Levelf(alog.Info, "info")
			l.Levelf(alog.Warning, "%s", evil)
		},
		"anacrolix global": func(p *progress) {
			saved := alog.Default
			defer func() { alog.Default = saved }()
			alog.Default = p.alogger()
			// Level-less, like the DHT server's bad-packet log: must be
			// dropped, not panic or reach stderr raw.
			alog.Printf("debug %s", evil)
			alog.Levelf(alog.Info, "info")
			alog.Levelf(alog.Warning, "%s", evil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			logf(newProgress(&out, &errOut, false, 1))
			got := errOut.String()
			if out.Len() != 0 {
				t.Errorf("stdout = %q, want empty", out.String())
			}
			if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
				t.Fatalf("stderr = %q, want exactly one line", got)
			}
			if strings.ContainsAny(got, "\x1b\x07\r\u202e\u2028") {
				t.Errorf("stderr leaks control characters: %q", got)
			}
			if !strings.Contains(got, "level=WARN") || strings.Contains(got, "debug") || strings.Contains(got, "info") {
				t.Errorf("stderr = %q, want only the warning", got)
			}
		})
	}
}

// TestProgressTall checks a block taller than the terminal is capped at
// rows-1 lines, keeping active and failed lines and summarising the rest.
func TestProgressTall(t *testing.T) {
	var out bytes.Buffer
	p := newProgress(&out, &out, true, 10)
	p.rows = func() int { return 5 }
	p.Update(0, "[1]", statusDone)
	p.Update(2, "[3]", "fetching metadata...")
	p.Fail(3)
	p.Update(9, "[10]", " 10.00%")

	for _, n := range cursorUps(out.String()) {
		if n != "4" {
			t.Fatalf("cursor-up by %s lines, want 4", n)
		}
	}
	lines, _ := screen(t, out.String())
	want := []string{
		"[3] fetching metadata...",
		"[4] failed",
		"[10]  10.00%",
		"… 7 more (1 done, 6 queued)",
		"",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("screen:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// TestProgressRowsFallback checks the block height when the terminal size is
// unknown (defaultRows) or large enough for every line.
func TestProgressRowsFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows func() int
		want string
	}{
		{"unknown", nil, "23"},
		{"error", func() int { return 0 }, "23"},
		{"fits", func() int { return 50 }, "30"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			p := newProgress(&out, &out, true, 30)
			p.rows = tc.rows
			p.Update(0, "[1]", "a")
			p.Update(0, "[1]", "b")
			if ups := cursorUps(out.String()); len(ups) != 1 || ups[0] != tc.want {
				t.Errorf("cursor-ups = %v, want [%s]", ups, tc.want)
			}
		})
	}
}
