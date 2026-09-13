package main

import (
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

// console serializes stdout writes: log.Logger emits each message in a single
// locked Write, so concurrent downloads never interleave mid-line.
var console = log.New(os.Stdout, "", 0)

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
	flag.Var(&magnets, "magnet", "Magnet link (required); repeat the flag or pass a comma-separated list to download several concurrently")
	flag.Usage = func() {
		_, _ = fmt.Fprintf(flag.CommandLine.Output(), // nothing useful to do if writing usage fails
			"Usage: %s -magnet <uri>[,<uri>...] [-magnet <uri>...] [-parallel N] [-out <dir>] [-no-seed=<bool>]\n\n",
			filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
	flag.Parse()

	if err := run(magnets); err != nil {
		log.Fatal(err)
	}
}

func run(magnets []string) error {
	if len(magnets) == 0 {
		return errors.New("at least one -magnet link is required")
	}
	magnets = dedupe(magnets)

	dir, err := resolveOutputDir(*outputDir)
	if err != nil {
		return err
	}

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dir
	cfg.NoUpload = *noSeed
	cfg.ListenPort = 0 // let the OS pick a free port so multiple instances can run concurrently

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
			if err := download(ctx, client, tag, uri); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("%s failed: %v", tag, err)
				failed.Add(1)
			}
		})
	}
	wg.Wait()

	if ctx.Err() != nil {
		console.Println("Interrupted, progress saved")
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

// download fetches the media files of one magnet on the shared client and
// blocks until they are complete or ctx is cancelled (returning ctx.Err()).
func download(ctx context.Context, client *torrent.Client, tag, uri string) error {
	t, err := client.AddMagnet(uri)
	if err != nil {
		return fmt.Errorf("add magnet: %w", err)
	}
	defer t.Drop()

	console.Printf("%s fetching metadata...", tag)
	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		return ctx.Err()
	}
	tag += " " + t.Name()

	var (
		selected []*torrent.File
		total    int64
	)
	for _, f := range t.Files() {
		if !mediaExt[strings.ToLower(filepath.Ext(f.Path()))] {
			continue
		}
		console.Printf("%s downloading: %s", tag, f.Path())
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
			console.Printf("%s download completed", tag)
			return nil
		}
		console.Printf("%s %6.2f%% | %d/%d MB | peers: %d",
			tag, float64(done)/float64(total)*100, done>>20, total>>20, t.Stats().ActivePeers)
	}
}
