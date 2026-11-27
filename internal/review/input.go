package review

import (
	"fmt"
	"strings"

	"github.com/orpheus-agents/orpheus-github-pr-review/internal/github"
)

// Input is the connector's immutable control-plane input. It is not a copy of
// all GitHub data available to the agent; the agent can read live PR context
// through gh while the code under review stays pinned by DiffRefs.
type Input struct {
	PullRequest       github.PullRequest
	Repository        github.Repository
	Notes             []github.Note
	Discussions       []github.Discussion
	Reviewer          github.User
	PRKey             string
	DiffFingerprint   string
	ReviewFingerprint string
}

// Snapshot is the complete set of review inputs observed by one successful
// GitHub poll. An empty snapshot is meaningful: it means that no pull request
// is currently assigned to the authenticated reviewer.
type Snapshot struct {
	Reviewer github.User
	Inputs   []Input
}

func NewInput(host string, reviewer github.User, source github.ReviewInput) (Input, error) {
	prKey, err := PullRequestKey(host, source.PullRequest.RepositoryID, source.PullRequest.Number)
	if err != nil {
		return Input{}, err
	}
	diffFingerprint, err := DiffFingerprint(host, source.PullRequest)
	if err != nil {
		return Input{}, err
	}
	reviewFingerprint, err := ReviewFingerprint(host, source.PullRequest, source.Notes, reviewer, source.Discussions)
	if err != nil {
		return Input{}, err
	}

	return Input{
		PullRequest:       source.PullRequest,
		Repository:        source.Repository,
		Notes:             source.Notes,
		Discussions:       source.Discussions,
		Reviewer:          reviewer,
		PRKey:             prKey,
		DiffFingerprint:   diffFingerprint,
		ReviewFingerprint: reviewFingerprint,
	}, nil
}

func PullRequestKey(host string, repositoryID, number int64) (string, error) {
	host = normalizeHost(host)
	if host == "" {
		return "", fmt.Errorf("build pull request key: GitHub host is empty")
	}
	if repositoryID <= 0 {
		return "", fmt.Errorf("build pull request key: invalid repository ID %d", repositoryID)
	}
	if number <= 0 {
		return "", fmt.Errorf("build pull request key: invalid pull request Number %d", number)
	}

	return fmt.Sprintf("%s:%d#%d", host, repositoryID, number), nil
}

func normalizeHost(host string) string {
	return strings.TrimRight(strings.TrimSpace(host), "/")
}
