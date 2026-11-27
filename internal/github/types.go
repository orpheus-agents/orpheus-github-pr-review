package github

import (
	"time"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"login"`
}

type DiffRefs struct {
	BaseSHA string
	Digest  string
	HeadSHA string
}

type PullRequest struct {
	ID               int64
	RepositoryID     int64
	Number           int64
	Title            string
	Description      string
	State            string
	SourceBranch     string
	TargetBranch     string
	WebURL           string
	Draft            bool
	UpdatedAt        time.Time
	Reviewers        []User
	AssignmentID     int64
	DiffProblem      bool
	HasPendingReview bool
	DiffRefs         DiffRefs
}

type Repository struct {
	ID       int64
	FullName string
	CloneURL string
	WebURL   string
	Archived bool
}

type InlineComment struct {
	Path string `json:"path"`
	Line int64  `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

type Review struct {
	ID       int64  `json:"id"`
	NodeID   string `json:"node_id"`
	User     User   `json:"user"`
	Body     string `json:"body"`
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
}

type ReviewDraft struct {
	CommitID string          `json:"commit_id"`
	Body     string          `json:"body"`
	Comments []InlineComment `json:"comments,omitempty"`
}

type Diff struct {
	Digest string
	Files  []*gitdiff.File
}

type ResolutionCause string

const (
	ResolutionCauseNone           ResolutionCause = ""
	ResolutionCauseExplicit       ResolutionCause = "explicit"
	ResolutionCauseOutdatedByPush ResolutionCause = "outdated_by_push"
	ResolutionCauseUnknown        ResolutionCause = "unknown"
)

type Note struct {
	ID             int64
	CreatedAt      time.Time
	Body           string
	Author         User
	System         bool
	Resolvable     bool
	Resolved       bool
	ResolvedAt     *time.Time
	ResolvedBy     User
	ResolvedByPush bool
	ReviewNodeID   string
	Pending        bool
}

type Discussion struct {
	ID                 string
	Resolvable         bool
	Resolved           bool
	ResolutionCause    ResolutionCause
	ResolvedAt         *time.Time
	ResolvedBy         User
	Notes              []Note
	ViewerCanResolve   bool
	ViewerCanUnresolve bool
	ViewerCanReply     bool
}

type ReviewInput struct {
	Discussions []Discussion
	PullRequest PullRequest
	Repository  Repository
	Notes       []Note
}
