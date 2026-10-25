package integrations

import (
	"slices"
	"testing"
)

// TL-651: a merge finishes the tickets the PR names in its branch, title or a commit's subject, or
// after a closing word; a passing mention elsewhere only links it.
func TestClosers(t *testing.T) {
	pr := PullRequest{Branch: "tl-7-login", Title: "Fix login (TL-8)", body: "The Jira importer (TL-538) builds on this; TL-540 comes later.\n\nCloses TL-9, TL-10 and TL-11. Fixes: tl-12; resolved TL-13"}
	commits := []string{"feat: login (TL-14)\n\nPart of TL-472, see TL-15.\nFixes TL-16", "Wip"}
	got := keysIn("TL", closers(pr, commits)...)
	want := []int{7, 8, 14, 9, 10, 11, 12, 13, 16}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("closing %v, want %v", got, want)
	}
}
