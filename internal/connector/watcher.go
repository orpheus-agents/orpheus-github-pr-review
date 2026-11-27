package connector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/orpheus-agents/orpheus-github-pr-review/internal/github"
	"github.com/orpheus-agents/orpheus-github-pr-review/internal/review"

	"go.uber.org/zap"
)

type gitHubClient interface {
	CurrentUser(ctx context.Context) (github.User, error)
	ListPullRequestsForReview(ctx context.Context, reviewerID int64) ([]github.PullRequest, error)
	GetReviewInput(ctx context.Context, repositoryID, number int64) (github.ReviewInput, error)
}

type reviewInputSink interface {
	Submit(ctx context.Context, snapshot review.Snapshot) error
}

type WatcherConfig struct {
	PollInterval time.Duration
	GitHubHost   string
}

// Watcher periodically discovers pull requests assigned to the authenticated
// GitHub reviewer, fetches their control-plane inputs, and hands one complete
// poll snapshot to the reconciler. It does not make admission or Orpheus
// lifecycle decisions.
type Watcher struct {
	logger         *zap.Logger
	gitHubClient   gitHubClient
	inputSink      reviewInputSink
	pollInterval   time.Duration
	gitHubHost     string
	stopOnce       sync.Once
	stop           chan struct{}
	started        chan struct{}
	done           chan struct{}
	cancelMu       sync.Mutex
	cancelRequests context.CancelFunc
}

func NewWatcher(
	logger *zap.Logger,
	gitHubClient gitHubClient,
	inputSink reviewInputSink,
	cfg WatcherConfig,
) *Watcher {
	return &Watcher{
		logger:       logger,
		gitHubClient: gitHubClient,
		inputSink:    inputSink,
		pollInterval: cfg.PollInterval,
		gitHubHost:   cfg.GitHubHost,
		stop:         make(chan struct{}),
		started:      make(chan struct{}),
		done:         make(chan struct{}),
	}
}

func (w *Watcher) Run(ctx context.Context) error {
	requestCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	w.cancelMu.Lock()
	w.cancelRequests = cancelRequests
	w.cancelMu.Unlock()
	close(w.started)
	defer close(w.done)
	defer func() {
		cancelRequests()
		w.cancelMu.Lock()
		w.cancelRequests = nil
		w.cancelMu.Unlock()
	}()

	if w.stopping(ctx) {
		return nil
	}

	user, err := w.gitHubClient.CurrentUser(requestCtx)
	if err != nil {
		return fmt.Errorf("resolve authenticated GitHub reviewer: %w", err)
	}

	w.logger.Info("authenticated GitHub user",
		zap.Int64("github_user_id", user.ID),
		zap.String("github_username", user.Username),
	)

	if w.stopping(ctx) {
		return nil
	}
	w.poll(requestCtx, user)

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.stop:
			return nil
		case <-ticker.C:
			if w.stopping(ctx) {
				return nil
			}
			w.poll(requestCtx, user)
		}
	}
}

func (w *Watcher) Stop(ctx context.Context) error {
	w.stopOnce.Do(func() {
		w.logger.Debug("stopping pull request watcher")
		close(w.stop)
	})

	select {
	case <-w.started:
	case <-ctx.Done():
		w.logger.Debug("pull request watcher shutdown timed out before start",
			zap.Error(ctx.Err()),
		)
		return ctx.Err()
	}

	select {
	case <-w.done:
		w.logger.Debug("pull request watcher stopped")
		return nil
	case <-ctx.Done():
		w.logger.Debug("pull request watcher shutdown timed out; cancelling active requests",
			zap.Error(ctx.Err()),
		)
		w.cancelMu.Lock()
		cancelRequests := w.cancelRequests
		w.cancelMu.Unlock()
		if cancelRequests != nil {
			cancelRequests()
		}
		return ctx.Err()
	}
}

func (w *Watcher) stopping(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-w.stop:
		return true
	default:
		return false
	}
}

func (w *Watcher) poll(ctx context.Context, reviewer github.User) {
	pullRequests, err := w.gitHubClient.ListPullRequestsForReview(ctx, reviewer.ID)
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return
		}
		w.logger.Error("failed to poll GitHub pull requests", zap.Error(err))
		return
	}

	w.logger.Info("found GitHub pull requests assigned for review",
		zap.Int("pull_request_count", len(pullRequests)),
	)
	inputs := make([]review.Input, 0, len(pullRequests))
	for _, candidate := range pullRequests {
		gitHubInput, err := w.gitHubClient.GetReviewInput(ctx, candidate.RepositoryID, candidate.Number)
		if err != nil {
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return
			}
			w.logger.Error("failed to fetch GitHub review input",
				zap.Int64("repository_id", candidate.RepositoryID),
				zap.Int64("pull_request_number", candidate.Number),
				zap.Error(err),
			)
			return
		}

		gitHubInput.PullRequest.HasPendingReview = candidate.HasPendingReview
		input, err := review.NewInput(w.gitHubHost, reviewer, gitHubInput)
		if err != nil {
			w.logger.Error("failed to prepare GitHub review input",
				zap.Int64("repository_id", candidate.RepositoryID),
				zap.Int64("pull_request_number", candidate.Number),
				zap.Error(err),
			)
			return
		}
		inputs = append(inputs, input)
	}

	if err := w.inputSink.Submit(ctx, review.Snapshot{Reviewer: reviewer, Inputs: inputs}); err != nil {
		switch {
		case errors.Is(err, context.Canceled) && ctx.Err() != nil:
			return
		case errors.Is(err, ErrReconcilerStopping):
			w.logger.Debug("review reconciler stopped accepting snapshot")
		case errors.Is(err, ErrReconcileQueueFull):
			w.logger.Warn("review reconcile queue is full; deferring snapshot until the next poll",
				zap.Int("pull_request_count", len(inputs)),
			)
		default:
			w.logger.Error("failed to submit GitHub review snapshot", zap.Error(err))
		}
	}
}
