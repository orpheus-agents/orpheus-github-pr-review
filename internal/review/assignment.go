package review

import "fmt"

// AssignmentKey binds a lifecycle to the exact GitHub review request.
func AssignmentKey(input Input) string {
	return fmt.Sprintf("%s:%d:%d", input.PRKey, input.Reviewer.ID, input.PullRequest.AssignmentID)
}
