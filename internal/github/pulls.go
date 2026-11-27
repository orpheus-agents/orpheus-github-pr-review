package github

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

const diffMediaType = "application/vnd.github.diff"
const maxDiffBytes = 8 << 20

var errInvalidDiff = errors.New("GitHub pull request diff is incomplete or exceeds the review limit")

type gitRef struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo struct {
		ID int64 `json:"id"`
	} `json:"repo"`
}

type pullResponse struct {
	ID           int64     `json:"id"`
	Number       int64     `json:"number"`
	Title        string    `json:"title"`
	Body         string    `json:"body"`
	State        string    `json:"state"`
	WebURL       string    `json:"html_url"`
	Draft        bool      `json:"draft"`
	UpdatedAt    time.Time `json:"updated_at"`
	Reviewers    []User    `json:"requested_reviewers"`
	Base         gitRef    `json:"base"`
	Head         gitRef    `json:"head"`
	ChangedFiles int       `json:"changed_files"`
	Additions    int64     `json:"additions"`
	Deletions    int64     `json:"deletions"`
}

type timelineEvent struct {
	ID        int64     `json:"id"`
	Event     string    `json:"event"`
	Reviewer  User      `json:"requested_reviewer"`
	User      User      `json:"user"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

func pullPath(repository Repository, number int64) string {
	return fmt.Sprintf("/repos/%s/pulls/%d", repository.FullName, number)
}

func (c *Client) pull(ctx context.Context, repository Repository, number int64) (pullResponse, error) {
	var item pullResponse
	if err := c.json(ctx, http.MethodGet, pullPath(repository, number), nil, &item); err != nil {
		return pullResponse{}, err
	}
	if item.ID <= 0 || item.Number != number || item.Base.Repo.ID != repository.ID {
		return pullResponse{}, errors.New("inconsistent GitHub pull request identity")
	}
	return item, nil
}

func (c *Client) timeline(ctx context.Context, repository Repository, number int64) ([]timelineEvent, error) {
	return listREST[timelineEvent](ctx, c, fmt.Sprintf("/repos/%s/issues/%d/timeline", repository.FullName, number))
}

func assignmentID(events []timelineEvent, reviewer int64) int64 {
	var latest timelineEvent
	for _, event := range events {
		if event.Reviewer.ID != reviewer || event.ID <= latest.ID {
			continue
		}
		if event.Event == "review_requested" || event.Event == "review_request_removed" {
			latest = event
		}
	}
	if latest.Event == "review_requested" {
		return latest.ID
	}
	return 0
}

func (c *Client) GetPullRequest(ctx context.Context, repositoryID, number int64) (PullRequest, error) {
	repository, err := c.repository(repositoryID)
	if err != nil {
		return PullRequest{}, err
	}
	item, err := c.pull(ctx, repository, number)
	if err != nil {
		return PullRequest{}, err
	}
	return c.pullState(ctx, repository, item)
}

func (c *Client) pullState(ctx context.Context, repository Repository, item pullResponse) (PullRequest, error) {
	request := PullRequest{
		ID: item.ID, Number: item.Number, RepositoryID: repository.ID, Title: item.Title, Description: item.Body,
		State: item.State, SourceBranch: item.Head.Ref, TargetBranch: item.Base.Ref, WebURL: item.WebURL,
		Draft: item.Draft, UpdatedAt: item.UpdatedAt, Reviewers: item.Reviewers,
		DiffRefs: DiffRefs{BaseSHA: item.Base.SHA, HeadSHA: item.Head.SHA},
	}
	if request.State != "open" || !slices.ContainsFunc(request.Reviewers, func(user User) bool { return user.ID == c.viewer.ID }) {
		return request, nil
	}
	events, err := c.timeline(ctx, repository, item.Number)
	if err != nil {
		return PullRequest{}, err
	}
	request.AssignmentID = assignmentID(events, c.viewer.ID)
	if request.AssignmentID == 0 {
		return PullRequest{}, ErrReviewInputChanged
	}
	diff, err := c.readDiff(ctx, repository, item)
	if errors.Is(err, errInvalidDiff) {
		request.DiffProblem = true
	} else if err != nil {
		return PullRequest{}, err
	} else {
		request.DiffRefs.Digest = diff.Digest
	}
	return request, nil
}

func (c *Client) GetReviewInput(ctx context.Context, repositoryID, number int64) (ReviewInput, error) {
	repository, err := c.repository(repositoryID)
	if err != nil {
		return ReviewInput{}, err
	}
	item, err := c.pull(ctx, repository, number)
	if err != nil {
		return ReviewInput{}, err
	}
	request, err := c.pullState(ctx, repository, item)
	if err != nil {
		return ReviewInput{}, err
	}
	input := ReviewInput{PullRequest: request, Repository: repository}
	if request.AssignmentID == 0 || request.DiffProblem {
		return input, nil
	}

	events, err := c.timeline(ctx, repository, number)
	if err != nil {
		return ReviewInput{}, err
	}
	if request.AssignmentID != assignmentID(events, c.viewer.ID) {
		return ReviewInput{}, ErrReviewInputChanged
	}
	for _, event := range events {
		if (event.Event == "commented" || event.Event == "reviewed") && event.User.ID != c.viewer.ID && strings.TrimSpace(event.Body) != "" {
			input.Notes = append(input.Notes, Note{ID: event.ID, Author: event.User, Body: event.Body, CreatedAt: event.CreatedAt})
		}
	}
	discussions, err := c.ListPullRequestDiscussions(ctx, repositoryID, number)
	if err != nil {
		return ReviewInput{}, err
	}
	for _, thread := range discussions {
		thread.Notes = slices.DeleteFunc(thread.Notes, func(note Note) bool { return note.Pending })
		if len(thread.Notes) > 0 {
			input.Discussions = append(input.Discussions, thread)
		}
	}
	confirmed, err := c.pull(ctx, repository, number)
	if err != nil {
		return ReviewInput{}, err
	}
	if confirmed.Base != item.Base || confirmed.Head != item.Head || confirmed.State != item.State ||
		confirmed.UpdatedAt != item.UpdatedAt || !slices.Equal(confirmed.Reviewers, item.Reviewers) {
		return ReviewInput{}, ErrReviewInputChanged
	}
	return input, nil
}

func (c *Client) GetDiff(ctx context.Context, repositoryID, number int64, refs DiffRefs) (Diff, error) {
	repository, err := c.repository(repositoryID)
	if err != nil {
		return Diff{}, err
	}
	item, err := c.pull(ctx, repository, number)
	if err != nil {
		return Diff{}, err
	}
	diff, err := c.readDiff(ctx, repository, item)
	if err != nil {
		return Diff{}, err
	}
	if item.Base.SHA != refs.BaseSHA || item.Head.SHA != refs.HeadSHA || diff.Digest != refs.Digest {
		return Diff{}, ErrReviewInputChanged
	}
	return diff, nil
}

func (c *Client) readDiff(ctx context.Context, repository Repository, pull pullResponse) (Diff, error) {
	body, err := c.request(ctx, http.MethodGet, pullPath(repository, pull.Number), diffMediaType, nil, maxDiffBytes)
	if errors.Is(err, errResponseTooLarge) {
		return Diff{}, errInvalidDiff
	}
	if response, ok := errors.AsType[*responseError](err); ok && (response.status == 406 || response.status == 413 || response.status == 422) {
		return Diff{}, errInvalidDiff
	}
	if err != nil {
		return Diff{}, err
	}
	return parseDiff(body, pull.ChangedFiles, pull.Additions, pull.Deletions)
}

func parseDiff(body []byte, count int, additions, deletions int64) (Diff, error) {
	files, preamble, err := gitdiff.Parse(bytes.NewReader(body))
	if err != nil || len(files) != count || strings.TrimSpace(preamble) != "" {
		return Diff{}, errInvalidDiff
	}
	var added, removed int64
	for _, file := range files {
		for _, fragment := range file.TextFragments {
			if err := fragment.Validate(); err != nil {
				return Diff{}, errInvalidDiff
			}
			added += fragment.LinesAdded
			removed += fragment.LinesDeleted
		}
	}
	if added != additions || removed != deletions {
		return Diff{}, errInvalidDiff
	}
	return Diff{Digest: fmt.Sprintf("%x", sha256.Sum256(body)), Files: files}, nil
}
