package review

import (
	"testing"

	"github.com/orpheus-agents/orpheus-github-pr-review/internal/github"

	"github.com/stretchr/testify/require"
)

func TestPullRequestKey(t *testing.T) {
	t.Parallel()

	key, err := PullRequestKey(" https://github.example.com/root/ ", 42, 17)
	require.NoError(t, err)
	require.Equal(t, "https://github.example.com/root:42#17", key)
}

func TestDiffFingerprintIsCanonical(t *testing.T) {
	t.Parallel()

	pullRequest := fingerprintPullRequest()
	fingerprint, err := DiffFingerprint(" https://github.example.com/ ", pullRequest)
	require.NoError(t, err)
	require.Equal(t, "e2d220a7c3c9398470a5d505fe116e1c7d6dee63e8d749420f6ab3be669289cc", fingerprint)

	same, err := DiffFingerprint("https://github.example.com", pullRequest)
	require.NoError(t, err)
	require.Equal(t, fingerprint, same)
}

func TestReviewFingerprintTracksRequestAndHumanContext(t *testing.T) {
	t.Parallel()

	reviewer := github.User{ID: 123, Username: "orpheus"}
	human := github.User{ID: 7}
	input := fingerprintPullRequest()
	input.AssignmentID = 10
	notes := []github.Note{
		{ID: 20, Body: "ordinary note", Author: human},
		{ID: 10, Body: " please  fix\nthis ", Author: human},
	}
	fingerprint, err := ReviewFingerprint("github.com", input, notes, reviewer, nil)
	require.NoError(t, err)
	reordered := []github.Note{notes[1], {ID: 30, Body: "bot output", Author: reviewer}, notes[0]}
	same, err := ReviewFingerprint("github.com", input, reordered, reviewer, nil)
	require.NoError(t, err)
	require.Equal(t, fingerprint, same)

	input.AssignmentID++
	distinct, err := ReviewFingerprint("github.com", input, notes, reviewer, nil)
	require.NoError(t, err)
	require.NotEqual(t, fingerprint, distinct)
	input.AssignmentID--
	threads := []github.Discussion{{ID: "thread-1", Resolved: true}}
	distinct, err = ReviewFingerprint("github.com", input, notes, reviewer, threads)
	require.NoError(t, err)
	require.NotEqual(t, fingerprint, distinct)
}

func fingerprintPullRequest() github.PullRequest {
	return github.PullRequest{
		RepositoryID: 42,
		Number:       17,
		Description:  "Review this change",
		SourceBranch: "feature/x",
		TargetBranch: "main",
		DiffRefs: github.DiffRefs{
			BaseSHA: "base",
			Digest:  "start",
			HeadSHA: "head",
		},
	}
}
