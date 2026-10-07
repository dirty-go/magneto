package main

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
