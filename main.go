package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	alog "github.com/anacrolix/log"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
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

// stringList is a flag.Value that collects values from repeated flags and
// from comma-separated values within one flag.
type stringList []string

func (m *stringList) String() string { return strings.Join(*m, ",") }

func (m *stringList) Set(v string) error {
	for s := range strings.SplitSeq(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*m = append(*m, s)
		}
	}
	return nil
}

func main() {
	var magnets, torrents stringList
	flag.Var(&magnets, "magnet", "Magnet link or path to a batch file of newline-separated magnet links, .torrent paths and http(s) .torrent URLs; repeat the flag or pass a comma-separated list to download several concurrently")
	flag.Var(&torrents, "torrent", "Path to a local .torrent file or an http(s) URL of one; repeat the flag or pass a comma-separated list")
	flag.Usage = func() {
		_, _ = fmt.Fprintf(flag.CommandLine.Output(), // nothing useful to do if writing usage fails
			"Usage: %s [-magnet <uri|file>[,...]]... [-torrent <file|url>[,...]]... [-parallel N] [-out <dir>] [-no-seed=<bool>]\n"+
				"At least one -magnet or -torrent is required.\n\n",
			filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
	flag.Parse()

	if err := run(magnets, torrents); err != nil {
		log.Fatal(err)
	}
}

func run(magnets, torrents []string) error {
	refs, err := expandMagnets(magnets)
	if err != nil {
		return err
	}
	for _, t := range torrents {
		refs = append(refs, sourceRef{ref: t, torrent: true})
	}
	if len(refs) == 0 {
		return errors.New("at least one -magnet or -torrent is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// Once interrupted, restore default SIGINT handling so a second Ctrl+C force-quits.
	context.AfterFunc(ctx, stop)

	sources := dedupe(resolveSources(ctx, newHTTPClient(fetchTimeout), refs))
	if ctx.Err() != nil {
		return errors.New("interrupted before downloads started")
	}

	dir, err := resolveOutputDir(*outputDir)
	if err != nil {
		return err
	}

	// From here on all output goes through p, so log lines (stderr) print
	// above the progress block instead of corrupting it.
	p := newProgress(os.Stdout, os.Stderr, isTerminal(os.Stdout), len(sources))
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

	var sem chan struct{} // nil = unlimited
	if *parallel > 0 {
		sem = make(chan struct{}, *parallel)
	}

	var (
		wg     sync.WaitGroup
		failed atomic.Int32
	)
	for i, src := range sources {
		tag := fmt.Sprintf("[%d]", i+1)
		if src.err != nil {
			p.Fail(i)
			log.Printf("%s failed: %v", tag, src.err)
			failed.Add(1)
			continue
		}
		wg.Go(func() {
			defer func() {
				// One torrent's download must never take the others down with it.
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
			if err := download(ctx, client, p, i, src.spec); err != nil && !errors.Is(err, context.Canceled) {
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
		return fmt.Errorf("%d of %d downloads failed", n, len(sources))
	}
	return nil
}

// source is one torrent to download, as given on the command line. It is
// resolved to a spec up front so duplicates can be dropped by infohash; a
// source that fails to resolve keeps its error and is reported as one failed
// download without stopping the others.
type source struct {
	ref  string               // the magnet URI, file path or redacted URL given, for messages
	spec *torrent.TorrentSpec // nil when err != nil
	err  error
}

// sourceRef is one unresolved source: a magnet URI, or a .torrent file path or
// URL.
type sourceRef struct {
	ref     string
	torrent bool // ref is a .torrent file path or URL, not a magnet URI
}

// resolveSources resolves refs (already expanded from batch files) in order,
// skipping exact repeats so a URL or file listed twice is only read once.
// URLs are fetched with hc, one at a time, and abandoned when ctx is
// cancelled.
func resolveSources(ctx context.Context, hc *http.Client, refs []sourceRef) []source {
	out := make([]source, 0, len(refs))
	seen := make(map[sourceRef]bool, len(refs))
	for _, r := range refs {
		if seen[r] {
			continue
		}
		seen[r] = true
		if !r.torrent {
			out = append(out, resolveMagnet(r.ref))
			continue
		}
		u, err := parseTorrentURL(r.ref)
		switch {
		case err != nil: // r.ref may hold credentials; err has it redacted
			out = append(out, source{ref: "(rejected URL)", err: err})
		case u != nil:
			out = append(out, resolveTorrentURL(ctx, hc, u))
		default:
			out = append(out, resolveTorrentFile(r.ref))
		}
	}
	return out
}

// resolveMagnet parses a magnet URI into a source.
func resolveMagnet(uri string) source {
	spec, err := torrent.TorrentSpecFromMagnetUri(uri)
	if err == nil && len(infohashKeys(spec)) == 0 {
		err = errors.New("no infohash")
	}
	if err != nil {
		return source{ref: uri, err: fmt.Errorf("invalid magnet URI %q: %w", truncate(uri, 80), err)}
	}
	return source{ref: uri, spec: spec}
}

// resolveTorrentFile loads a local .torrent file into a source.
func resolveTorrentFile(path string) source {
	spec, err := loadTorrentFile(path)
	if err != nil {
		return source{ref: path, err: fmt.Errorf("torrent file %q: %w", path, err)}
	}
	return source{ref: path, spec: spec}
}

// resolveTorrentURL fetches the .torrent file at u into a source.
func resolveTorrentURL(ctx context.Context, hc *http.Client, u *url.URL) source {
	ref := redactURL(u)
	spec, err := fetchTorrent(ctx, hc, u)
	if err != nil {
		return source{ref: ref, err: fmt.Errorf("torrent URL %q: %w", ref, err)}
	}
	return source{ref: ref, spec: spec}
}

// Limits on fetching .torrent URLs.
const (
	fetchTimeout = 30 * time.Second // whole request, body included
	maxRedirects = 5
)

// newHTTPClient returns the client .torrent URLs are fetched with: requests
// time out after timeout, and redirects are capped, must stay on http(s) and
// can't downgrade from https. No Referer is sent, so a token in the query
// doesn't leak to the redirect target.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("more than %d redirects", maxRedirects)
			}
			if !isHTTPScheme(req.URL.Scheme) {
				return fmt.Errorf("redirect to unsupported scheme %q", truncate(req.URL.Scheme, 20))
			}
			if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
				return errors.New("redirect from https to http")
			}
			req.Header.Del("Referer")
			return nil
		},
	}
}

func isHTTPScheme(s string) bool { return s == "http" || s == "https" }

// parseTorrentURL returns the URL in ref, or nil when ref is a local path.
// Only "scheme://..." counts as a URL, so a Windows path like C:\x.torrent stays
// a path. A URL whose scheme isn't http or https is an error. Errors carry the
// URL redacted.
func parseTorrentURL(ref string) (*url.URL, error) {
	scheme, _, ok := strings.Cut(ref, "://")
	if !ok || !isURLScheme(scheme) {
		return nil, nil
	}
	u, err := url.Parse(ref)
	if err != nil {
		// url.Parse's error repeats ref, credentials and all.
		return nil, errors.New("invalid torrent URL")
	}
	if !isHTTPScheme(u.Scheme) { // Parse lowercases the scheme
		return nil, fmt.Errorf("torrent URL %q: unsupported scheme %q; only http and https are allowed", redactURL(u), truncate(u.Scheme, 20))
	}
	if u.Host == "" {
		return nil, fmt.Errorf("torrent URL %q: no host", redactURL(u))
	}
	return u, nil
}

// isURLScheme reports whether s is a syntactically valid URI scheme
// (RFC 3986: a letter, then letters, digits, "+", "-" or ".").
func isURLScheme(s string) bool {
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return s != ""
}

// redactURL returns u for messages, with any userinfo and query replaced by
// "REDACTED" and the fragment dropped, since those commonly carry tokens.
func redactURL(u *url.URL) string {
	r := *u
	if r.User != nil {
		r.User = url.User("REDACTED")
	}
	if r.RawQuery != "" {
		r.RawQuery = "REDACTED"
	}
	r.Fragment, r.RawFragment = "", ""
	return truncate(stripCtl(r.String()), 200)
}

// fetchTorrent downloads and validates the .torrent file at u. Errors don't
// repeat the URL; the caller adds it redacted.
func fetchTorrent(ctx context.Context, hc *http.Client, u *url.URL) (*torrent.TorrentSpec, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("invalid request")
	}
	req.Header.Set("User-Agent", "magneto")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, urlErrCause(err)
	}
	defer func() { _ = resp.Body.Close() }() // read-only; close error carries no data loss

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Not resp.Status: its text comes from the server.
		return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if resp.ContentLength > maxTorrentFileSize {
		return nil, fmt.Errorf("larger than %d MiB", maxTorrentFileSize>>20)
	}
	// Content-Length may be absent or wrong, so the read is capped as well.
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxTorrentFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", urlErrCause(err))
	}
	if len(b) > maxTorrentFileSize {
		return nil, fmt.Errorf("larger than %d MiB", maxTorrentFileSize>>20)
	}
	return parseTorrent(b)
}

// urlErrCause unwraps a *url.Error, whose message repeats the request URL
// with its credentials and query.
func urlErrCause(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}
	return err
}

// Limits on .torrent files, which are untrusted input. Real ones are rarely
// more than a few MB or nest more than a handful of levels, even for huge
// multi-file torrents.
const (
	maxTorrentFileSize = 8 << 20
	// The bencode decoder recurses per nesting level, and a Go stack overflow
	// is fatal (not recoverable), so nesting is bounded before decoding.
	maxBencodeDepth = 64
	// Bounds decode time and memory: each list/dict costs allocations.
	maxBencodeContainers = 200_000
	// The client creates every zero-length file on disk when the torrent is
	// added, whatever the media filter selects.
	maxTorrentFiles = 50_000
)

// loadTorrentFile reads and validates the .torrent file at path. Errors don't
// repeat the path; the caller adds it.
func loadTorrentFile(path string) (*torrent.TorrentSpec, error) {
	// Stat before Open: opening a FIFO would block.
	st, err := os.Stat(path)
	if err != nil {
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			err = pe.Err // keeps errors.Is(err, fs.ErrNotExist) working
		}
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if st.Size() > maxTorrentFileSize {
		return nil, fmt.Errorf("larger than %d MiB", maxTorrentFileSize>>20)
	}

	f, err := os.Open(path)
	if err != nil {
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			err = pe.Err
		}
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only; close error carries no data loss

	// The limit also guards against the file growing after Stat.
	b, err := io.ReadAll(io.LimitReader(f, maxTorrentFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if len(b) > maxTorrentFileSize {
		return nil, fmt.Errorf("larger than %d MiB", maxTorrentFileSize>>20)
	}
	return parseTorrent(b)
}

// parseTorrent decodes and validates the .torrent file contents b.
func parseTorrent(b []byte) (*torrent.TorrentSpec, error) {
	if err := checkBencodeShape(b); err != nil {
		return nil, fmt.Errorf("not a valid .torrent file: %w", err)
	}
	mi, err := metainfo.Load(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("not a valid .torrent file: %w", err)
	}
	if len(mi.InfoBytes) == 0 {
		return nil, errors.New("not a valid .torrent file: no info dictionary")
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return nil, fmt.Errorf("not a valid .torrent file: %w", err)
	}
	if n := countFiles(&info); n > maxTorrentFiles {
		return nil, fmt.Errorf("more than %d files", maxTorrentFiles)
	}
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return nil, fmt.Errorf("not a valid .torrent file: %w", err)
	}
	if len(infohashKeys(spec)) == 0 {
		return nil, errors.New("not a valid .torrent file: no v1 or v2 info")
	}
	return spec, nil
}

// checkBencodeShape walks b without recursing and rejects nesting deeper than
// maxBencodeDepth or more than maxBencodeContainers lists and dicts. It only
// tracks structure: on malformed input it stops and leaves the error to the
// real decoder.
func checkBencodeShape(b []byte) error {
	depth, containers := 0, 0
	for i := 0; i < len(b); {
		switch c := b[i]; {
		case c == 'l' || c == 'd':
			if depth++; depth > maxBencodeDepth {
				return fmt.Errorf("nested deeper than %d levels", maxBencodeDepth)
			}
			if containers++; containers > maxBencodeContainers {
				return fmt.Errorf("more than %d lists and dictionaries", maxBencodeContainers)
			}
			i++
		case c == 'e' && depth > 0:
			depth--
			i++
		case c == 'i':
			j := bytes.IndexByte(b[i:], 'e')
			if j < 0 {
				return nil
			}
			i += j + 1
		case c >= '0' && c <= '9': // <len>:<bytes>
			j := bytes.IndexByte(b[i:], ':')
			if j < 0 {
				return nil
			}
			n, err := strconv.Atoi(string(b[i : i+j]))
			if err != nil || n > len(b)-(i+j+1) {
				return nil
			}
			i += j + 1 + n
		default:
			return nil
		}
	}
	return nil
}

// countFiles returns the number of files in info, taking the larger of the v1
// list and the v2 tree so hybrid torrents can't hide files in either.
func countFiles(info *metainfo.Info) int {
	n := 0
	for range info.UpvertedFilesIter() {
		n++
	}
	return max(n, len(info.Files))
}

// infohashKeys returns the dedupe keys of spec: its v1 and/or v2 infohash.
func infohashKeys(spec *torrent.TorrentSpec) []string {
	var keys []string
	if !spec.InfoHash.IsZero() {
		keys = append(keys, "v1:"+spec.InfoHash.HexString())
	}
	if spec.InfoHashV2.Ok {
		keys = append(keys, "v2:"+spec.InfoHashV2.Value.HexString())
	}
	return keys
}

// dedupe drops sources whose torrent was already given, matched by infohash,
// preserving first-seen order; so the same torrent passed as a magnet and as
// a .torrent file is downloaded once. Duplicates would otherwise resolve to
// the same client torrent and double-count in progress and exit-code
// accounting. When a duplicate carries the info dictionary and the kept
// source doesn't, the duplicate's spec is used, skipping the metadata fetch.
// Unresolved sources are kept as they are.
func dedupe(sources []source) []source {
	seen := make(map[string]int) // infohash key -> index in out
	out := sources[:0]
	for _, s := range sources {
		if s.err != nil {
			out = append(out, s)
			continue
		}
		keys := infohashKeys(s.spec)
		j := -1
		for _, k := range keys {
			if idx, ok := seen[k]; ok {
				j = idx
				break
			}
		}
		if j < 0 {
			j = len(out)
			out = append(out, s)
		} else if out[j].spec.InfoBytes == nil && s.spec.InfoBytes != nil {
			out[j] = s
		}
		for _, k := range keys {
			seen[k] = j
		}
	}
	return out
}

// expandMagnets returns entries with every non-magnet entry treated as a path
// to a batch file and replaced by the sources it lists, one per line.
func expandMagnets(entries []string) ([]sourceRef, error) {
	var out []sourceRef
	for _, e := range entries {
		if isMagnet(e) {
			out = append(out, sourceRef{ref: e})
			continue
		}
		refs, err := readBatchFile(e)
		if err != nil {
			return nil, err
		}
		out = append(out, refs...)
	}
	return out, nil
}

func isMagnet(s string) bool { return strings.HasPrefix(strings.ToLower(s), "magnet:") }

// maxBatchEntries caps the entries read from one batch file; each one costs a
// goroutine and a progress line.
const maxBatchEntries = 10_000

// readBatchFile reads newline-separated sources from path, skipping blank
// lines and a leading UTF-8 BOM. A line is a magnet URI, an http(s) .torrent
// URL or a .torrent file path, relative to path's directory unless absolute;
// it can't name another batch file. A file with no entries is an error.
func readBatchFile(path string) ([]sourceRef, error) {
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

	var refs []sourceRef
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
		ref, err := parseBatchLine(filepath.Dir(path), line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if len(refs) == maxBatchEntries {
			return nil, fmt.Errorf("%s:%d: more than %d entries; split the file", path, n, maxBatchEntries)
		}
		refs = append(refs, ref)
	}
	if errScan := scanner.Err(); errScan != nil {
		return nil, fmt.Errorf("%s:%d: %w", path, n+1, errScan)
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("no entries in %s", path)
	}
	return refs, nil
}

// parseBatchLine classifies one non-blank batch-file line, resolving a
// relative .torrent path against dir.
func parseBatchLine(dir, line string) (sourceRef, error) {
	if isMagnet(line) {
		return sourceRef{ref: line}, nil
	}
	u, err := parseTorrentURL(line)
	if err != nil {
		return sourceRef{}, err // redacted: line may hold credentials
	}
	if u != nil {
		return sourceRef{ref: line, torrent: true}, nil
	}
	if !strings.EqualFold(filepath.Ext(line), ".torrent") {
		const want = "want a magnet URI, .torrent path or http(s) URL"
		if strings.ContainsAny(line, ":?@") {
			// Possibly a malformed URL (e.g. "https:/h?token=x") that
			// parseTorrentURL didn't recognize, so don't echo it.
			return sourceRef{}, errors.New("invalid entry: " + want)
		}
		return sourceRef{}, fmt.Errorf("invalid entry %q: %s", truncate(line, 80), want)
	}
	if !filepath.IsAbs(line) {
		line = filepath.Join(dir, line)
	}
	return sourceRef{ref: line, torrent: true}, nil
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

// download fetches the media files of torrent i on the shared client,
// reporting to p, and blocks until they are complete or ctx is cancelled
// (returning ctx.Err()).
func download(ctx context.Context, client *torrent.Client, p *progress, i int, spec *torrent.TorrentSpec) error {
	tag := fmt.Sprintf("[%d]", i+1)
	t, _, err := client.AddTorrentSpec(spec)
	if err != nil {
		return fmt.Errorf("add torrent: %w", err)
	}
	defer t.Drop()

	if spec.DisplayName != "" { // known up front for .torrent files
		p.Update(i, tag+" "+truncate(stripCtl(spec.DisplayName), maxNameRunes), "fetching metadata...")
	} else {
		p.Update(i, tag, "fetching metadata...")
	}
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
