package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

type pageInfo struct {
	HasNextPage bool
	EndCursor   string
}

type connection[T any] struct {
	Nodes    []T
	PageInfo pageInfo
}

func (p pageInfo) next(previous string) (*string, error) {
	if !p.HasNextPage {
		return nil, nil
	}
	if p.EndCursor == "" || p.EndCursor == previous {
		return nil, errors.New("invalid GitHub GraphQL pagination cursor")
	}
	return &p.EndCursor, nil
}

func (c *Client) graphQL(ctx context.Context, query string, variables map[string]any, result any) error {
	var response struct {
		Data   json.RawMessage
		Errors []struct {
			Type    string
			Message string
		}
	}
	payload := map[string]any{"query": query, "variables": variables}
	if err := c.json(ctx, http.MethodPost, "/graphql", payload, &response); err != nil {
		return err
	}
	for _, failure := range response.Errors {
		if failure.Type == "RATE_LIMITED" || failure.Type == "ABUSE_DETECTED" || strings.Contains(failure.Message, "secondary rate limit") {
			c.pause(nil)
			return ErrRateLimited
		}
	}
	if len(response.Errors) > 0 || len(response.Data) == 0 {
		return errors.New("GitHub GraphQL operation was rejected")
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(response.Data, result); err != nil {
		return fmt.Errorf("decode GitHub GraphQL data: %w", err)
	}
	return nil
}

const discoveryQuery = `query($owner: String!, $repo: String!, $login: String!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    databaseId
    pullRequests(first: 25, states: OPEN, after: $cursor) {
      nodes {
        number
        reviewRequests(first: 100) {
          nodes { requestedReviewer { ... on User { databaseId } } }
          pageInfo { hasNextPage }
        }
        reviews(first: 1, author: $login, states: [PENDING]) { nodes { id } }
      }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

func (c *Client) ListPullRequestsForReview(ctx context.Context, reviewerID int64) ([]PullRequest, error) {
	if reviewerID <= 0 || reviewerID != c.viewer.ID {
		return nil, errors.New("invalid GitHub reviewer identity")
	}
	repositories, err := c.refreshRepositories(ctx)
	if err != nil {
		return nil, err
	}
	var candidates []PullRequest
	for _, repository := range repositories {
		owner, repo, _ := strings.Cut(repository.FullName, "/")
		var cursor *string
		for {
			var result struct {
				Repository struct {
					DatabaseID   int64
					PullRequests connection[struct {
						Number         int64
						ReviewRequests connection[struct{ RequestedReviewer struct{ DatabaseID int64 } }]
						Reviews        connection[struct{ ID string }]
					}]
				}
			}
			if err := c.graphQL(ctx, discoveryQuery, map[string]any{
				"owner": owner, "repo": repo, "login": c.viewer.Username, "cursor": cursor,
			}, &result); err != nil {
				return nil, err
			}
			if result.Repository.DatabaseID != repository.ID {
				return nil, errors.New("GitHub repository identity changed")
			}
			pulls := result.Repository.PullRequests
			for _, pr := range pulls.Nodes {
				if pr.Number <= 0 || pr.ReviewRequests.PageInfo.HasNextPage {
					return nil, errors.New("incomplete GitHub pull request listing")
				}
				assigned := slices.ContainsFunc(pr.ReviewRequests.Nodes, func(node struct{ RequestedReviewer struct{ DatabaseID int64 } }) bool {
					return node.RequestedReviewer.DatabaseID == reviewerID
				})
				pending := len(pr.Reviews.Nodes) > 0
				if assigned || pending {
					candidates = append(candidates, PullRequest{RepositoryID: repository.ID, Number: pr.Number, HasPendingReview: pending})
				}
			}
			var previous string
			if cursor != nil {
				previous = *cursor
			}
			next, err := pulls.PageInfo.next(previous)
			if err != nil {
				return nil, err
			}
			if next == nil {
				break
			}
			cursor = next
		}
	}
	return candidates, nil
}

// GitHub encodes its GraphQL BigInt scalar as a decimal string.
type databaseID int64

func (id *databaseID) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	value, err := strconv.ParseInt(strings.Trim(string(data), `"`), 10, 64)
	if err != nil {
		return errors.New("invalid GitHub database ID")
	}
	*id = databaseID(value)
	return nil
}

type graphUser struct {
	DatabaseID int64
	Login      string
}

func (u graphUser) user() User {
	return User{ID: u.DatabaseID, Username: u.Login}
}

type graphComment struct {
	FullDatabaseID    databaseID
	Body              string
	CreatedAt         time.Time
	Author            graphUser
	PullRequestReview struct{ ID, State string }
}

type graphThread struct {
	ID                 string
	IsOutdated         bool
	IsResolved         bool
	ResolvedBy         graphUser
	ViewerCanResolve   bool
	ViewerCanUnresolve bool
	ViewerCanReply     bool
	Comments           connection[graphComment]
}

const commentFields = `fullDatabaseId body createdAt author { login ... on User { databaseId } }
  pullRequestReview { id state }`

const threadListQuery = `query($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    databaseId
    pullRequest(number: $number) {
      reviewThreads(first: 25, after: $cursor) {
        nodes {
          id isOutdated isResolved resolvedBy { login databaseId }
          viewerCanResolve viewerCanUnresolve viewerCanReply
          comments(first: 50) {
            nodes { ` + commentFields + ` }
            pageInfo { hasNextPage endCursor }
          }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

const threadCommentsQuery = `query($id: ID!, $cursor: String!) {
  node(id: $id) {
    ... on PullRequestReviewThread {
      comments(first: 50, after: $cursor) {
        nodes { ` + commentFields + ` }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

func (c *Client) ListPullRequestDiscussions(ctx context.Context, repositoryID, number int64) ([]Discussion, error) {
	repository, err := c.repository(repositoryID)
	if err != nil {
		return nil, err
	}
	owner, repo, _ := strings.Cut(repository.FullName, "/")
	var discussions []Discussion
	var cursor *string
	for {
		var result struct {
			Repository struct {
				DatabaseID  int64
				PullRequest *struct{ ReviewThreads connection[graphThread] }
			}
		}
		if err := c.graphQL(ctx, threadListQuery, map[string]any{
			"owner": owner, "repo": repo, "number": number, "cursor": cursor,
		}, &result); err != nil {
			return nil, err
		}
		if result.Repository.DatabaseID != repositoryID || result.Repository.PullRequest == nil {
			return nil, errors.New("GitHub review threads belong to an unexpected pull request")
		}
		threads := result.Repository.PullRequest.ReviewThreads
		for _, thread := range threads.Nodes {
			discussion, err := c.discussion(ctx, thread)
			if err != nil {
				return nil, err
			}
			discussions = append(discussions, discussion)
		}
		var previous string
		if cursor != nil {
			previous = *cursor
		}
		cursor, err = threads.PageInfo.next(previous)
		if err != nil || cursor == nil {
			return discussions, err
		}
	}
}

func (c *Client) discussion(ctx context.Context, thread graphThread) (Discussion, error) {
	discussion := Discussion{
		ID: thread.ID, Resolvable: true, Resolved: thread.IsResolved,
		ResolvedBy: thread.ResolvedBy.user(), ViewerCanReply: thread.ViewerCanReply,
		ViewerCanResolve: thread.ViewerCanResolve, ViewerCanUnresolve: thread.ViewerCanUnresolve,
	}
	switch {
	case thread.IsOutdated:
		discussion.ResolutionCause = ResolutionCauseOutdatedByPush
	case thread.IsResolved && thread.ResolvedBy.DatabaseID > 0:
		discussion.ResolutionCause = ResolutionCauseExplicit
	case thread.IsResolved:
		discussion.ResolutionCause = ResolutionCauseUnknown
	}
	comments := thread.Comments
	var previous string
	for {
		for _, comment := range comments.Nodes {
			discussion.Notes = append(discussion.Notes, Note{
				ID: int64(comment.FullDatabaseID), Body: comment.Body, Author: comment.Author.user(),
				CreatedAt: comment.CreatedAt, Resolvable: true, Resolved: thread.IsResolved,
				ReviewNodeID: comment.PullRequestReview.ID,
				Pending:      comment.PullRequestReview.State == "PENDING",
			})
		}
		cursor, err := comments.PageInfo.next(previous)
		if err != nil || cursor == nil {
			return discussion, err
		}
		previous = *cursor
		var result struct {
			Node *struct{ Comments connection[graphComment] }
		}
		if err := c.graphQL(ctx, threadCommentsQuery, map[string]any{"id": thread.ID, "cursor": cursor}, &result); err != nil {
			return Discussion{}, err
		}
		if result.Node == nil {
			return Discussion{}, ErrReviewInputChanged
		}
		comments = result.Node.Comments
	}
}

func (c *Client) AddReviewReply(ctx context.Context, threadID, reviewNodeID, body string) error {
	const mutation = `mutation($thread: ID!, $review: ID!, $body: String!) {
	  addPullRequestReviewThreadReply(input: {pullRequestReviewThreadId: $thread, pullRequestReviewId: $review, body: $body}) {
	    comment { id }
	  }
	}`
	return c.graphQL(ctx, mutation, map[string]any{"thread": threadID, "review": reviewNodeID, "body": body}, nil)
}

func (c *Client) SetReviewThreadResolved(ctx context.Context, threadID string, resolved bool) error {
	mutation := "unresolveReviewThread"
	if resolved {
		mutation = "resolveReviewThread"
	}
	query := "mutation($id: ID!) {" + mutation + "(input: {threadId: $id}) { thread { id } }}"
	return c.graphQL(ctx, query, map[string]any{"id": threadID}, nil)
}
