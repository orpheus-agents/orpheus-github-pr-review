package publication

import (
	"context"
	"strings"
	"testing"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"github.com/orpheus-agents/orpheus-github-pr-review/internal/github"
	"github.com/orpheus-agents/orpheus-github-pr-review/internal/protocol"
	"github.com/orpheus-agents/orpheus-github-pr-review/internal/review"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type fakeGitHub struct {
	Client
	current                   github.PullRequest
	viewer                    github.User
	review                    github.Review
	draft                     github.ReviewDraft
	creates, submits, deletes int
	loseCreate, loseSubmit    bool
}

func (c *fakeGitHub) GetPullRequest(context.Context, int64, int64) (github.PullRequest, error) {
	return c.current, nil
}

func (c *fakeGitHub) ListReviews(context.Context, int64, int64) ([]github.Review, error) {
	if c.review.ID == 0 {
		return nil, nil
	}
	return []github.Review{c.review}, nil
}

func (c *fakeGitHub) CreateReview(_ context.Context, _, _ int64, draft github.ReviewDraft) (github.Review, error) {
	c.creates++
	c.draft = draft
	c.review = github.Review{ID: 11, NodeID: "review", User: c.viewer, State: "PENDING", Body: draft.Body, CommitID: draft.CommitID}
	if c.loseCreate {
		return github.Review{}, context.DeadlineExceeded
	}
	return c.review, nil
}

func (c *fakeGitHub) SubmitReview(context.Context, int64, int64, int64, string) error {
	c.submits++
	c.review.State = "COMMENTED"
	c.current.AssignmentID = 0
	c.current.Reviewers = nil
	if c.loseSubmit {
		return context.DeadlineExceeded
	}
	return nil
}

func (c *fakeGitHub) DeletePendingReview(context.Context, int64, int64, int64) error {
	c.deletes++
	c.review = github.Review{}
	return nil
}

func publicationFixture(t *testing.T) (review.Input, *fakeGitHub) {
	t.Helper()
	viewer := github.User{ID: 17, Username: "reviewer"}
	repo := github.Repository{ID: 12, FullName: "org/repo"}
	pr := github.PullRequest{Number: 23, RepositoryID: 12, AssignmentID: 800,
		State: "open", Reviewers: []github.User{viewer}, DiffRefs: github.DiffRefs{BaseSHA: "base", Digest: "diff", HeadSHA: "head"}}
	input, err := review.NewInput(github.Host, viewer, github.ReviewInput{PullRequest: pr, Repository: repo})
	require.NoError(t, err)
	return input, &fakeGitHub{current: pr, viewer: viewer}
}

func TestPendingAndSubmitRecoveryUsesOneReview(t *testing.T) {
	t.Parallel()
	input, client := publicationFixture(t)
	client.loseCreate, client.loseSubmit = true, true
	publisher := New(zap.NewNop(), client, github.Host, "https://orpheus.example")
	err := publisher.PublishError(t.Context(), input, Failure{
		Message: "Public error <detail> @somebody", Code: "run_failed",
		Notes: review.NoteTemplates{Fail: "Review failed.\n\n> {{ .Reason }}"},
	})
	require.NoError(t, err)
	require.Equal(t, 1, client.creates)
	require.Equal(t, 1, client.submits)
	require.Equal(t, "head", client.draft.CommitID)
	require.Contains(t, client.draft.Body, "> Public error &lt;detail&gt; &#64;somebody")
	require.True(t, hasTrailingMarker(client.draft.Body, lifecycleMarker(input, "failed")))

	done, err := publisher.Finalize(t.Context(), input)
	require.NoError(t, err)
	require.True(t, done)
}

func TestStaleRunDoesNotCloseNewRequest(t *testing.T) {
	t.Parallel()
	input, client := publicationFixture(t)
	client.current.AssignmentID++
	client.current.DiffRefs.HeadSHA = "new-head"
	publisher := New(zap.NewNop(), client, github.Host, "")
	require.NoError(t, publisher.Discard(t.Context(), input))
	require.ErrorIs(t, publisher.PublishSkipped(t.Context(), input), ErrReviewInactive)
	require.Zero(t, client.creates)
	require.Zero(t, client.submits)
}

func TestRemovedRequestCleansUpOwnedPendingReview(t *testing.T) {
	t.Parallel()
	input, client := publicationFixture(t)
	client.review = github.Review{ID: 101, NodeID: "pending", User: input.Reviewer,
		State: "PENDING", Body: "Summary\n\n" + lifecycleMarker(input, "complete"), CommitID: "head"}
	input.PullRequest.AssignmentID = 0
	input.PullRequest.Reviewers = nil
	client.current = input.PullRequest
	publisher := New(zap.NewNop(), client, github.Host, "")
	_, err := publisher.Finalize(t.Context(), input)
	require.NoError(t, err)
	require.Equal(t, 1, client.deletes)
	require.Zero(t, client.submits)
}

func TestInlineCommentsUseGitHubPathAndSide(t *testing.T) {
	t.Parallel()
	files, _, err := gitdiff.Parse(strings.NewReader(`diff --git a/old.go b/new.go
similarity index 50%
rename from old.go
rename to new.go
--- a/old.go
+++ b/new.go
@@ -1,2 +1,2 @@
 context
-old
+new
`))
	require.NoError(t, err)
	diff := github.Diff{Files: files}
	for _, finding := range []protocol.Finding{
		{Path: "old.go", Line: 2, Side: "LEFT"},
		{Path: "new.go", Line: 2, Side: "RIGHT"},
	} {
		comment, ok := inlineComment(diff, finding, "body")
		require.True(t, ok)
		require.Equal(t, github.InlineComment{Path: "new.go", Line: 2, Side: finding.Side, Body: "body"}, comment)
	}
	_, ok := inlineComment(diff, protocol.Finding{Path: "old.go", Line: 1, Side: "LEFT"}, "body")
	require.False(t, ok)
}
