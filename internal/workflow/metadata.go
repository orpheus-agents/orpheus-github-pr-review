package workflow

import "github.com/orpheus-agents/orpheus-github-pr-review/internal/review"

const (
	Namespace             = "github/pr-review"
	MetadataSchemaVersion = 1
	ArtifactSchemaVersion = 1
	BundleSchemaVersion   = 1
)

type MetadataV1 struct {
	Notes            *review.NoteTemplates `json:"notes,omitempty"`
	SchemaVersion    int                   `json:"schema_version"`
	WorkflowID       string                `json:"workflow_id"`
	WorkflowRevision string                `json:"workflow_revision"`
	GitHub           GitHubMetadata        `json:"github"`
	Review           ReviewMetadata        `json:"review"`
	DiffRefs         DiffRefsMetadata      `json:"diff_refs"`
	Protocol         ProtocolMetadata      `json:"protocol"`
}

type GitHubMetadata struct {
	Host              string `json:"host"`
	RepositoryID      int64  `json:"repository_id"`
	RepositoryPath    string `json:"repository_path"`
	PullRequestNumber int64  `json:"pr_number"`
	PullRequestURL    string `json:"pr_url"`
	AssignmentID      int64  `json:"assignment_id"`
	ReviewerUserID    int64  `json:"reviewer_user_id"`
	ReviewerUsername  string `json:"reviewer_username"`
}

type ReviewMetadata struct {
	DiffFingerprint   string `json:"diff_fingerprint"`
	ReviewFingerprint string `json:"review_fingerprint"`
	ArtifactsPath     string `json:"artifacts_path"`
}

type DiffRefsMetadata struct {
	BaseSHA string `json:"base_sha"`
	Digest  string `json:"diff_digest"`
	HeadSHA string `json:"head_sha"`
}

type ProtocolMetadata struct {
	ArtifactSchemaVersion int    `json:"artifact_schema_version"`
	BundleSchemaVersion   int    `json:"bundle_schema_version"`
	HelperSHA256          string `json:"helper_sha256"`
}
