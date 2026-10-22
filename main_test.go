package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

func TestExpandMagnets(t *testing.T) {
	lines := func(n int) string { return strings.Repeat("magnet:?xt=a\n", n) }
	m := func(uris ...string) []sourceRef {
		out := make([]sourceRef, len(uris))
		for i, u := range uris {
			out[i] = sourceRef{ref: u}
		}
		return out
	}
	tor := func(ref string) sourceRef { return sourceRef{ref: ref, torrent: true} }
	abs := filepath.Join(t.TempDir(), "abs.torrent")
	tests := []struct {
		name    string
		file    *string // contents of the batch file; nil = not created
		path    string  // batch file path relative to the temp dir; "" = list.txt
		args    []string
		want    []sourceRef // "$DIR" in a ref is replaced by the temp dir
		wantErr string
		wantIs  error
	}{
		{name: "raw URIs only", args: []string{"magnet:?a", "MAGNET:?b"}, want: m("magnet:?a", "MAGNET:?b")},
		{name: "missing file", args: []string{"list.txt"}, wantErr: "neither a magnet URI nor a readable file", wantIs: os.ErrNotExist},
		{name: "directory", args: []string{"."}, wantErr: "is not a regular file"},
		{
			name: "mixed lines",
			file: ptr("magnet:?a\nhttps://h/x.torrent?k=1\nlocal.torrent\nUP.TORRENT\n" + abs + "\nHTTP://h/dl\nmagnet:?b\n"),
			args: []string{"list.txt"},
			want: []sourceRef{{ref: "magnet:?a"}, tor("https://h/x.torrent?k=1"), tor("$DIR/local.torrent"), tor("$DIR/UP.TORRENT"), tor(abs), tor("HTTP://h/dl"), {ref: "magnet:?b"}},
		},
		{
			name: "relative to batch file dir",
			file: ptr("a.torrent\n../b.torrent\nc/d.torrent\n"),
			path: "sub/list.txt",
			args: []string{"sub/list.txt"},
			want: []sourceRef{tor("$DIR/sub/a.torrent"), tor("$DIR/b.torrent"), tor("$DIR/sub/c/d.torrent")},
		},
		{name: "invalid line", file: ptr("magnet:?a\nnope\n"), args: []string{"list.txt"}, wantErr: `list.txt:2: invalid entry "nope"`},
		{name: "no batch recursion", file: ptr("other.txt\n"), args: []string{"list.txt"}, wantErr: `list.txt:1: invalid entry "other.txt"`},
		{name: "unsupported scheme redacted", file: ptr("ftp://u:secret@h/x.torrent\n"), args: []string{"list.txt"}, wantErr: `list.txt:1: torrent URL "ftp://REDACTED@h/x.torrent": unsupported scheme "ftp"`},
		{name: "malformed URL not echoed", file: ptr("https:/h/x?token=secret\n"), args: []string{"list.txt"}, wantErr: "list.txt:1: invalid entry: want a magnet URI"},
		{name: "URL without host", file: ptr("magnet:?a\nhttp:///x.torrent\n"), args: []string{"list.txt"}, wantErr: "list.txt:2: torrent URL \"http:///x.torrent\": no host"},
		{name: "bad line truncated", file: ptr(strings.Repeat("x", 200)), args: []string{"list.txt"}, wantErr: `"` + strings.Repeat("x", 80) + `…"`},
		{name: "blank and whitespace lines", file: ptr("\n  \nmagnet:?a\n\t\n  magnet:?b  \n"), args: []string{"list.txt"}, want: m("magnet:?a", "magnet:?b")},
		{name: "CRLF", file: ptr("magnet:?a\r\nx.torrent\r\n"), args: []string{"list.txt"}, want: []sourceRef{{ref: "magnet:?a"}, tor("$DIR/x.torrent")}},
		{name: "no trailing newline", file: ptr("magnet:?a"), args: []string{"list.txt"}, want: m("magnet:?a")},
		{name: "BOM", file: ptr("\uFEFFmagnet:?a\n"), args: []string{"list.txt"}, want: m("magnet:?a")},
		{name: "BOM only stripped on line 1", file: ptr("magnet:?a\n\uFEFFmagnet:?b\n"), args: []string{"list.txt"}, wantErr: "list.txt:2: invalid entry"},
		{name: "mixed file and URIs", file: ptr("magnet:?a\nmagnet:?b\n"), args: []string{"magnet:?x", "list.txt", "magnet:?y"}, want: m("magnet:?x", "magnet:?a", "magnet:?b", "magnet:?y")},
		{name: "empty file", file: ptr(""), args: []string{"list.txt"}, wantErr: "no entries in"},
		{name: "blank-only file", file: ptr("\n  \r\n\t\n"), args: []string{"list.txt"}, wantErr: "no entries in"},
		{name: "line too long", file: ptr("magnet:?" + strings.Repeat("a", bufio.MaxScanTokenSize)), args: []string{"list.txt"}, wantErr: "list.txt:1:", wantIs: bufio.ErrTooLong},
		{name: "at line cap", file: ptr(lines(maxBatchEntries)), args: []string{"list.txt"}, want: m(slices.Repeat([]string{"magnet:?xt=a"}, maxBatchEntries)...)},
		{name: "over line cap", file: ptr(lines(maxBatchEntries + 1)), args: []string{"list.txt"}, wantErr: "list.txt:10001: more than 10000 entries"},
		{name: "cap counts torrent lines", file: ptr(lines(maxBatchEntries-1) + "a.torrent\nhttp://h/b.torrent\n"), args: []string{"list.txt"}, wantErr: "list.txt:10001: more than 10000 entries"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.file != nil {
				path := filepath.Join(dir, cmp.Or(tc.path, "list.txt"))
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(*tc.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			args := make([]string, len(tc.args))
			for i, a := range tc.args {
				if !isMagnet(a) {
					a = filepath.Join(dir, a)
				}
				args[i] = a
			}

			got, err := expandMagnets(args)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				want := slices.Clone(tc.want)
				for i := range want {
					want[i].ref = strings.ReplaceAll(want[i].ref, "$DIR", dir)
				}
				if !slices.Equal(got, want) {
					t.Errorf("got %+v, want %+v", got, want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("error = %v, leaks credentials", err)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("error = %v, want errors.Is %v", err, tc.wantIs)
			}
		})
	}
}

func FuzzReadBatchFile(f *testing.F) {
	for _, s := range []string{"", "\n", "magnet:?a\n", "\uFEFFmagnet:?a\r\n\n MAGNET:?b \n", "magnet:?a\nnope\n", "\x00\xff\r\r\n", "a.torrent\nhttps://h/x\n", "ftp://h/x.torrent\n"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		path := filepath.Join(dir, "list.txt")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		refs, err := readBatchFile(path)
		if err != nil {
			return
		}
		if len(refs) == 0 || len(refs) > maxBatchEntries {
			t.Fatalf("got %d entries without error", len(refs))
		}
		for _, r := range refs {
			if strings.ContainsAny(r.ref, "\r\n") || r.ref != strings.TrimSpace(r.ref) {
				t.Fatalf("bad entry %q", r.ref)
			}
			if !r.torrent {
				if !isMagnet(r.ref) {
					t.Fatalf("bad magnet %q", r.ref)
				}
				continue
			}
			if u, err := parseTorrentURL(r.ref); err != nil || u == nil && (!strings.EqualFold(filepath.Ext(r.ref), ".torrent") || !filepath.IsAbs(r.ref)) {
				t.Fatalf("bad torrent entry %q (url err %v)", r.ref, err)
			}
		}
	})
}

func ptr[T any](v T) *T { return &v }

// sampleHash is the v1 infohash of testdata/sample.torrent.
const sampleHash = "d6fb363fa1e18362e5fd510c65a9b5e75cda76e9"

func TestStringList(t *testing.T) {
	var l stringList
	for _, v := range []string{"a, b", " ,c,", "d"} {
		if err := l.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"a", "b", "c", "d"}; !slices.Equal(l, want) {
		t.Errorf("got %q, want %q", l, want)
	}
}

func TestLoadTorrentFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	big := filepath.Join(dir, "big.torrent")
	if f, err := os.Create(big); err != nil {
		t.Fatal(err)
	} else if err = errors.Join(f.Truncate(maxTorrentFileSize+1), f.Close()); err != nil {
		t.Fatal(err)
	}

	fifo := filepath.Join(dir, "fifo.torrent")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr string
		wantIs  error
	}{
		{name: "valid", path: filepath.Join("testdata", "sample.torrent")},
		{name: "missing", path: filepath.Join(dir, "nope.torrent"), wantErr: "no such file", wantIs: os.ErrNotExist},
		{name: "directory", path: dir, wantErr: "not a regular file"},
		{name: "garbage", path: write("garbage.torrent", []byte("this is not bencode")), wantErr: "not a valid .torrent file"},
		{name: "empty", path: write("empty.torrent", nil), wantErr: "not a valid .torrent file"},
		{name: "no info dict", path: write("noinfo.torrent", []byte("d8:announce3:fooe")), wantErr: "no info dictionary"},
		{name: "too large", path: big, wantErr: "larger than 8 MiB"},
		{name: "fifo", path: fifo, wantErr: "not a regular file"},
		{name: "deep nesting", path: write("deep.torrent", []byte("d4:info"+strings.Repeat("l", 100_000)+strings.Repeat("e", 100_001))), wantErr: "nested deeper than 64 levels"},
		{name: "too many containers", path: write("wide.torrent", []byte("d4:infol"+strings.Repeat("le", maxBencodeContainers)+"ee")), wantErr: "more than 200000 lists"},
		{name: "too many files", path: write("files.torrent", manyFiles(maxTorrentFiles+1)), wantErr: "more than 50000 files"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := loadTorrentFile(tc.path)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got := spec.InfoHash.HexString(); got != sampleHash {
					t.Errorf("infohash = %s, want %s", got, sampleHash)
				}
				if spec.DisplayName != "sample" || spec.InfoBytes == nil {
					t.Errorf("DisplayName = %q, InfoBytes set = %v; want \"sample\", true", spec.DisplayName, spec.InfoBytes != nil)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("error = %v, want errors.Is %v", err, tc.wantIs)
			}
		})
	}
}

func TestResolveSourcesAndDedupe(t *testing.T) {
	sample := filepath.Join("testdata", "sample.torrent")
	missing := filepath.Join(t.TempDir(), "missing.torrent")
	magnetSample := "magnet:?xt=urn:btih:" + strings.ToUpper(sampleHash)
	magnetOther := "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20)

	srcs := dedupe(resolveSources(t.Context(), nil, refs(
		[]string{magnetSample, magnetOther, "magnet:?xt=urn:btih:zz", magnetOther, "magnet:?dn=nohash"},
		[]string{sample, missing, sample},
	)))

	type row struct {
		ref     string
		errPart string
	}
	want := []row{
		{ref: sample}, // replaces the magnet in place: it carries the info dict
		{ref: magnetOther},
		{ref: "magnet:?xt=urn:btih:zz", errPart: "invalid magnet URI"},
		{ref: "magnet:?dn=nohash", errPart: "no infohash"},
		{ref: missing, errPart: `torrent file "` + missing + `"`},
	}
	if len(srcs) != len(want) {
		for _, s := range srcs {
			t.Logf("got %q err=%v", s.ref, s.err)
		}
		t.Fatalf("got %d sources, want %d", len(srcs), len(want))
	}
	for i, w := range want {
		s := srcs[i]
		if s.ref != w.ref {
			t.Errorf("[%d] ref = %q, want %q", i, s.ref, w.ref)
		}
		switch {
		case w.errPart == "" && s.err != nil:
			t.Errorf("[%d] unexpected error: %v", i, s.err)
		case w.errPart != "" && (s.err == nil || !strings.Contains(s.err.Error(), w.errPart)):
			t.Errorf("[%d] error = %v, want it to contain %q", i, s.err, w.errPart)
		case w.errPart == "" && s.spec == nil:
			t.Errorf("[%d] nil spec without error", i)
		}
	}
	if !errors.Is(srcs[4].err, os.ErrNotExist) {
		t.Errorf("missing file error = %v, want errors.Is os.ErrNotExist", srcs[4].err)
	}
}

// refs returns magnets then torrents as sourceRefs, as run orders them.
func refs(magnets, torrents []string) []sourceRef {
	var out []sourceRef
	for _, m := range magnets {
		out = append(out, sourceRef{ref: m})
	}
	for _, t := range torrents {
		out = append(out, sourceRef{ref: t, torrent: true})
	}
	return out
}

// manyFiles returns a bencoded metainfo whose v1 info lists n empty files.
func manyFiles(n int) []byte {
	var b strings.Builder
	b.WriteString("d4:infod5:filesl")
	for i := range n {
		name := strconv.Itoa(i)
		fmt.Fprintf(&b, "d6:lengthi0e4:pathl%d:%see", len(name), name)
	}
	b.WriteString("e4:name1:x12:piece lengthi16384e6:pieces0:ee")
	return []byte(b.String())
}

func TestCheckBencodeShape(t *testing.T) {
	// Strings and integers may contain l/d/e bytes without affecting depth.
	ok := "d1:l" + "3:lll" + "i-123e" + "5:ddddde" + "l" + strings.Repeat("d1:ale", 10) + "ee"
	if err := checkBencodeShape([]byte(ok)); err != nil {
		t.Errorf("valid input rejected: %v", err)
	}
	for _, s := range []string{"", "x", "5:ab", "99999999999999999999:", "i12", "eee", "l" + strings.Repeat("0:", 1000)} {
		if err := checkBencodeShape([]byte(s)); err != nil {
			t.Errorf("checkBencodeShape(%q) = %v, want nil (left to decoder)", truncate(s, 20), err)
		}
	}
}

func FuzzCheckBencodeShape(f *testing.F) {
	for _, s := range []string{"d4:infod4:name1:xee", "lli1eee", "3:abc", "l99:x"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if checkBencodeShape(data) != nil {
			return
		}
		// Anything that passes must decode without a stack overflow.
		_, _ = metainfo.Load(bytes.NewReader(data))
	})
}

func TestParseTorrentURL(t *testing.T) {
	tests := []struct {
		ref     string
		want    string // "" = local path
		wantErr string
	}{
		{ref: "x.torrent"},
		{ref: "dir/x.torrent"},
		{ref: `C:\x.torrent`},
		{ref: "1x://h/x.torrent"},  // not a scheme: starts with a digit
		{ref: "dir/a://b.torrent"}, // not a scheme: contains "/"
		{ref: "https://example.org/x.torrent", want: "https://example.org/x.torrent"},
		{ref: "HTTP://example.org/x.torrent", want: "http://example.org/x.torrent"},
		{ref: "ftp://example.org/x.torrent", wantErr: `unsupported scheme "ftp"`},
		{ref: "file:///etc/x.torrent", wantErr: `unsupported scheme "file"`},
		{ref: "https:///x.torrent", wantErr: "no host"},
		{ref: "https://u:secret@[::1/x?token=secret", wantErr: "invalid torrent URL"},
		{ref: "ftp://u:secret@h/x?token=secret", wantErr: `"ftp://REDACTED@h/x?REDACTED"`},
	}
	for _, tc := range tests {
		t.Run(tc.ref, func(t *testing.T) {
			u, err := parseTorrentURL(tc.ref)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), "secret") {
					t.Errorf("error leaks credentials: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want == "" {
				if u != nil {
					t.Errorf("got URL %v, want local path", u)
				}
				return
			}
			if u == nil || u.String() != tc.want {
				t.Errorf("got %v, want %s", u, tc.want)
			}
		})
	}
}

func TestFetchTorrent(t *testing.T) {
	sample, err := os.ReadFile(filepath.Join("testdata", "sample.torrent"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		if r.Referer() != "" {
			http.Error(w, "referer leaked", http.StatusBadRequest)
			return
		}
		_, _ = w.Write(sample)
	})
	mux.HandleFunc("/invalid", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>not bencode</html>")) })
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, maxTorrentFileSize+1)) // Content-Length is set
	})
	mux.HandleFunc("/big-chunked", func(w http.ResponseWriter, _ *http.Request) {
		chunk := make([]byte, 1<<20)
		for range maxTorrentFileSize>>20 + 1 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush() // no Content-Length
		}
	})
	mux.HandleFunc("/fail", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	mux.HandleFunc("/slow", func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	mux.HandleFunc("/to-ok", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", http.StatusFound) })
	mux.HandleFunc("/to-ftp", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://example.org/x.torrent", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		name    string
		path    string
		ctx     context.Context // nil = t.Context()
		timeout time.Duration   // 0 = fetchTimeout
		wantErr string
		wantIs  error
	}{
		{name: "success", path: "/ok"},
		{name: "redirect", path: "/to-ok"},
		{name: "404", path: "/missing", wantErr: "HTTP 404 Not Found"},
		{name: "500", path: "/fail", wantErr: "HTTP 500 Internal Server Error"},
		{name: "invalid body", path: "/invalid", wantErr: "not a valid .torrent file"},
		{name: "oversize", path: "/big", wantErr: "larger than 8 MiB"},
		{name: "oversize chunked", path: "/big-chunked", wantErr: "larger than 8 MiB"},
		{name: "timeout", path: "/slow", timeout: 50 * time.Millisecond, wantErr: "/slow"},
		{name: "cancelled", path: "/slow", ctx: cancelled, wantErr: "context canceled", wantIs: context.Canceled},
		{name: "redirect to ftp", path: "/to-ftp", wantErr: `redirect to unsupported scheme "ftp"`},
		{name: "redirect loop", path: "/loop", wantErr: "more than 5 redirects"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tc.ctx
			if ctx == nil {
				ctx = t.Context()
			}
			timeout := tc.timeout
			if timeout == 0 {
				timeout = fetchTimeout
			}
			u, err := parseTorrentURL(srv.URL + tc.path + "?token=secret")
			if err != nil || u == nil {
				t.Fatalf("parseTorrentURL = %v, %v", u, err)
			}
			src := resolveTorrentURL(ctx, newHTTPClient(timeout), u)
			if want := srv.URL + tc.path + "?REDACTED"; src.ref != want {
				t.Errorf("ref = %q, want %q", src.ref, want)
			}
			if tc.wantErr == "" {
				if src.err != nil {
					t.Fatalf("unexpected error: %v", src.err)
				}
				if got := src.spec.InfoHash.HexString(); got != sampleHash {
					t.Errorf("infohash = %s, want %s", got, sampleHash)
				}
				return
			}
			if src.err == nil || !strings.Contains(src.err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", src.err, tc.wantErr)
			}
			if msg := src.err.Error(); strings.Contains(msg, "secret") || !strings.Contains(msg, srv.URL+tc.path) {
				t.Errorf("error = %q, want it to name the URL without its query", msg)
			}
			if tc.wantIs != nil && !errors.Is(src.err, tc.wantIs) {
				t.Errorf("error = %v, want errors.Is %v", src.err, tc.wantIs)
			}
			if tc.timeout != 0 {
				if ne, ok := errors.AsType[net.Error](src.err); !ok || !ne.Timeout() {
					t.Errorf("error = %v, want a net.Error timeout", src.err)
				}
			}
		})
	}
}

func TestFetchTorrentCancelInFlight(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()
	u, err := parseTorrentURL(srv.URL + "/x.torrent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetchTorrent(ctx, newHTTPClient(fetchTimeout), u); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want errors.Is context.Canceled", err)
	}
}

func TestFetchTorrentRejectsDowngrade(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("plain-http target was requested")
	}))
	t.Cleanup(plain.Close)
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/x.torrent", http.StatusFound)
	}))
	t.Cleanup(tlsSrv.Close)

	hc := newHTTPClient(fetchTimeout)
	hc.Transport = tlsSrv.Client().Transport // trust the test certificate
	u, err := parseTorrentURL(tlsSrv.URL + "/x.torrent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetchTorrent(t.Context(), hc, u); err == nil || !strings.Contains(err.Error(), "redirect from https to http") {
		t.Errorf("error = %v, want a downgrade error", err)
	}
}

func TestResolveSourcesURLs(t *testing.T) {
	sample, err := os.ReadFile(filepath.Join("testdata", "sample.torrent"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sample) }))
	t.Cleanup(srv.Close)

	srcs := dedupe(resolveSources(t.Context(), newHTTPClient(fetchTimeout), refs(
		[]string{"magnet:?xt=urn:btih:" + sampleHash},
		[]string{"ftp://u:secret@h/x.torrent", srv.URL + "/x.torrent?k=secret", filepath.Join("testdata", "sample.torrent")},
	)))
	// The magnet is replaced by the URL source (it has the info dict); the
	// local file is a duplicate of it.
	if len(srcs) != 2 {
		for _, s := range srcs {
			t.Logf("got %q err=%v", s.ref, s.err)
		}
		t.Fatalf("got %d sources, want 2", len(srcs))
	}
	if want := srv.URL + "/x.torrent?REDACTED"; srcs[0].ref != want || srcs[0].err != nil {
		t.Errorf("[0] = %q, %v; want %q, no error", srcs[0].ref, srcs[0].err, want)
	}
	if s := srcs[1]; s.ref != "(rejected URL)" || s.err == nil || !strings.Contains(s.err.Error(), `unsupported scheme "ftp"`) || strings.Contains(s.err.Error(), "secret") {
		t.Errorf("[1] = %q, %v; want a redacted unsupported-scheme error", s.ref, s.err)
	}
}

// TestBatchFileSources runs a mixed batch file through to resolved sources:
// entry order is kept and a relative .torrent path is found next to the batch
// file, not in the working directory.
func TestBatchFileSources(t *testing.T) {
	sample, err := os.ReadFile(filepath.Join("testdata", "sample.torrent"))
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	// Run from an unrelated directory so a CWD-relative lookup would fail.
	t.Chdir(t.TempDir())
	dir := filepath.Join(t.TempDir(), "batch")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rel.torrent"), sample, 0o600); err != nil {
		t.Fatal(err)
	}
	magnet := "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20)
	list := filepath.Join(dir, "list.txt")
	gone := srv.URL + "/gone.torrent?k=secret"
	if err := os.WriteFile(list, []byte(magnet+"\nrel.torrent\n"+gone+"\n"+gone+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rs, err := expandMagnets([]string{list})
	if err != nil {
		t.Fatal(err)
	}
	srcs := dedupe(resolveSources(t.Context(), newHTTPClient(fetchTimeout), rs))
	if len(srcs) != 3 {
		t.Fatalf("got %d sources, want 3", len(srcs))
	}
	if s := srcs[0]; s.ref != magnet || s.err != nil {
		t.Errorf("[0] = %q, %v; want the magnet", s.ref, s.err)
	}
	if s := srcs[1]; s.ref != filepath.Join(dir, "rel.torrent") || s.err != nil || s.spec.InfoHash.HexString() != sampleHash {
		t.Errorf("[1] = %q, %v; want the relative .torrent loaded", s.ref, s.err)
	}
	if s := srcs[2]; s.ref != srv.URL+"/gone.torrent?REDACTED" || s.err == nil || !strings.Contains(s.err.Error(), "HTTP 404") {
		t.Errorf("[2] = %q, %v; want a redacted 404 failure", s.ref, s.err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("repeated URL fetched %d times, want 1", n)
	}
}
