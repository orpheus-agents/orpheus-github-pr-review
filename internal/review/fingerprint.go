package review

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"github.com/orpheus-agents/orpheus-github-pr-review/internal/github"
)

const fingerprintVersion = 1

type diffFingerprintPayload struct {
	Version      int      `json:"version"`
	GitHubHost   string   `json:"github_host"`
	RepositoryID int64    `json:"repository_id"`
	Number       int64    `json:"number"`
	SourceBranch string   `json:"source_branch"`
	TargetBranch string   `json:"target_branch"`
	DiffRefs     diffRefs `json:"diff_refs"`
}

type diffRefs struct {
	BaseSHA string `json:"base_sha"`
	Digest  string `json:"diff_digest"`
	HeadSHA string `json:"head_sha"`
}

type reviewFingerprintPayload struct {
	Version      int                    `json:"version"`
	Diff         diffFingerprintPayload `json:"diff"`
	Description  string                 `json:"description"`
	AssignmentID int64                  `json:"assignment_id"`
	Discussions  []github.Discussion    `json:"discussions"`
	Notes        []fingerprintNote      `json:"notes"`
}

type fingerprintNote struct {
	ID       int64  `json:"id"`
	AuthorID int64  `json:"author_id"`
	Body     string `json:"body"`
	Resolved bool   `json:"resolved,omitempty"`
}

func DiffFingerprint(host string, pullRequest github.PullRequest) (string, error) {
	return hashJSON(newDiffFingerprintPayload(host, pullRequest))
}

func ReviewFingerprint(
	host string,
	pullRequest github.PullRequest,
	notes []github.Note,
	reviewer github.User,
	discussions []github.Discussion,
) (string, error) {
	payload := reviewFingerprintPayload{
		Version:      fingerprintVersion,
		Diff:         newDiffFingerprintPayload(host, pullRequest),
		Description:  pullRequest.Description,
		AssignmentID: pullRequest.AssignmentID,
		Discussions:  discussions,
		Notes:        reviewFingerprintNotes(notes, reviewer),
	}

	return hashJSON(payload)
}

func reviewFingerprintNotes(notes []github.Note, reviewer github.User) []fingerprintNote {
	selected := make([]fingerprintNote, 0, len(notes))
	for _, note := range notes {
		if note.Author.ID == reviewer.ID {
			continue
		}

		body := normalizeBody(note.Body)
		if body == "" {
			continue
		}
		selected = append(selected, fingerprintNote{
			ID:       note.ID,
			AuthorID: note.Author.ID,
			Body:     body,
			Resolved: note.Resolved,
		})
	}

	slices.SortFunc(selected, func(left, right fingerprintNote) int {
		return cmp.Compare(left.ID, right.ID)
	})

	return selected
}

func newDiffFingerprintPayload(host string, pullRequest github.PullRequest) diffFingerprintPayload {
	return diffFingerprintPayload{
		Version:      fingerprintVersion,
		GitHubHost:   normalizeHost(host),
		RepositoryID: pullRequest.RepositoryID,
		Number:       pullRequest.Number,
		SourceBranch: pullRequest.SourceBranch,
		TargetBranch: pullRequest.TargetBranch,
		DiffRefs: diffRefs{
			BaseSHA: pullRequest.DiffRefs.BaseSHA,
			Digest:  pullRequest.DiffRefs.Digest,
			HeadSHA: pullRequest.DiffRefs.HeadSHA,
		},
	}
}

func normalizeBody(body string) string {
	return strings.Join(strings.Fields(body), " ")
}

func hashJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)

	return hex.EncodeToString(sum[:]), nil
}
