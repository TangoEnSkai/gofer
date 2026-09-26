package gh

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	repoRE        = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	loginRE       = regexp.MustCompile(`^(@me|(app/)?[A-Za-z0-9][A-Za-z0-9-]{0,38})$`)
	htmlCommentRE = regexp.MustCompile(`(?s)<!--.*?-->`)
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// checkRepo accepts "owner/name" only. A leading "-" is rejected so the value
// can never be parsed as a gh flag.
func checkRepo(repo string) error {
	if strings.HasPrefix(repo, "-") || !repoRE.MatchString(repo) {
		return invalid("repo %q must look like owner/name", repo)
	}
	return nil
}

func checkItem(repo string, number int) error {
	if err := checkRepo(repo); err != nil {
		return err
	}
	if number <= 0 {
		return invalid("number must be positive, got %d", number)
	}
	return nil
}

// checkState returns state, defaulting to "open", if it is one of allowed.
func checkState(state string, allowed ...string) (string, error) {
	if state == "" {
		return "open", nil
	}
	if !slices.Contains(allowed, state) {
		return "", invalid("state %q must be one of %s", state, strings.Join(allowed, ", "))
	}
	return state, nil
}

func checkAuthor(author string) error {
	if !loginRE.MatchString(author) {
		return invalid("author %q must be a GitHub login or @me", author)
	}
	return nil
}

// checkAPIPath accepts a relative REST path such as
// "repos/OWNER/REPO/pulls/1/comments?per_page=20". It rejects anything that
// could be a flag, a full URL (gh api would send the token there), a path
// traversal, or GraphQL (whose POST requests can mutate).
func checkAPIPath(p string) error {
	if p == "" || len(p) > 1024 {
		return invalid("path must be 1-1024 characters")
	}
	if first, _ := utf8.DecodeRuneInString(p); first != '/' && !isASCIILetter(first) {
		return invalid("path %q must start with a letter or /", p)
	}
	if strings.ContainsFunc(p, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r == '\\' }) {
		return invalid("path %q must not contain whitespace, control characters or backslashes", p)
	}
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.User != nil {
		return invalid("path %q must be relative to the API root, not a URL", p)
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimLeft(u.Path, "/")), "graphql") {
		return invalid("GraphQL is not allowed; use a REST path")
	}
	if slices.Contains(strings.Split(u.Path, "/"), "..") {
		return invalid("path %q must not contain ..", p)
	}
	return nil
}

func isASCIILetter(r rune) bool {
	return ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z')
}

// cleanText drops HTML comments (PR and issue templates are full of them),
// normalises line endings, and truncates the result to n bytes.
func cleanText(s string, n int) string {
	s = htmlCommentRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return truncate(strings.TrimSpace(s), n)
}

// truncate shortens s to at most n bytes (on a rune boundary) and marks the cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "… [truncated]"
}
