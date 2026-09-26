package fs

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultReadLimit = 2000 // lines returned by read_file when limit is unset
	maxLineLen       = 2000 // bytes kept per line by read_file
	sniffLen         = 8000 // leading bytes checked for NUL to detect binary files
)

type readArgs struct {
	Path   string `json:"path" jsonschema:"File path, relative to the workspace root."`
	Offset int    `json:"offset,omitempty" jsonschema:"1-based line number to start reading from. Defaults to 1."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of lines to return. Defaults to 2000."`
}

type readResult struct {
	Content    string `json:"content"`
	TotalLines int    `json:"total_lines"`
	Note       string `json:"note,omitempty"`
}

func (w *workspace) readFile(in readArgs) (readResult, error) {
	if in.Path == "" {
		return readResult{}, errors.New("path is required")
	}
	if in.Offset < 0 || in.Limit < 0 {
		return readResult{}, errors.New("offset and limit must not be negative")
	}
	start, limit := max(in.Offset, 1), in.Limit
	if limit == 0 {
		limit = defaultReadLimit
	}
	p, err := w.resolve(in.Path)
	if err != nil {
		return readResult{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return readResult{}, w.pathErr(p, err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return readResult{}, w.pathErr(p, err)
	} else if fi.IsDir() {
		return readResult{}, fmt.Errorf("%s is a directory; use glob to list its files", w.rel(p))
	}

	br := bufio.NewReader(f)
	if head, _ := br.Peek(sniffLen); bytes.IndexByte(head, 0) >= 0 {
		return readResult{}, fmt.Errorf("%s looks like a binary file; refusing to read it", w.rel(p))
	}
	var b strings.Builder
	total, shown, cut := 0, 0, 0
	for {
		keep := total+1 >= start && shown < limit
		line, long, err := readLine(br, keep)
		if err == io.EOF {
			break
		}
		if err != nil {
			return readResult{}, w.pathErr(p, err)
		}
		total++
		if keep {
			fmt.Fprintf(&b, "%6d\t%s\n", total, line)
			shown++
			if long {
				cut++
			}
		}
	}

	res := readResult{Content: b.String(), TotalLines: total}
	switch {
	case total == 0:
		res.Note = "The file is empty."
	case start > total:
		return readResult{}, fmt.Errorf("offset %d is past the end of %s (%d lines)", start, w.rel(p), total)
	case start+shown <= total:
		res.Note = fmt.Sprintf("Showing lines %d-%d of %d. Call read_file with offset=%d to read more.",
			start, start+shown-1, total, start+shown)
	}
	if cut > 0 {
		res.Note = strings.TrimSpace(fmt.Sprintf("%s %d line(s) longer than %d characters were truncated.",
			res.Note, cut, maxLineLen))
	}
	return res, nil
}

// readLine reads one line without its line ending. When keep is set it
// returns up to maxLineLen bytes of it and reports whether the rest was cut;
// otherwise the line is only consumed. It returns io.EOF once no data is left.
func readLine(r *bufio.Reader, keep bool) (string, bool, error) {
	var line []byte
	for {
		frag, more, err := r.ReadLine()
		if err != nil {
			return "", false, err
		}
		if keep {
			line = append(line, frag...)
			if len(line) > maxLineLen {
				line = line[:maxLineLen+1] // one extra byte tells truncate the line was cut
			}
		}
		if !more {
			s, cut := truncate(string(line), maxLineLen)
			return s, cut, nil
		}
	}
}

type writeArgs struct {
	Path    string `json:"path" jsonschema:"File path, relative to the workspace root. Missing parent directories are created."`
	Content string `json:"content" jsonschema:"Complete new content of the file."`
}

type writeResult struct {
	Path         string `json:"path"`
	BytesWritten int    `json:"bytes_written"`
}

func (w *workspace) writeFile(in writeArgs) (writeResult, error) {
	if in.Path == "" {
		return writeResult{}, errors.New("path is required")
	}
	p, err := w.resolve(in.Path)
	if err != nil {
		return writeResult{}, err
	}
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return writeResult{}, fmt.Errorf("%s is a directory", w.rel(p))
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return writeResult{}, w.pathErr(p, err)
	}
	if err := os.WriteFile(p, []byte(in.Content), 0o644); err != nil {
		return writeResult{}, w.pathErr(p, err)
	}
	return writeResult{Path: w.rel(p), BytesWritten: len(in.Content)}, nil
}

type editArgs struct {
	Path       string `json:"path" jsonschema:"File path, relative to the workspace root."`
	OldString  string `json:"old_string" jsonschema:"Exact text to replace, including whitespace and indentation."`
	NewString  string `json:"new_string" jsonschema:"Text to replace it with."`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"Replace every occurrence instead of requiring exactly one."`
}

type editResult struct {
	Path         string `json:"path"`
	Replacements int    `json:"replacements"`
}

func (w *workspace) editFile(in editArgs) (editResult, error) {
	switch {
	case in.Path == "":
		return editResult{}, errors.New("path is required")
	case in.OldString == "":
		return editResult{}, errors.New("old_string must not be empty; use write_file to create a file")
	case in.OldString == in.NewString:
		return editResult{}, errors.New("old_string and new_string are identical; nothing to change")
	}
	p, err := w.resolve(in.Path)
	if err != nil {
		return editResult{}, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return editResult{}, w.pathErr(p, err)
	}
	if fi.IsDir() {
		return editResult{}, fmt.Errorf("%s is a directory", w.rel(p))
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return editResult{}, w.pathErr(p, err)
	}
	if bytes.IndexByte(data[:min(len(data), sniffLen)], 0) >= 0 {
		return editResult{}, fmt.Errorf("%s looks like a binary file; refusing to edit it", w.rel(p))
	}

	content := string(data)
	n := strings.Count(content, in.OldString)
	switch {
	case n == 0:
		return editResult{}, fmt.Errorf("old_string not found in %s; read the file and copy the text exactly", w.rel(p))
	case n > 1 && !in.ReplaceAll:
		return editResult{}, fmt.Errorf("old_string occurs %d times in %s; include more surrounding text to make it unique, or set replace_all", n, w.rel(p))
	}
	if in.ReplaceAll {
		content = strings.ReplaceAll(content, in.OldString, in.NewString)
	} else {
		content = strings.Replace(content, in.OldString, in.NewString, 1)
	}
	if err := os.WriteFile(p, []byte(content), fi.Mode().Perm()); err != nil {
		return editResult{}, w.pathErr(p, err)
	}
	return editResult{Path: w.rel(p), Replacements: n}, nil
}
