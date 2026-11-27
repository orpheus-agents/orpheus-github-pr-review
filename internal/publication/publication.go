package publication

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/orpheus-agents/orpheus-github-pr-review/internal/github"
	"github.com/orpheus-agents/orpheus-github-pr-review/internal/protocol"
	"github.com/orpheus-agents/orpheus-github-pr-review/internal/review"
	"go.uber.org/zap"
)

var ErrStaleReview = errors.New("GitHub pull request diff is stale")
var ErrReviewInactive = errors.New("GitHub review request is no longer active")
var errPublicationUnconfirmed = errors.New("GitHub review publication is not confirmed")

type Client interface {
	GetPullRequest(context.Context, int64, int64) (github.PullRequest, error)
	GetDiff(context.Context, int64, int64, github.DiffRefs) (github.Diff, error)
	ListReviews(context.Context, int64, int64) ([]github.Review, error)
	ListPullRequestDiscussions(context.Context, int64, int64) ([]github.Discussion, error)
	CreateReview(context.Context, int64, int64, github.ReviewDraft) (github.Review, error)
	SubmitReview(context.Context, int64, int64, int64, string) error
	DeletePendingReview(context.Context, int64, int64, int64) error
	AddReviewReply(context.Context, string, string, string) error
	SetReviewThreadResolved(context.Context, string, bool) error
}

type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string {
	return fmt.Sprintf("GitHub publication failed (%s): %v", e.Code, e.Err)
}
func (e *Error) Unwrap() error { return e.Err }

func Code(err error) string {
	if failure, ok := errors.AsType[*Error](err); ok {
		return failure.Code
	}
	return "publication_failed"
}

func IsRetryable(err error) bool {
	return github.IsRetryable(err) || errors.Is(err, errPublicationUnconfirmed)
}

func classify(code string, err error) error {
	return &Error{Code: code, Err: err}
}

type Publisher struct {
	logger     *zap.Logger
	client     Client
	host       string
	webBaseURL string
}

func New(logger *zap.Logger, client Client, host, webBaseURL string) *Publisher {
	return &Publisher{logger: logger, client: client, host: host, webBaseURL: webBaseURL}
}

type threadAction struct {
	threadID string
	noteID   int64
	finding  string
	reply    string
	marker   string
	resolved bool
}

func (p *Publisher) Publish(ctx context.Context, input review.Input, bundle protocol.Bundle, notes ...review.NoteTemplates) error {
	if err := p.ensureCurrent(ctx, input); err != nil {
		return err
	}
	repo, number := input.PullRequest.RepositoryID, input.PullRequest.Number
	diff, err := p.client.GetDiff(ctx, repo, number, input.PullRequest.DiffRefs)
	if errors.Is(err, github.ErrReviewInputChanged) {
		return ErrStaleReview
	}
	if err != nil {
		return classify("load_diff", err)
	}
	threads, err := p.client.ListPullRequestDiscussions(ctx, repo, number)
	if err != nil {
		return classify("load_threads", err)
	}

	body := completionBody(len(bundle.Confirmed))
	if len(notes) > 0 && notes[0].Success != "" {
		body, err = review.RenderNote(notes[0].Success, review.NoteData{FindingsCount: len(bundle.Confirmed)})
		if err != nil {
			return classify("invalid_success_note", err)
		}
	}
	draft := github.ReviewDraft{Body: body}
	var actions []threadAction
	reviewedThreads := map[string]bool{}
	for _, finding := range bundle.Confirmed {
		if prior := finding.Previous; prior != nil {
			if reviewedThreads[prior.DiscussionID] {
				return classify("invalid_thread_plan", errors.New("multiple intents refer to the same GitHub thread"))
			}
			reviewedThreads[prior.DiscussionID] = true
			thread, ok := ownedThread(threads, input.Reviewer.ID, prior.DiscussionID, prior.NoteID, prior.Marker)
			if !ok {
				return classify("previous_finding_not_owned", errors.New("previous finding is not an owned GitHub review comment"))
			}
			switch {
			case thread.ResolutionCause == github.ResolutionCauseOutdatedByPush:
				// An outdated thread cannot anchor a finding in the current diff.
			case !thread.Resolved:
				continue
			case thread.ResolutionCause == github.ResolutionCauseUnknown:
				return classify("unknown_discussion_resolution_cause", errors.New("GitHub did not return the thread resolver"))
			case thread.ResolvedBy.ID != input.Reviewer.ID:
				marker := replyMarker(input, prior.DiscussionID, prior.Marker)
				actions = append(actions, threadAction{
					threadID: prior.DiscussionID, noteID: prior.NoteID, finding: prior.Marker,
					reply: prior.RecurrenceComment, marker: marker,
				})
				continue
			}
		}
		marker := FindingMarker(input.DiffFingerprint, finding)
		text := findingBody(finding, marker)
		if finding.Previous != nil {
			text = findingBody(finding, "")
			text += fmt.Sprintf("Previous GitHub comment: [discussion](%s#discussion_r%d).\n\n%s",
				input.PullRequest.WebURL, finding.Previous.NoteID, marker)
		}
		if comment, ok := inlineComment(diff, finding, text); ok {
			draft.Comments = append(draft.Comments, comment)
		} else {
			draft.Body += fmt.Sprintf("\n\n%s:%d (%s)\n\n%s", escapeFailureMessage(finding.Path), finding.Line, finding.Side, text)
		}
	}
	for _, recommendation := range bundle.Recommendations {
		draft.Body += "\n\n" + strings.TrimSpace(recommendation.Body)
	}
	for _, resolution := range bundle.Resolutions {
		if reviewedThreads[resolution.DiscussionID] {
			return classify("invalid_thread_plan", errors.New("multiple intents refer to the same GitHub thread"))
		}
		reviewedThreads[resolution.DiscussionID] = true
		thread, ok := ownedThread(threads, input.Reviewer.ID, resolution.DiscussionID, resolution.NoteID, resolution.Marker)
		if !ok {
			return classify("resolution_not_owned", errors.New("resolution target is not an owned GitHub review comment"))
		}
		if thread.Resolved {
			continue
		}
		actions = append(actions, threadAction{
			threadID: resolution.DiscussionID, noteID: resolution.NoteID, finding: resolution.Marker,
			reply: resolution.Body, marker: replyMarker(input, resolution.DiscussionID, resolution.Marker),
			resolved: true,
		})
	}
	return p.publish(ctx, input, "complete", draft, actions)
}

func (p *Publisher) applyThreadAction(ctx context.Context, input review.Input, draft github.Review, action threadAction) error {
	if err := p.ensureCurrent(ctx, input); err != nil {
		return err
	}
	repo, number := input.PullRequest.RepositoryID, input.PullRequest.Number
	threads, err := p.client.ListPullRequestDiscussions(ctx, repo, number)
	if err != nil {
		return classify("load_threads", err)
	}
	thread, ok := ownedThread(threads, input.Reviewer.ID, action.threadID, action.noteID, action.finding)
	if !ok {
		return classify("thread_not_owned", errors.New("GitHub thread changed during publication"))
	}
	if thread.Resolved == action.resolved {
		return nil
	}
	if thread.ResolutionCause == github.ResolutionCauseOutdatedByPush ||
		(!action.resolved && thread.ResolutionCause != github.ResolutionCauseExplicit) {
		return classify("thread_state_changed", errors.New("GitHub thread is no longer eligible for this action"))
	}
	if (action.resolved && !thread.ViewerCanResolve) || (!action.resolved && !thread.ViewerCanUnresolve) {
		return classify("thread_action_forbidden", errors.New("GitHub reviewer cannot change the thread state"))
	}
	hasReply := false
	for _, note := range thread.Notes {
		if note.Author.ID == input.Reviewer.ID && note.ReviewNodeID == draft.NodeID && hasTrailingMarker(note.Body, action.marker) {
			hasReply = true
			break
		}
	}
	if !hasReply {
		if !thread.ViewerCanReply {
			return classify("thread_reply_forbidden", errors.New("GitHub reviewer cannot reply to this thread"))
		}
		if err := p.ensureCurrent(ctx, input); err != nil {
			return err
		}
		body := strings.TrimSpace(action.reply) + "\n\n" + action.marker
		if err := p.client.AddReviewReply(ctx, thread.ID, draft.NodeID, body); err != nil {
			return classify("add_review_reply", err)
		}
	}
	if err := p.ensureCurrent(ctx, input); err != nil {
		return err
	}
	// Re-read the target after replying in case a human changed it meanwhile.
	threads, err = p.client.ListPullRequestDiscussions(ctx, repo, number)
	if err != nil {
		return classify("load_threads", err)
	}
	current, ok := ownedThread(threads, input.Reviewer.ID, action.threadID, action.noteID, action.finding)
	if !ok {
		return classify("thread_not_owned", errors.New("GitHub thread changed after reply"))
	}
	if current.Resolved != thread.Resolved || current.ResolvedBy != thread.ResolvedBy || current.ResolutionCause != thread.ResolutionCause {
		return nil
	}
	if err := p.client.SetReviewThreadResolved(ctx, thread.ID, action.resolved); err != nil {
		return classify("set_review_thread_state", err)
	}
	return nil
}

func (p *Publisher) ensureCurrent(ctx context.Context, input review.Input) error {
	repo, number := input.PullRequest.RepositoryID, input.PullRequest.Number
	current, err := p.client.GetPullRequest(ctx, repo, number)
	if err != nil {
		return classify("read_pull_state", err)
	}
	if current.State != "open" || current.AssignmentID <= 0 || current.AssignmentID != input.PullRequest.AssignmentID ||
		!hasReviewer(current.Reviewers, input.Reviewer.ID) {
		return ErrReviewInactive
	}
	if current.DiffRefs != input.PullRequest.DiffRefs || current.TargetBranch != input.PullRequest.TargetBranch ||
		current.SourceBranch != input.PullRequest.SourceBranch {
		return ErrStaleReview
	}
	return nil
}
