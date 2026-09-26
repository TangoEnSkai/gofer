package github

import (
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/TangoEnSkai/gofer/internal/tools/gh"
)

// Text limits for what the judge sees, in runes.
const (
	maxBody     = 300
	maxActivity = 500
	// latestActivity is how many recent comments and reviews are kept.
	latestActivity = 3
)

// staleLabels are label names, compared case-insensitively, that stale bots
// put on PRs they are about to close.
var staleLabels = []string{"stale", "lifecycle/stale", "no-activity"}

// prInfo is one pull request as the judge sees it: deterministic signals
// computed here, then text written by other people, which the prompt marks
// as untrusted.
type prInfo struct {
	URL    string `json:"url"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`

	// CI is failing, pending, passing, none (no checks), or unknown (the
	// checks could not be fetched).
	CI string `json:"ci"`
	// Review is changes_requested, approved, review_required, none, or
	// unknown.
	Review string `json:"review"`
	// Mergeable is mergeable, conflicting, or unknown.
	Mergeable string `json:"mergeable"`
	// MergeState is GitHub's mergeStateStatus, lower-cased (e.g. clean,
	// blocked, behind, dirty).
	MergeState        string `json:"merge_state,omitempty"`
	Draft             bool   `json:"draft"`
	DaysSinceActivity int    `json:"days_since_activity"`
	// Inactive is days_since_activity >= stale_after_days.
	Inactive        bool   `json:"inactive"`
	StaleLabel      bool   `json:"stale_label"`
	LastCommentByMe bool   `json:"last_comment_by_me"`
	Error           string `json:"error,omitempty"`

	// Untrusted text.
	Title  string     `json:"title"`
	Labels []string   `json:"labels,omitempty"`
	Body   string     `json:"body,omitempty"`
	Latest []activity `json:"latest,omitempty"`
}

// activity is a comment or review, newest last.
type activity struct {
	Kind  string `json:"kind"` // comment or review
	By    string `json:"by"`
	ByMe  bool   `json:"by_me,omitempty"`
	Role  string `json:"role,omitempty"`  // author association, e.g. member
	State string `json:"state,omitempty"` // review state, e.g. changes_requested
	On    string `json:"on"`              // date
	Text  string `json:"text,omitempty"`

	at time.Time
}

// newPRInfo derives the signals of f as of now. viewer is the gh user's
// login; when it is unknown the PR's author stands in, which is the same
// user for a search by --author=@me.
func newPRInfo(f fetched, viewer string, now time.Time, staleAfterDays int) prInfo {
	pr := f.PR
	v := f.View
	if v != nil {
		pr = v.PR
		if viewer == "" {
			viewer = v.Author
		}
	}
	days := max(0, int(now.Sub(pr.UpdatedAt).Hours()/24))
	info := prInfo{
		URL:               f.PR.URL,
		Repo:              f.PR.Repo,
		Number:            f.PR.Number,
		CI:                ciSignal(f.Checks),
		Review:            reviewSignal(v),
		Mergeable:         "unknown",
		Draft:             pr.IsDraft,
		DaysSinceActivity: days,
		Inactive:          days >= staleAfterDays,
		Error:             f.Error,
		Title:             oneLine(pr.Title),
	}
	if v == nil {
		return info
	}
	switch v.Mergeable {
	case "MERGEABLE":
		info.Mergeable = "mergeable"
	case "CONFLICTING":
		info.Mergeable = "conflicting"
	}
	info.MergeState = strings.ToLower(v.MergeStateStatus)
	info.Labels = v.Labels
	info.StaleLabel = slices.ContainsFunc(v.Labels, func(l string) bool {
		return slices.ContainsFunc(staleLabels, func(s string) bool { return strings.EqualFold(l, s) })
	})
	info.Body = trim(v.Body, maxBody)
	info.Latest = latest(v, viewer)
	if n := len(info.Latest); n > 0 {
		info.LastCommentByMe = info.Latest[n-1].ByMe
	}
	return info
}

// ciSignal summarises check buckets. A cancelled check on the head commit
// blocks a merge like a failing one.
func ciSignal(c *gh.CheckReport) string {
	switch {
	case c == nil:
		return "unknown"
	case c.Buckets["fail"]+c.Buckets["cancel"] > 0:
		return "failing"
	case c.Buckets["pending"] > 0:
		return "pending"
	case c.Buckets["pass"] > 0:
		return "passing"
	}
	return "none"
}

// reviewSignal maps reviewDecision. Repos without required reviews report no
// decision, so then the latest review of each reviewer decides.
func reviewSignal(v *gh.PRDetail) string {
	if v == nil {
		return "unknown"
	}
	switch v.ReviewDecision {
	case "CHANGES_REQUESTED":
		return "changes_requested"
	case "APPROVED":
		return "approved"
	case "REVIEW_REQUIRED":
		return "review_required"
	}
	review := "none"
	for _, r := range v.Reviews {
		switch r.State {
		case "CHANGES_REQUESTED":
			return "changes_requested"
		case "APPROVED":
			review = "approved"
		}
	}
	return review
}

// latest returns the newest latestActivity comments and reviews, oldest
// first.
func latest(v *gh.PRDetail, viewer string) []activity {
	var all []activity
	add := func(kind, by, role, state, text string, at time.Time) {
		all = append(all, activity{
			Kind:  kind,
			By:    by,
			ByMe:  viewer != "" && strings.EqualFold(by, viewer),
			Role:  strings.ToLower(role),
			State: strings.ToLower(state),
			On:    at.Format(time.DateOnly),
			Text:  trim(text, maxActivity),
			at:    at,
		})
	}
	for _, c := range v.Comments {
		add("comment", c.Author, c.Association, "", c.Body, c.CreatedAt)
	}
	for _, r := range v.Reviews {
		add("review", r.Author, r.Association, r.State, r.Body, r.SubmittedAt)
	}
	slices.SortStableFunc(all, func(a, b activity) int { return a.at.Compare(b.at) })
	return all[max(0, len(all)-latestActivity):]
}

// trim shortens s to at most n runes, marking the cut with "…".
func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// oneLine collapses runs of whitespace, including newlines, to one space.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
