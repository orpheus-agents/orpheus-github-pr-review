package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/orpheus-agents/orpheus-github-pr-review/internal/config"
)

const Host = "github.com"
const apiOrigin = "https://api.github.com"
const jsonMediaType = "application/vnd.github+json"
const apiVersion = "2022-11-28"
const maxResponseBytes = 16 << 20

var ErrReviewInputChanged = errors.New("pull request changed while loading review input")
var ErrRepositoryOutsideScope = errors.New("repository is outside the configured GitHub scope")
var ErrRateLimited = errors.New("GitHub API rate limit reached")

type responseError struct {
	status int
	path   string
}

func (e *responseError) Error() string {
	return fmt.Sprintf("GitHub API %s returned HTTP %d", e.path, e.status)
}

func IsRetryable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrRateLimited) || errors.Is(err, ErrReviewInputChanged) {
		return true
	}
	if response, ok := errors.AsType[*responseError](err); ok {
		return response.status == http.StatusRequestTimeout || response.status >= http.StatusInternalServerError
	}
	_, ok := errors.AsType[net.Error](err)
	return ok
}

// Client uses one token for the control plane. The repository catalog and viewer
// are initialized by the watcher before it sends the first snapshot.
type Client struct {
	origin       string
	token        string
	names        []string
	http         *http.Client
	repositories []Repository
	viewer       User
	retryAt      atomic.Int64
}

func New(cfg config.Config) (*Client, error) {
	return &Client{
		origin: apiOrigin,
		token:  cfg.GitHub.Token,
		names:  slices.Clone(cfg.GitHub.Repositories),
		http: &http.Client{
			Timeout: cfg.HTTPTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *Client) CurrentUser(ctx context.Context) (User, error) {
	var viewer User
	if err := c.json(ctx, http.MethodGet, "/user", nil, &viewer); err != nil {
		return User{}, err
	}
	if viewer.ID <= 0 || strings.TrimSpace(viewer.Username) == "" {
		return User{}, errors.New("invalid authenticated GitHub user")
	}

	repositories := make([]Repository, 0, len(c.names))
	for _, name := range c.names {
		var item struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
			CloneURL string `json:"clone_url"`
			WebURL   string `json:"html_url"`
			Archived bool   `json:"archived"`
			Owner    struct {
				Type string `json:"type"`
			} `json:"owner"`
			Permissions struct {
				Push bool `json:"push"`
			} `json:"permissions"`
		}
		if err := c.json(ctx, http.MethodGet, "/repos/"+name, nil, &item); err != nil {
			return User{}, err
		}
		if item.ID <= 0 || !strings.EqualFold(item.FullName, name) || item.Archived || item.Owner.Type != "Organization" || !item.Permissions.Push {
			return User{}, fmt.Errorf("GitHub repository %s must be an active organization repository with write access for the reviewer", name)
		}
		if item.CloneURL != "https://github.com/"+item.FullName+".git" {
			return User{}, fmt.Errorf("unexpected HTTPS clone URL for GitHub repository %s", name)
		}
		repositories = append(repositories, Repository{
			ID: item.ID, FullName: item.FullName, CloneURL: item.CloneURL, WebURL: item.WebURL,
		})
	}
	c.viewer = viewer
	c.repositories = repositories
	return viewer, nil
}

func (c *Client) repository(id int64) (Repository, error) {
	for _, repo := range c.repositories {
		if repo.ID == id {
			return repo, nil
		}
	}
	return Repository{}, ErrRepositoryOutsideScope
}

func (c *Client) request(ctx context.Context, method, path, media string, payload any, limit int64) ([]byte, error) {
	if deadline := c.retryAt.Load(); time.Now().Unix() < deadline {
		return nil, ErrRateLimited
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.origin+path, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", media)
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("User-Agent", "orpheus-github-pr-review")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	readLimit := limit
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		readLimit = 4096
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, readLimit+1))
	if err != nil {
		return nil, err
	}
	switch {
	case response.StatusCode == http.StatusTooManyRequests,
		response.StatusCode == http.StatusForbidden && (response.Header.Get("X-RateLimit-Remaining") == "0" ||
			response.Header.Get("Retry-After") != "" || bytes.Contains(data, []byte("secondary rate limit"))):
		c.pause(response.Header)
		return nil, ErrRateLimited
	case response.StatusCode < 200 || response.StatusCode >= 300:
		return nil, &responseError{status: response.StatusCode, path: request.URL.Path}
	case int64(len(data)) > limit:
		return nil, errResponseTooLarge
	default:
		return data, nil
	}
}

var errResponseTooLarge = errors.New("GitHub response exceeds the allowed size")

func (c *Client) pause(headers http.Header) {
	now := time.Now()
	until := now.Add(time.Minute).Unix()
	if duration, err := strconv.ParseInt(headers.Get("Retry-After"), 10, 64); err == nil && duration > 0 {
		until = now.Unix() + duration
	} else if reset, err := strconv.ParseInt(headers.Get("X-RateLimit-Reset"), 10, 64); headers.Get("X-RateLimit-Remaining") == "0" && err == nil && reset > now.Unix() {
		until = reset
	}
	for {
		previous := c.retryAt.Load()
		if previous >= until || c.retryAt.CompareAndSwap(previous, until) {
			return
		}
	}
}

func (c *Client) json(ctx context.Context, method, path string, payload, result any) error {
	data, err := c.request(ctx, method, path, jsonMediaType, payload, maxResponseBytes)
	if err != nil || result == nil {
		return err
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("decode GitHub API response for %s: %w", path, err)
	}
	return nil
}

func listREST[T any](ctx context.Context, client *Client, path string) ([]T, error) {
	const pageSize = 100
	var all []T
	for page := 1; ; page++ {
		var items []T
		url := fmt.Sprintf("%s?per_page=%d&page=%d", path, pageSize, page)
		if err := client.json(ctx, http.MethodGet, url, nil, &items); err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < pageSize {
			return all, nil
		}
	}
}
