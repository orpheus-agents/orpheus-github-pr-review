package publication

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"

	"github.com/orpheus-agents/orpheus-github-pr-review/internal/github"
	"github.com/orpheus-agents/orpheus-github-pr-review/internal/protocol"
	"github.com/orpheus-agents/orpheus-github-pr-review/internal/review"
)

const reviewMarkerPrefix = "<!-- orpheus-github-review:"
const findingMarkerPrefix = "<!-- orpheus-review-finding:"
const markerSuffix = " -->"

func lifecycleMarker(input review.Input, outcome string) string {
	return reviewMarkerPrefix + markerHash(review.AssignmentKey(input)) + ":" + input.DiffFingerprint + ":" + outcome + markerSuffix
}

func reviewMarkerParts(body string) (assignment, diff, outcome string, ok bool) {
	marker := trailingMarker(body)
	if !strings.HasPrefix(marker, reviewMarkerPrefix) || !strings.HasSuffix(marker, markerSuffix) {
		return "", "", "", false
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(marker, reviewMarkerPrefix), markerSuffix), ":")
	if len(parts) != 3 || !validHash(parts[0]) || !validHash(parts[1]) {
		return "", "", "", false
	}
	switch parts[2] {
	case "complete", "failed", "skipped", "stale":
		return parts[0], parts[1], parts[2], true
	default:
		return "", "", "", false
	}
}

func FindingMarker(diffFingerprint string, finding protocol.Finding) string {
	payload := strings.Join([]string{diffFingerprint, finding.ID, finding.Path, strconv.Itoa(finding.Line),
		finding.Side, finding.Severity, finding.Title, finding.Body, finding.Source}, "\x00")
	return findingMarkerPrefix + markerHash(payload) + markerSuffix
}

func replyMarker(input review.Input, threadID, finding string) string {
	payload := strings.Join([]string{review.AssignmentKey(input), threadID, finding}, "\x00")
	return "<!-- orpheus-review-reply:" + markerHash(payload) + markerSuffix
}

func markerHash(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func findingBody(finding protocol.Finding, marker string) string {
	return fmt.Sprintf("**%s**\n\n%s\n\nSeverity: `%s`  \nSource: `%s`\n\n%s",
		strings.TrimSpace(finding.Title), strings.TrimSpace(finding.Body), finding.Severity, finding.Source, marker)
}

func completionBody(count int) string {
	return fmt.Sprintf("✅ Review of the current pull request is complete. Confirmed findings: %d.", count)
}

func ownedThread(threads []github.Discussion, reviewerID int64, threadID string, commentID int64, marker string) (github.Discussion, bool) {
	if reviewerID <= 0 || !strings.HasPrefix(marker, findingMarkerPrefix) || !strings.HasSuffix(marker, markerSuffix) ||
		!validHash(strings.TrimSuffix(strings.TrimPrefix(marker, findingMarkerPrefix), markerSuffix)) {
		return github.Discussion{}, false
	}
	for _, thread := range threads {
		if thread.ID != threadID || len(thread.Notes) == 0 {
			continue
		}
		first := thread.Notes[0]
		if first.ID == commentID && first.Author.ID == reviewerID && hasTrailingMarker(first.Body, marker) {
			return thread, true
		}
	}
	return github.Discussion{}, false
}

func validHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, character := range hash {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func hasTrailingMarker(body, marker string) bool { return trailingMarker(body) == marker }

func trailingMarker(body string) string {
	trimmed := strings.TrimSpace(body)
	if index := strings.LastIndexByte(trimmed, '\n'); index >= 0 {
		trimmed = strings.TrimSpace(trimmed[index+1:])
	}
	return trimmed
}

func hasReviewer(reviewers []github.User, reviewerID int64) bool {
	for _, reviewer := range reviewers {
		if reviewer.ID == reviewerID {
			return true
		}
	}
	return false
}
