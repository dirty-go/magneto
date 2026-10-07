package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	alog "github.com/anacrolix/log"
	"github.com/anacrolix/torrent"
)

var (
	outputDir = flag.String("out", "", "Download directory (default: Downloads next to main.go; only meaningful with go run)")
	noSeed    = flag.Bool("no-seed", true, "Disable seeding after download")
	parallel  = flag.Int("parallel", 0, "Max simultaneous downloads (0 = unlimited)")
)

// mediaExt is the allow-list of file extensions that get downloaded.
var mediaExt = map[string]bool{
	".mp4":  true,
	".mkv":  true,
	".avi":  true,
	".mp3":  true,
	".flac": true,
	".wav":  true,
}

// magnetList is a flag.Value that collects magnet URIs from repeated -magnet
// flags and from comma-separated values within one flag.
type magnetList []string

func (m *magnetList) String() string { return strings.Join(*m, ",") }

func (m *magnetList) Set(v string) error {
	for s := range strings.SplitSeq(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*m = append(*m, s)
		}
	}
	return nil
}

func main() {
	var magnets magnetList
	flag.Var(&magnets, "magnet", "Magnet link or path to a file of newline-separated magnet links (required); repeat the flag or pass a comma-separated list to download several concurrently")
	flag.Usage = func() {
		_, _ = fmt.Fprintf(flag.CommandLine.Output(), // nothing useful to do if writing usage fails
			"Usage: %s -magnet <uri|file>[,<uri|file>...] [-magnet <uri|file>...] [-parallel N] [-out <dir>] [-no-seed=<bool>]\n\n",
			filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
	flag.Parse()

	if err := run(magnets); err != nil {
		log.Fatal(err)
	}
}

func run(magnets []string) error {
	magnets, err := expandMagnets(magnets)
	if err != nil {
		return err
	}
	if len(magnets) == 0 {
		return errors.New("at least one -magnet link is required")
	}
	magnets = dedupe(magnets)

	dir, err := resolveOutputDir(*outputDir)
	if err != nil {
		return err
	}

	// From here on all output goes through p, so log lines (stderr) print
	// above the progress block instead of corrupting it.
	p := newProgress(os.Stdout, os.Stderr, isTerminal(os.Stdout), len(magnets))
	p.rows = terminalRows(os.Stdout)
	log.SetOutput(logWriter{p})
	alog.Default = p.alogger()

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dir
	cfg.NoUpload = *noSeed
	cfg.ListenPort = 0 // let the OS pick a free port so multiple instances can run concurrently
	// With Slogger set and Logger zero, the client points its legacy Logger at
	// Slogger too (Client.getLoggers), so all library logging goes through p.
	cfg.Slogger = p.slogger()

	client, err := torrent.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("create torrent client: %w", err)
	}
	defer client.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// Once interrupted, restore default SIGINT handling so a second Ctrl+C force-quits.
	context.AfterFunc(ctx, stop)

	var sem chan struct{} // nil = unlimited
	if *parallel > 0 {
		sem = make(chan struct{}, *parallel)
	}

	var (
		wg     sync.WaitGroup
		failed atomic.Int32
	)
	for i, uri := range magnets {
		wg.Go(func() {
			tag := fmt.Sprintf("[%d]", i+1)
			defer func() {
				// One magnet's download must never take the others down with it.
				if r := recover(); r != nil {
					p.Fail(i)
					log.Printf("%s panic: %v", tag, r)
					failed.Add(1)
				}
			}()
			if sem != nil {
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					return
				}
			}
			if err := download(ctx, client, p, i, uri); err != nil && !errors.Is(err, context.Canceled) {
				p.Fail(i)
				log.Printf("%s failed: %v", tag, err)
				failed.Add(1)
			}
		})
	}
	wg.Wait()

	if ctx.Err() != nil {
		p.Printf("Interrupted, progress saved")
	}
	if n := failed.Load(); n > 0 {
		return fmt.Errorf("%d of %d downloads failed", n, len(magnets))
	}
	return nil
}

// dedupe drops repeated magnet URIs, preserving first-seen order. Duplicate
// links would otherwise resolve to the same underlying torrent (the client
// dedupes by infohash) and silently double-count in progress and exit-code
// accounting.
func dedupe(magnets []string) []string {
	seen := make(map[string]bool, len(magnets))
	out := magnets[:0]
	for _, m := range magnets {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// expandMagnets returns entries with every non-magnet entry treated as a path
// to a batch file and replaced by the magnet URIs it lists, one per line.
func expandMagnets(entries []string) ([]string, error) {
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(strings.ToLower(e), "magnet:") {
			out = append(out, e)
			continue
		}
		uris, err := readBatchFile(e)
		if err != nil {
			return nil, err
		}
		out = append(out, uris...)
	}
	return out, nil
}

// maxBatchEntries caps the magnet URIs read from one batch file; each one
// costs a goroutine and a progress line.
const maxBatchEntries = 10_000

// readBatchFile reads newline-separated magnet URIs from path, skipping blank
// lines and a leading UTF-8 BOM. A file with no URIs is an error.
func readBatchFile(path string) ([]string, error) {
	f, errOpen := os.Open(path)
	if errOpen != nil {
		return nil, fmt.Errorf("%q is neither a magnet URI nor a readable file: %w", path, errOpen)
	}
	defer func() { _ = f.Close() }() // read-only; close error carries no data loss

	info, errStat := f.Stat()
	if errStat != nil {
		return nil, fmt.Errorf("stat batch file: %w", errStat)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", path)
	}

	var uris []string
	scanner := bufio.NewScanner(f)
	n := 0
	for scanner.Scan() {
		n++
		line := scanner.Text()
		if n == 1 {
			line = strings.TrimPrefix(line, "\uFEFF")
		}
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(line), "magnet:") {
			return nil, fmt.Errorf("%s:%d: invalid magnet URI %q", path, n, truncate(line, 80))
		}
		if len(uris) == maxBatchEntries {
			return nil, fmt.Errorf("%s:%d: more than %d magnet URIs; split the file", path, n, maxBatchEntries)
		}
		uris = append(uris, line)
	}
	if errScan := scanner.Err(); errScan != nil {
		return nil, fmt.Errorf("%s:%d: %w", path, n+1, errScan)
	}
	if len(uris) == 0 {
		return nil, fmt.Errorf("no magnet URIs in %s", path)
	}
	return uris, nil
}

// maxNameRunes caps torrent names and file paths, which the remote side
// controls, in progress lines and log messages.
const maxNameRunes = 120

// truncate shortens s to at most n runes, marking a cut with "…".
func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// resolveOutputDir returns dir, or when empty a "Downloads" directory next to
// this source file, creating it if needed.
func resolveOutputDir(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	_, filename, _, _ := runtime.Caller(0)
	dir = filepath.Join(filepath.Dir(filename), "Downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create download directory: %w", err)
	}
	return dir, nil
}

// download fetches the media files of magnet i on the shared client, reporting
// to p, and blocks until they are complete or ctx is cancelled (returning
// ctx.Err()).
func download(ctx context.Context, client *torrent.Client, p *progress, i int, uri string) error {
	tag := fmt.Sprintf("[%d]", i+1)
	t, err := client.AddMagnet(uri)
	if err != nil {
		return fmt.Errorf("add magnet: %w", err)
	}
	defer t.Drop()

	p.Update(i, tag, "fetching metadata...")
	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		return ctx.Err()
	}
	tag += " " + truncate(stripCtl(t.Name()), maxNameRunes)

	var (
		selected []*torrent.File
		total    int64
	)
	for _, f := range t.Files() {
		if !mediaExt[strings.ToLower(filepath.Ext(f.Path()))] {
			continue
		}
		p.Printf("%s downloading: %s", tag, truncate(stripCtl(f.Path()), maxNameRunes))
		f.Download()
		selected = append(selected, f)
		total += f.Length()
	}
	if len(selected) == 0 {
		return errors.New("no media files found in torrent")
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		// t.Complete() only fires once every piece is present, which never
		// happens when non-media files are skipped, so track selected files.
		var done int64
		for _, f := range selected {
			done += f.BytesCompleted()
		}
		if done >= total {
			p.Update(i, tag, statusDone)
			return nil
		}
		p.Update(i, tag, fmt.Sprintf("%6.2f%% | %d/%d MB | peers: %d",
			float64(done)/float64(total)*100, done>>20, total>>20, t.Stats().ActivePeers))
	}
}
