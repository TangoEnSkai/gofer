package fs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFile(t *testing.T) {
	var ten strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&ten, "line %d\n", i)
	}
	w := newTestWorkspace(t, map[string]string{
		"abc.txt":   "a\nb\nc\n",
		"no-eol":    "a\nb",
		"crlf.txt":  "a\r\nb\r\n",
		"ten.txt":   ten.String(),
		"empty.txt": "",
		"bin.dat":   "abc\x00def",
		"dir/x.txt": "x",
	})

	for _, tc := range []struct {
		name      string
		in        readArgs
		content   string
		total     int
		noteParts []string
	}{
		{"whole file", readArgs{Path: "abc.txt"}, "     1\ta\n     2\tb\n     3\tc\n", 3, nil},
		{"no trailing newline", readArgs{Path: "no-eol"}, "     1\ta\n     2\tb\n", 2, nil},
		{"crlf", readArgs{Path: "crlf.txt"}, "     1\ta\n     2\tb\n", 2, nil},
		{"offset and limit", readArgs{Path: "ten.txt", Offset: 3, Limit: 2}, "     3\tline 3\n     4\tline 4\n", 10,
			[]string{"lines 3-4 of 10", "offset=5"}},
		{"offset to end", readArgs{Path: "ten.txt", Offset: 9}, "     9\tline 9\n    10\tline 10\n", 10, nil},
		{"empty", readArgs{Path: "empty.txt"}, "", 0, []string{"empty"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := w.readFile(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if res.Content != tc.content {
				t.Errorf("content = %q, want %q", res.Content, tc.content)
			}
			if res.TotalLines != tc.total {
				t.Errorf("total_lines = %d, want %d", res.TotalLines, tc.total)
			}
			if len(tc.noteParts) == 0 && res.Note != "" {
				t.Errorf("unexpected note %q", res.Note)
			}
			for _, part := range tc.noteParts {
				if !strings.Contains(res.Note, part) {
					t.Errorf("note = %q, want it to contain %q", res.Note, part)
				}
			}
		})
	}

	for _, tc := range []struct {
		name string
		in   readArgs
		err  string
	}{
		{"missing path", readArgs{}, "path is required"},
		{"not found", readArgs{Path: "nope.txt"}, "nope.txt: no such file or directory"},
		{"directory", readArgs{Path: "dir"}, "is a directory"},
		{"binary", readArgs{Path: "bin.dat"}, "binary"},
		{"offset past end", readArgs{Path: "abc.txt", Offset: 4}, "past the end"},
		{"negative offset", readArgs{Path: "abc.txt", Offset: -1}, "negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := w.readFile(tc.in)
			wantErr(t, err, tc.err)
		})
	}
}

func TestReadFileLimits(t *testing.T) {
	var many strings.Builder
	for i := 1; i <= defaultReadLimit+500; i++ {
		fmt.Fprintf(&many, "%d\n", i)
	}
	long := strings.Repeat("x", 3*maxLineLen)
	w := newTestWorkspace(t, map[string]string{
		"many.txt": many.String(),
		"long.txt": "short\n" + long + "\n",
	})

	res, err := w.readFile(readArgs{Path: "many.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(res.Content, "\n"); got != defaultReadLimit {
		t.Errorf("returned %d lines, want the default limit %d", got, defaultReadLimit)
	}
	if res.TotalLines != defaultReadLimit+500 {
		t.Errorf("total_lines = %d", res.TotalLines)
	}
	if !strings.Contains(res.Note, fmt.Sprintf("offset=%d", defaultReadLimit+1)) {
		t.Errorf("note = %q", res.Note)
	}

	res, err = w.readFile(readArgs{Path: "long.txt"})
	if err != nil {
		t.Fatal(err)
	}
	want := "     1\tshort\n     2\t" + long[:maxLineLen] + "\n"
	if res.Content != want {
		t.Errorf("long line not cut to %d bytes (content length %d)", maxLineLen, len(res.Content))
	}
	if res.TotalLines != 2 || !strings.Contains(res.Note, "truncated") {
		t.Errorf("total_lines = %d, note = %q", res.TotalLines, res.Note)
	}
}

func TestWriteFile(t *testing.T) {
	w := newTestWorkspace(t, map[string]string{"old.txt": "old", "dir/keep": ""})

	res, err := w.writeFile(writeArgs{Path: "a/b/new.txt", Content: "hello\n"})
	if err != nil {
		t.Fatal(err)
	}
	if res != (writeResult{Path: "a/b/new.txt", BytesWritten: 6}) {
		t.Errorf("result = %+v", res)
	}
	if got := readTestFile(t, filepath.Join(w.root, "a/b/new.txt")); got != "hello\n" {
		t.Errorf("content = %q", got)
	}

	if _, err := w.writeFile(writeArgs{Path: "old.txt", Content: "new"}); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, filepath.Join(w.root, "old.txt")); got != "new" {
		t.Errorf("overwritten content = %q", got)
	}

	if _, err := w.writeFile(writeArgs{Path: "empty.txt"}); err != nil {
		t.Fatalf("writing an empty file: %v", err)
	}

	_, err = w.writeFile(writeArgs{Path: "dir", Content: "x"})
	wantErr(t, err, "is a directory")
	_, err = w.writeFile(writeArgs{Content: "x"})
	wantErr(t, err, "path is required")
	_, err = w.writeFile(writeArgs{Path: "old.txt/child", Content: "x"})
	wantErr(t, err, "old.txt")
}

func TestEditFile(t *testing.T) {
	const src = "alpha beta\ngamma beta\ndelta\n"
	for _, tc := range []struct {
		name  string
		in    editArgs
		want  string // file content afterwards
		repls int
		err   string
	}{
		{name: "unique", in: editArgs{OldString: "delta", NewString: "DELTA"},
			want: "alpha beta\ngamma beta\nDELTA\n", repls: 1},
		{name: "multiline", in: editArgs{OldString: "beta\ngamma", NewString: "beta\n\ngamma"},
			want: "alpha beta\n\ngamma beta\ndelta\n", repls: 1},
		{name: "delete", in: editArgs{OldString: "delta\n"},
			want: "alpha beta\ngamma beta\n", repls: 1},
		{name: "replace all", in: editArgs{OldString: "beta", NewString: "BETA", ReplaceAll: true},
			want: "alpha BETA\ngamma BETA\ndelta\n", repls: 2},
		{name: "ambiguous", in: editArgs{OldString: "beta", NewString: "BETA"},
			want: src, err: "occurs 2 times"},
		{name: "not found", in: editArgs{OldString: "epsilon", NewString: "x"},
			want: src, err: "not found"},
		{name: "whitespace must match", in: editArgs{OldString: "alpha  beta", NewString: "x"},
			want: src, err: "not found"},
		{name: "identical", in: editArgs{OldString: "delta", NewString: "delta"},
			want: src, err: "identical"},
		{name: "empty old_string", in: editArgs{NewString: "x"},
			want: src, err: "must not be empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkspace(t, map[string]string{"f.txt": src})
			tc.in.Path = "f.txt"
			res, err := w.editFile(tc.in)
			if tc.err != "" {
				wantErr(t, err, tc.err)
			} else if err != nil {
				t.Fatal(err)
			} else if res != (editResult{Path: "f.txt", Replacements: tc.repls}) {
				t.Errorf("result = %+v", res)
			}
			if got := readTestFile(t, filepath.Join(w.root, "f.txt")); got != tc.want {
				t.Errorf("file = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEditFileKeepsModeAndRefusesOddFiles(t *testing.T) {
	w := newTestWorkspace(t, map[string]string{"script.sh": "echo hi\n", "bin.dat": "a\x00b", "dir/x": ""})
	p := filepath.Join(w.root, "script.sh")
	if err := os.Chmod(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := w.editFile(editArgs{Path: "script.sh", OldString: "hi", NewString: "there"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("mode after edit = %v, %v; want 0700", fi.Mode().Perm(), err)
	}

	_, err := w.editFile(editArgs{Path: "bin.dat", OldString: "a", NewString: "b"})
	wantErr(t, err, "binary")
	_, err = w.editFile(editArgs{Path: "dir", OldString: "a", NewString: "b"})
	wantErr(t, err, "is a directory")
	_, err = w.editFile(editArgs{Path: "missing.txt", OldString: "a", NewString: "b"})
	wantErr(t, err, "missing.txt: no such file or directory")
}
