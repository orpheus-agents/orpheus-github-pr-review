package publication

import (
	"context"
	"errors"
	"html"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-github-pr-review/internal/github"
	"github.com/orpheus-agents/orpheus-github-pr-review/internal/review"
	"go.uber.org/zap"
)

// Message is a public Orpheus error. Raw transport errors and hook output are
// never placed in review comments.
type Failure struct {
	Notes     review.NoteTemplates
	Code      string
	Message   string
	SessionID string
}

const maxReviewTextBytes = 64000
const maxInlineComments = 100
const maxReviewPayloadBytes = 512 << 10

func (p *Publisher) Finalize(ctx context.Context, input review.Input) (bool, error) {
	pending, submitted, err := p.reviews(ctx, input)
	if err != nil {
		return false, err
	}
	if pending.ID == 0 {
		return submitted, nil
	}
	assignment, diff, _, _ := reviewMarkerParts(pending.Body)
	if !submitted && input.PullRequest.AssignmentID > 0 && input.PullRequest.State == "open" &&
		assignment == markerHash(review.AssignmentKey(input)) && diff == input.DiffFingerprint &&
		pending.CommitID == input.PullRequest.DiffRefs.HeadSHA {
		return false, nil
	}
	if err := p.deleteDraft(ctx, input, pending); err != nil {
		return false, err
	}
	return submitted, nil
}

func (p *Publisher) PublishError(ctx context.Context, input review.Input, failure Failure) error {
	reason := strings.TrimSpace(failure.Message)
	if reason == "" {
		reason = safeFailureReason(failure.Code)
	}
	template := failure.Notes.Fail
	if template == "" {
		template = "⚠️ Automated review could not be completed.\n\n> {{ .Reason }}"
	}
	body, err := review.RenderNote(template, review.NoteData{
		Reason: escapeFailureMessage(reason), SessionURL: p.sessionURL(failure.SessionID),
	})
	if err != nil {
		return classify("invalid_failure_note", err)
	}
	return p.publish(ctx, input, "failed", github.ReviewDraft{Body: body}, nil)
}

func (p *Publisher) PublishSkipped(ctx context.Context, input review.Input) error {
	return p.publish(ctx, input, "skipped", github.ReviewDraft{Body: "⏭️ No review workflow is configured for this repository."}, nil)
}

// Discard closes the same request with a stale-diff notice. A later request is
// independent of the cancelled run and remains eligible.
func (p *Publisher) Discard(ctx context.Context, input review.Input) error {
	repo, number := input.PullRequest.RepositoryID, input.PullRequest.Number
	current, err := p.client.GetPullRequest(ctx, repo, number)
	if err != nil {
		return classify("read_pull_state", err)
	}
	if current.AssignmentID != input.PullRequest.AssignmentID || current.State != "open" || current.AssignmentID <= 0 {
		return nil
	}
	input.PullRequest = current
	input.DiffFingerprint, err = review.DiffFingerprint(p.host, current)
	if err != nil {
		return classify("fingerprint_pull_state", err)
	}
	return p.publish(ctx, input, "stale", github.ReviewDraft{
		Body: "⚠️ The pull request diff changed. Request a review again to check the current version.",
	}, nil)
}

func (p *Publisher) publish(ctx context.Context, input review.Input, outcome string, spec github.ReviewDraft, actions []threadAction) error {
	if err := p.ensureCurrent(ctx, input); err != nil {
		return err
	}
	spec.CommitID = input.PullRequest.DiffRefs.HeadSHA
	spec.Body = strings.TrimSpace(spec.Body) + "\n\n" + lifecycleMarker(input, outcome)

	size := len(spec.Body)
	if size > maxReviewTextBytes || len(spec.Comments) > maxInlineComments {
		return classify("review_payload_too_large", errors.New("GitHub review exceeds its publication limit"))
	}
	for _, comment := range spec.Comments {
		if len(comment.Body) > maxReviewTextBytes {
			return classify("review_payload_too_large", errors.New("GitHub review comment exceeds its publication limit"))
		}
		size += len(comment.Body)
	}
	for _, action := range actions {
		if len(action.reply)+len(action.marker)+2 > maxReviewTextBytes {
			return classify("review_payload_too_large", errors.New("GitHub review reply exceeds its publication limit"))
		}
		size += len(action.reply) + len(action.marker) + 2
	}
	if size > maxReviewPayloadBytes {
		return classify("review_payload_too_large", errors.New("GitHub review payload exceeds its publication limit"))
	}

	pending, submitted, err := p.reviews(ctx, input)
	if err != nil || submitted {
		return err
	}
	if pending.ID != 0 && (!hasTrailingMarker(pending.Body, lifecycleMarker(input, outcome)) || pending.CommitID != spec.CommitID) {
		if err := p.deleteDraft(ctx, input, pending); err != nil {
			return err
		}
		pending = github.Review{}
	}
	if pending.ID == 0 {
		if err := p.ensureCurrent(ctx, input); err != nil {
			return err
		}
		repository, number := input.PullRequest.RepositoryID, input.PullRequest.Number
		pending, err = p.client.CreateReview(ctx, repository, number, spec)
		if err != nil || pending.ID <= 0 || pending.NodeID == "" || pending.User.ID != input.Reviewer.ID ||
			pending.State != "PENDING" || pending.CommitID != spec.CommitID || !hasTrailingMarker(pending.Body, lifecycleMarker(input, outcome)) {
			// A lost response can leave a durable draft. Read it before any retry.
			found, done, readErr := p.reviews(ctx, input)
			if readErr != nil {
				return classify("confirm_pending_review", errors.Join(errPublicationUnconfirmed, readErr))
			}
			if done {
				return nil
			}
			if found.ID <= 0 || found.NodeID == "" || found.CommitID != spec.CommitID ||
				!hasTrailingMarker(found.Body, lifecycleMarker(input, outcome)) {
				if err == nil {
					err = errPublicationUnconfirmed
				}
				return classify("create_pending_review", err)
			}
			pending = found
		}
	}
	for _, action := range actions {
		if err := p.applyThreadAction(ctx, input, pending, action); err != nil {
			return err
		}
	}
	if err := p.ensureCurrent(ctx, input); err != nil {
		return err
	}
	repo, number := input.PullRequest.RepositoryID, input.PullRequest.Number
	if err := p.client.SubmitReview(ctx, repo, number, pending.ID, pending.Body); err != nil {
		_, done, readErr := p.reviews(ctx, input)
		if readErr != nil {
			return classify("confirm_submitted_review", errors.Join(errPublicationUnconfirmed, readErr))
		}
		if !done {
			return classify("submit_review", err)
		}
	}
	p.logger.Info("published GitHub review", zap.String("pull_request_key", input.PRKey),
		zap.Int64("github_review_id", pending.ID), zap.String("outcome", outcome))
	return nil
}

func (p *Publisher) reviews(ctx context.Context, input review.Input) (github.Review, bool, error) {
	items, err := p.client.ListReviews(ctx, input.PullRequest.RepositoryID, input.PullRequest.Number)
	if err != nil {
		return github.Review{}, false, classify("list_reviews", err)
	}
	assignmentHash := markerHash(review.AssignmentKey(input))
	var pending github.Review
	var submitted bool
	for _, item := range items {
		if item.User.ID != input.Reviewer.ID {
			continue
		}
		assignment, _, _, ours := reviewMarkerParts(item.Body)
		if item.State == "PENDING" {
			if !ours || item.ID <= 0 || item.NodeID == "" || pending.ID != 0 {
				return github.Review{}, false, classify("unmanaged_pending_review", errors.New("unexpected pending GitHub review for the technical user"))
			}
			pending = item
			continue
		}
		if ours && item.State == "COMMENTED" && assignment == assignmentHash {
			submitted = true
		}
	}
	return pending, submitted, nil
}

func (p *Publisher) deleteDraft(ctx context.Context, input review.Input, pending github.Review) error {
	repo, number := input.PullRequest.RepositoryID, input.PullRequest.Number
	err := p.client.DeletePendingReview(ctx, repo, number, pending.ID)
	if err == nil {
		return nil
	}
	current, _, readErr := p.reviews(ctx, input)
	if readErr != nil {
		return classify("confirm_deleted_review", errors.Join(errPublicationUnconfirmed, readErr))
	}
	if current.ID == pending.ID {
		return classify("delete_pending_review", err)
	}
	return nil
}

func escapeFailureMessage(message string) string {
	message = strings.ReplaceAll(strings.ReplaceAll(message, "\r\n", "\n"), "\r", "\n")
	message = html.EscapeString(message)
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "~", "\\~", "@", "&#64;").Replace(message)
}

func (p *Publisher) sessionURL(sessionID string) string {
	if _, err := uuid.Parse(sessionID); err != nil {
		return ""
	}
	base, err := url.Parse(p.webBaseURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return ""
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/sessions/" + sessionID
	base.RawPath = ""
	// Escape Markdown delimiters as well as URL delimiters.
	return strings.NewReplacer("(", "%28", ")", "%29").Replace(base.String())
}

func safeFailureReason(code string) string {
	switch code {
	case "invalid_pull_request_diff":
		return "GitHub returned an incomplete diff or the diff exceeds the review limit"
	case "review_payload_too_large":
		return "the review exceeds the GitHub publication limit"
	case "session_request_too_large":
		return "the review request exceeds the configured size limit"
	case "invalid_session_contract":
		return "the review environment could not be prepared"
	case "run_failed", "run_cancelled", "agent_not_completed":
		return "the review run did not finish successfully"
	case "after_run_missing", "after_run_failed", "after_run_output_incomplete", "multiple_after_run_results":
		return "the review result could not be collected and verified (" + code + ")"
	case "unknown_discussion_resolution_cause":
		return "cannot determine how a GitHub discussion was resolved (unknown_discussion_resolution_cause)"
	case "invalid_discussion_resolution_state":
		return "a GitHub discussion is resolved without a resolution cause (invalid_discussion_resolution_state)"
	case "previous_finding_not_owned", "resolution_not_owned":
		return "the target discussion is not an Orpheus finding (" + code + ")"
	case "previous_finding_disappeared", "resolution_target_disappeared":
		return "the target GitHub discussion disappeared during publication (" + code + ")"
	case "invalid_session_metadata", "session_metadata_mismatch":
		return "the saved session metadata is invalid or does not match this review (" + code + ")"
	case "publisher_not_configured", "invalid_success_note", "invalid_failure_note":
		return "the review publication configuration is invalid (" + code + ")"
	case "list_discussions", "list_diffs", "read_current_merge_request", "create_finding",
		"reopen_discussion", "resolve_discussion", "create_resolution_reply", "create_note", "remove_reviewer":
		return "a GitHub operation failed during review publication (" + code + ")"
	case "envelope_too_large", "invalid_envelope_json", "unsupported_envelope_schema", "unsupported_envelope_encoding",
		"declared_payload_size_out_of_bounds", "invalid_payload_base64", "payload_size_mismatch", "payload_digest_mismatch",
		"invalid_payload_json", "payload_too_large", "unsupported_bundle_schema", "bundle_not_ready", "pending_findings",
		"count_mismatch", "too_many_items", "workflow_identity_mismatch", "review_identity_mismatch", "review_fingerprint_mismatch",
		"findings_not_sorted", "recommendations_not_sorted", "resolutions_not_sorted", "invalid_recommendation", "invalid_resolution",
		"invalid_finding_id", "duplicate_finding_id", "invalid_finding_path", "invalid_finding_line", "invalid_finding_severity",
		"invalid_finding_content", "invalid_previous_finding", "invalid_payload_compression":
		return "the review result failed validation (" + code + ")"
	default:
		return "the review result could not be safely validated or published"
	}
}
