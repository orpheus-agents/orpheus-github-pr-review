package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"text/template"

	"github.com/orpheus-agents/orpheus-github-pr-review/internal/github"
	"github.com/orpheus-agents/orpheus-github-pr-review/internal/orpheus"
	"github.com/orpheus-agents/orpheus-github-pr-review/internal/review"
)

var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Options struct {
	Notes              review.NoteTemplates
	WorkflowID         string
	Language           string
	GitHubHost         string
	Instructions       string
	AgentProfile       string
	AgentModel         string
	SandboxTemplate    string
	Services           []string
	RunTimeoutSeconds  int
	HookTimeoutSeconds int
	MaxRequestBytes    int
}

type SessionContract struct {
	Key              orpheus.ReviewSessionKey
	ReviewerID       int64
	Request          orpheus.CreateSessionRequest
	Metadata         MetadataV1
	Prompt           string
	WorkflowRevision string
	RequestBytes     int
}

type RequestTooLargeError struct {
	Size  int
	Limit int
}

func (e *RequestTooLargeError) Error() string {
	return fmt.Sprintf("Orpheus session request is too large: %d bytes exceeds %d byte limit", e.Size, e.Limit)
}

func BuildSessionContract(input review.Input, options Options) (SessionContract, error) {
	options, err := validateOptions(options)
	if err != nil {
		return SessionContract{}, err
	}
	if err := validateInput(input); err != nil {
		return SessionContract{}, err
	}

	revision, err := Revision(options)
	if err != nil {
		return SessionContract{}, err
	}
	artifactsPath := ".orpheus/reviews/" + input.DiffFingerprint
	helperSHA256 := helperDigest()
	metadata := newMetadata(input, options.WorkflowID, options.GitHubHost, artifactsPath, revision, helperSHA256)
	if !options.Notes.Empty() {
		metadata.Notes = new(options.Notes)
	}
	hooks, err := renderHooks(
		metadata,
		strings.TrimSpace(input.Repository.CloneURL),
		input.PullRequest.Number,
		options.HookTimeoutSeconds,
	)
	if err != nil {
		return SessionContract{}, err
	}
	prompt, err := renderPrompt(input, artifactsPath)
	if err != nil {
		return SessionContract{}, fmt.Errorf("render review prompt: %w", err)
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return SessionContract{}, fmt.Errorf("encode session metadata: %w", err)
	}
	metadataRaw := json.RawMessage(metadataJSON)
	messageExternalKey := MessageExternalKey(input.ReviewFingerprint)
	allowMultipleRuns := false
	key := orpheus.ReviewSessionKey{
		Namespace:         Namespace,
		PRKey:             input.PRKey,
		ReviewFingerprint: input.ReviewFingerprint,
		AssignmentID:      input.PullRequest.AssignmentID,
	}
	request := orpheus.CreateSessionRequest{
		AllowMultipleRuns: &allowMultipleRuns,
		Namespace:         &key.Namespace,
		ExternalKey:       &key.PRKey,
		InputFingerprint:  &key.ReviewFingerprint,
		Configuration: orpheus.ConfigurationInput{
			Agent: orpheus.AgentInput{
				Profile:      options.AgentProfile,
				Instructions: new(agentInstructions(options)),
			},
			Sandbox: orpheus.SandboxInput{
				Template: options.SandboxTemplate,
				Env:      new(hooks.Environment),
			},
			Hooks: &hooks.Hooks,
			Limits: &orpheus.LimitsInput{
				RunTimeoutSeconds: &options.RunTimeoutSeconds,
			},
		},
		Env: new(hooks.Environment),
		Messages: []orpheus.TextMessage{{
			Text:        prompt,
			ExternalKey: &messageExternalKey,
			Metadata:    &metadataRaw,
		}},
	}
	if options.AgentModel != "" {
		request.Configuration.Agent.Model = &options.AgentModel
	}
	request.Configuration.Sandbox.Services = new(append([]string{}, options.Services...))

	encodedRequest, err := json.Marshal(request)
	if err != nil {
		return SessionContract{}, fmt.Errorf("encode Orpheus session request: %w", err)
	}
	if len(encodedRequest) > options.MaxRequestBytes {
		return SessionContract{}, &RequestTooLargeError{
			Size:  len(encodedRequest),
			Limit: options.MaxRequestBytes,
		}
	}

	return SessionContract{
		Key:              key,
		ReviewerID:       input.Reviewer.ID,
		Request:          request,
		Metadata:         metadata,
		Prompt:           prompt,
		WorkflowRevision: revision,
		RequestBytes:     len(encodedRequest),
	}, nil
}

func MessageExternalKey(reviewFingerprint string) string {
	return "github-pr-review-input-v1:" + reviewFingerprint
}

func validateOptions(options Options) (Options, error) {
	if err := options.Notes.Validate(); err != nil {
		return Options{}, fmt.Errorf("build session contract: %w", err)
	}
	options.GitHubHost = strings.TrimRight(strings.TrimSpace(options.GitHubHost), "/")
	options.AgentProfile = strings.TrimSpace(options.AgentProfile)
	options.AgentModel = strings.TrimSpace(options.AgentModel)
	options.SandboxTemplate = strings.TrimSpace(options.SandboxTemplate)

	switch {
	case !ValidID(options.WorkflowID):
		return Options{}, errors.New("build session contract: valid workflow ID is required")
	case options.Language != "" && !languagePattern.MatchString(options.Language):
		return Options{}, errors.New("build session contract: invalid language tag")
	case options.Services == nil:
		return Options{}, errors.New("build session contract: services must be explicitly configured")
	case options.GitHubHost == "":
		return Options{}, errors.New("build session contract: GitHub host is required")
	case strings.TrimSpace(options.Instructions) == "":
		return Options{}, errors.New("build session contract: instructions are required")
	case options.AgentProfile == "":
		return Options{}, errors.New("build session contract: agent profile is required")
	case options.SandboxTemplate == "":
		return Options{}, errors.New("build session contract: sandbox template is required")
	case options.RunTimeoutSeconds <= 0:
		return Options{}, errors.New("build session contract: run timeout must be positive")
	case options.HookTimeoutSeconds <= 0:
		return Options{}, errors.New("build session contract: hook timeout must be positive")
	case options.MaxRequestBytes <= 0:
		return Options{}, errors.New("build session contract: maximum request size must be positive")
	}

	return options, nil
}

func validateInput(input review.Input) error {
	switch {
	case strings.TrimSpace(input.PRKey) == "":
		return errors.New("build session contract: PR key is required")
	case input.PullRequest.RepositoryID <= 0:
		return errors.New("build session contract: repository ID must be positive")
	case input.PullRequest.Number <= 0:
		return errors.New("build session contract: pull request Number must be positive")
	case strings.TrimSpace(input.Repository.FullName) == "":
		return errors.New("build session contract: repository path is required")
	case strings.TrimSpace(input.Repository.CloneURL) == "":
		return errors.New("build session contract: repository HTTPS clone URL is required")
	case strings.TrimSpace(input.PullRequest.WebURL) == "":
		return errors.New("build session contract: pull request URL is required")
	case input.PullRequest.AssignmentID <= 0:
		return errors.New("build session contract: review request ID is required")
	case input.Reviewer.ID <= 0:
		return errors.New("build session contract: reviewer ID must be positive")
	case !fingerprintPattern.MatchString(input.DiffFingerprint):
		return errors.New("build session contract: invalid diff fingerprint")
	case !fingerprintPattern.MatchString(input.ReviewFingerprint):
		return errors.New("build session contract: invalid review fingerprint")
	case strings.TrimSpace(input.PullRequest.DiffRefs.BaseSHA) == "":
		return errors.New("build session contract: base SHA is required")
	case strings.TrimSpace(input.PullRequest.DiffRefs.Digest) == "":
		return errors.New("build session contract: GitHub diff digest is required")
	case strings.TrimSpace(input.PullRequest.DiffRefs.HeadSHA) == "":
		return errors.New("build session contract: head SHA is required")
	}
	cloneURL, err := url.Parse(strings.TrimSpace(input.Repository.CloneURL))
	if err != nil || cloneURL.Scheme != "https" || cloneURL.Hostname() == "" {
		return errors.New("build session contract: repository clone URL must be an absolute HTTPS URL")
	}

	return nil
}

func newMetadata(input review.Input, workflowID, host, artifactsPath, revision, helperSHA256 string) MetadataV1 {
	return MetadataV1{
		SchemaVersion:    MetadataSchemaVersion,
		WorkflowID:       workflowID,
		WorkflowRevision: revision,
		GitHub: GitHubMetadata{
			Host:              host,
			RepositoryID:      input.PullRequest.RepositoryID,
			RepositoryPath:    input.Repository.FullName,
			PullRequestNumber: input.PullRequest.Number,
			PullRequestURL:    input.PullRequest.WebURL,
			AssignmentID:      input.PullRequest.AssignmentID,
			ReviewerUserID:    input.Reviewer.ID,
			ReviewerUsername:  input.Reviewer.Username,
		},
		Review: ReviewMetadata{
			DiffFingerprint:   input.DiffFingerprint,
			ReviewFingerprint: input.ReviewFingerprint,
			ArtifactsPath:     artifactsPath,
		},
		DiffRefs: DiffRefsMetadata{
			BaseSHA: input.PullRequest.DiffRefs.BaseSHA,
			Digest:  input.PullRequest.DiffRefs.Digest,
			HeadSHA: input.PullRequest.DiffRefs.HeadSHA,
		},
		Protocol: ProtocolMetadata{
			ArtifactSchemaVersion: ArtifactSchemaVersion,
			BundleSchemaVersion:   BundleSchemaVersion,
			HelperSHA256:          helperSHA256,
		},
	}
}

type promptContext struct {
	PRKey             string
	PullRequestNumber int64
	Title             string
	Description       string
	State             string
	Draft             bool
	PullRequestURL    string
	RepositoryPath    string
	SourceBranch      string
	TargetBranch      string
	ReviewerUserID    int64
	ReviewerUsername  string
	BaseSHA           string
	Digest            string
	HeadSHA           string
	DiffFingerprint   string
	ReviewFingerprint string
	ArtifactsPath     string
	PreviousThreads   string
}

func renderPrompt(input review.Input, artifactsPath string) (string, error) {
	promptTemplate, err := template.New("pull-request.md").Option("missingkey=error").Parse(promptAsset)
	if err != nil {
		return "", err
	}
	threads := []github.Discussion{}
	for _, thread := range input.Discussions {
		if len(thread.Notes) > 0 && thread.Notes[0].Author.ID == input.Reviewer.ID {
			threads = append(threads, thread)
		}
	}
	previousThreads, err := json.Marshal(threads)
	if err != nil {
		return "", err
	}
	context := promptContext{
		PreviousThreads:   string(previousThreads),
		PRKey:             input.PRKey,
		PullRequestNumber: input.PullRequest.Number,
		Title:             input.PullRequest.Title,
		Description:       input.PullRequest.Description,
		State:             input.PullRequest.State,
		Draft:             input.PullRequest.Draft,
		PullRequestURL:    input.PullRequest.WebURL,
		RepositoryPath:    input.Repository.FullName,
		SourceBranch:      input.PullRequest.SourceBranch,
		TargetBranch:      input.PullRequest.TargetBranch,
		ReviewerUserID:    input.Reviewer.ID,
		ReviewerUsername:  input.Reviewer.Username,
		BaseSHA:           input.PullRequest.DiffRefs.BaseSHA,
		Digest:            input.PullRequest.DiffRefs.Digest,
		HeadSHA:           input.PullRequest.DiffRefs.HeadSHA,
		DiffFingerprint:   input.DiffFingerprint,
		ReviewFingerprint: input.ReviewFingerprint,
		ArtifactsPath:     artifactsPath,
	}

	var rendered bytes.Buffer
	if err := promptTemplate.Execute(&rendered, context); err != nil {
		return "", err
	}

	return rendered.String(), nil
}

func agentInstructions(options Options) string {
	if options.Language == "" {
		return options.Instructions
	}
	return options.Instructions + "\n\nReview output language: " + options.Language + ". Use this language for the final response, finding titles and bodies, recommendations, resolution explanations and recurrence comments. Keep protocol field names, enum values and markers unchanged.\n"
}
