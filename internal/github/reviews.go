package github

import (
	"context"
	"fmt"
	"net/http"
)

func (c *Client) ListReviews(ctx context.Context, repositoryID, number int64) ([]Review, error) {
	repository, err := c.repository(repositoryID)
	if err != nil {
		return nil, err
	}
	return listREST[Review](ctx, c, pullPath(repository, number)+"/reviews")
}

func (c *Client) CreateReview(ctx context.Context, repositoryID, number int64, draft ReviewDraft) (Review, error) {
	repository, err := c.repository(repositoryID)
	if err != nil {
		return Review{}, err
	}
	var review Review
	err = c.json(ctx, http.MethodPost, pullPath(repository, number)+"/reviews", draft, &review)
	return review, err
}

func (c *Client) SubmitReview(ctx context.Context, repositoryID, number, reviewID int64, body string) error {
	repository, err := c.repository(repositoryID)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/reviews/%d/events", pullPath(repository, number), reviewID)
	return c.json(ctx, http.MethodPost, url, map[string]string{"event": "COMMENT", "body": body}, nil)
}

func (c *Client) DeletePendingReview(ctx context.Context, repositoryID, number, reviewID int64) error {
	repository, err := c.repository(repositoryID)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/reviews/%d", pullPath(repository, number), reviewID)
	return c.json(ctx, http.MethodDelete, url, nil, nil)
}
