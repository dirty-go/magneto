package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

func TestExpandMagnets(t *testing.T) {
	lines := func(n int) string { return strings.Repeat("magnet:?xt=a\n", n) }
	tests := []struct {
		name    string
		file    *string // contents of list.txt; nil = not created
		args    []string
		want    []string
		wantErr string
		wantIs  error
	}{
		{name: "raw URIs only", args: []string{"magnet:?a", "MAGNET:?b"}, want: []string{"magnet:?a", "MAGNET:?b"}},
		{name: "missing file", args: []string{"list.txt"}, wantErr: "neither a magnet URI nor a readable file", wantIs: os.ErrNotExist},
		{name: "directory", args: []string{"."}, wantErr: "is not a regular file"},
		{name: "non-magnet line", file: ptr("magnet:?a\nhttp://x\n"), args: []string{"list.txt"}, wantErr: `list.txt:2: invalid magnet URI "http://x"`},
		{name: "bad line truncated", file: ptr(strings.Repeat("x", 200)), args: []string{"list.txt"}, wantErr: `"` + strings.Repeat("x", 80) + `…"`},
		{name: "blank and whitespace lines", file: ptr("\n  \nmagnet:?a\n\t\n  magnet:?b  \n"), args: []string{"list.txt"}, want: []string{"magnet:?a", "magnet:?b"}},
		{name: "CRLF", file: ptr("magnet:?a\r\nmagnet:?b\r\n"), args: []string{"list.txt"}, want: []string{"magnet:?a", "magnet:?b"}},
		{name: "no trailing newline", file: ptr("magnet:?a"), args: []string{"list.txt"}, want: []string{"magnet:?a"}},
		{name: "BOM", file: ptr("\uFEFFmagnet:?a\n"), args: []string{"list.txt"}, want: []string{"magnet:?a"}},
		{name: "BOM only stripped on line 1", file: ptr("magnet:?a\n\uFEFFmagnet:?b\n"), args: []string{"list.txt"}, wantErr: "list.txt:2: invalid magnet URI"},
		{name: "mixed file and URIs", file: ptr("magnet:?a\nmagnet:?b\n"), args: []string{"magnet:?x", "list.txt", "magnet:?y"}, want: []string{"magnet:?x", "magnet:?a", "magnet:?b", "magnet:?y"}},
		{name: "empty file", file: ptr(""), args: []string{"list.txt"}, wantErr: "no magnet URIs in"},
		{name: "blank-only file", file: ptr("\n  \r\n\t\n"), args: []string{"list.txt"}, wantErr: "no magnet URIs in"},
		{name: "line too long", file: ptr("magnet:?" + strings.Repeat("a", bufio.MaxScanTokenSize)), args: []string{"list.txt"}, wantErr: "list.txt:1:", wantIs: bufio.ErrTooLong},
		{name: "at line cap", file: ptr(lines(maxBatchEntries)), args: []string{"list.txt"}, want: slices.Repeat([]string{"magnet:?xt=a"}, maxBatchEntries)},
		{name: "over line cap", file: ptr(lines(maxBatchEntries + 1)), args: []string{"list.txt"}, wantErr: "list.txt:10001: more than 10000 magnet URIs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.file != nil {
				if err := os.WriteFile(filepath.Join(dir, "list.txt"), []byte(*tc.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			args := make([]string, len(tc.args))
			for i, a := range tc.args {
				if !strings.HasPrefix(strings.ToLower(a), "magnet:") {
					a = filepath.Join(dir, a)
				}
				args[i] = a
			}

			got, err := expandMagnets(args)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !slices.Equal(got, tc.want) {
					t.Errorf("got %q, want %q", got, tc.want)
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

func FuzzReadBatchFile(f *testing.F) {
	for _, s := range []string{"", "\n", "magnet:?a\n", "\uFEFFmagnet:?a\r\n\n MAGNET:?b \n", "magnet:?a\nnope\n", "\x00\xff\r\r\n"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "list.txt")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		uris, err := readBatchFile(path)
		if err != nil {
			return
		}
		if len(uris) == 0 || len(uris) > maxBatchEntries {
			t.Fatalf("got %d URIs without error", len(uris))
		}
		for _, u := range uris {
			if !strings.HasPrefix(strings.ToLower(u), "magnet:") || u != strings.TrimSpace(u) || strings.Contains(u, "\n") {
				t.Fatalf("bad URI %q", u)
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

	srcs := dedupe(resolveSources(
		[]string{magnetSample, magnetOther, "magnet:?xt=urn:btih:zz", magnetOther, "magnet:?dn=nohash"},
		[]string{sample, missing, sample},
	))

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
