package review

import (
	"testing"

	"github.com/orpheus-agents/orpheus-github-pr-review/internal/github"

	"github.com/stretchr/testify/require"
)

func TestEvaluateInputEligibility(t *testing.T) {
	t.Parallel()

	base := eligibleInput()
	tests := map[string]struct {
		mutate func(*Input)
		want   []IneligibilityReason
	}{
		"eligible": {},
		"closed": {
			mutate: func(input *Input) { input.PullRequest.State = "closed" },
			want:   []IneligibilityReason{ReasonPRNotOpen},
		},
		"draft assigned to reviewer": {
			mutate: func(input *Input) { input.PullRequest.Draft = true },
		},
		"reviewer missing": {
			mutate: func(input *Input) { input.PullRequest.Reviewers = nil },
			want:   []IneligibilityReason{ReasonReviewerNotAssigned},
		},
		"reviewer identity invalid": {
			mutate: func(input *Input) { input.Reviewer.ID = 0 },
			want:   []IneligibilityReason{ReasonReviewerIdentityInvalid},
		},
		"repository mismatch": {
			mutate: func(input *Input) { input.Repository.ID = 99 },
			want:   []IneligibilityReason{ReasonRepositoryMismatch},
		},
		"diff refs incomplete": {
			mutate: func(input *Input) { input.PullRequest.DiffRefs.HeadSHA = "" },
			want:   []IneligibilityReason{ReasonDiffRefsIncomplete},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			input := base
			if test.mutate != nil {
				test.mutate(&input)
			}

			decision := EvaluateInputEligibility(input)

			require.Equal(t, len(test.want) == 0, decision.Eligible)
			require.Equal(t, test.want, decision.Reasons)
		})
	}
}

func eligibleInput() Input {
	reviewer := github.User{ID: 123, Username: "orpheus"}
	return Input{
		Reviewer: reviewer,
		Repository: github.Repository{
			ID:       42,
			FullName: "team/repository",
		},
		PullRequest: github.PullRequest{
			AssignmentID: 100,
			RepositoryID: 42,
			Number:       17,
			State:        "open",
			Reviewers:    []github.User{reviewer},
			DiffRefs: github.DiffRefs{
				BaseSHA: "base",
				Digest:  "start",
				HeadSHA: "head",
			},
		},
	}
}
