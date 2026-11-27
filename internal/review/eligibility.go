package review

import (
	"strings"

	"github.com/orpheus-agents/orpheus-github-pr-review/internal/github"
)

type IneligibilityReason string

const (
	ReasonPRNotOpen               IneligibilityReason = "pr_not_open"
	ReasonReviewerIdentityInvalid IneligibilityReason = "reviewer_identity_invalid"
	ReasonAssignmentMissing       IneligibilityReason = "missing_review_request_event"
	ReasonReviewerNotAssigned     IneligibilityReason = "reviewer_not_assigned"
	ReasonRepositoryMismatch      IneligibilityReason = "repository_mismatch"
	ReasonDiffRefsIncomplete      IneligibilityReason = "diff_refs_incomplete"
)

type Eligibility struct {
	Eligible bool
	Reasons  []IneligibilityReason
}

// EvaluateInputEligibility evaluates only gates that can be decided from the
// GitHub review input. Session history, unfinished lifecycle work, and concurrency
// capacity are admission gates evaluated later by the connector.
func EvaluateInputEligibility(input Input) Eligibility {
	var reasons []IneligibilityReason
	mr := input.PullRequest

	if mr.State != "open" {
		reasons = append(reasons, ReasonPRNotOpen)
	}
	if input.Reviewer.ID <= 0 {
		reasons = append(reasons, ReasonReviewerIdentityInvalid)
	} else if !hasReviewer(mr.Reviewers, input.Reviewer.ID) {
		reasons = append(reasons, ReasonReviewerNotAssigned)
	} else if mr.AssignmentID <= 0 {
		reasons = append(reasons, ReasonAssignmentMissing)
	}
	if input.Repository.ID != mr.RepositoryID {
		reasons = append(reasons, ReasonRepositoryMismatch)
	}
	if strings.TrimSpace(mr.DiffRefs.BaseSHA) == "" ||
		strings.TrimSpace(mr.DiffRefs.Digest) == "" ||
		strings.TrimSpace(mr.DiffRefs.HeadSHA) == "" {
		reasons = append(reasons, ReasonDiffRefsIncomplete)
	}

	return Eligibility{Eligible: len(reasons) == 0, Reasons: reasons}
}

func hasReviewer(reviewers []github.User, reviewerID int64) bool {
	for _, reviewer := range reviewers {
		if reviewer.ID == reviewerID {
			return true
		}
	}

	return false
}
