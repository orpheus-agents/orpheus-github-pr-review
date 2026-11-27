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
	"sync"
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
var ErrRepositoryOutsideScope = errors.New("repository is outside the current GitHub review scope")
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

// The watcher sets the viewer at startup and refreshes the repository catalog
// on each poll. The reconciler can read the catalog concurrently.
type Client struct {
	origin       string
	token        string
	names        []string
	http         *http.Client
	repositoryMu sync.RWMutex
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
	c.viewer = viewer
	return viewer, nil
}

type repositoryResponse struct {
	ID          int64  `json:"id"`
	FullName    string `json:"full_name"`
	CloneURL    string `json:"clone_url"`
	WebURL      string `json:"html_url"`
	Archived    bool   `json:"archived"`
	Disabled    bool   `json:"disabled"`
	Permissions struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}

func (c *Client) refreshRepositories(ctx context.Context) ([]Repository, error) {
	items, err := listREST[repositoryResponse](ctx, c, "/user/repos")
	if err != nil {
		return nil, err
	}
	repositories := make([]Repository, 0, len(items))
	seen := make(map[int64]bool, len(items))
	for _, item := range items {
		if item.Archived || item.Disabled || !item.Permissions.Push || seen[item.ID] ||
			(len(c.names) > 0 && !slices.Contains(c.names, strings.ToLower(item.FullName))) {
			continue
		}
		owner, name, qualified := strings.Cut(item.FullName, "/")
		if item.ID <= 0 || !qualified || owner == "" || name == "" || strings.Contains(name, "/") ||
			item.CloneURL != "https://github.com/"+item.FullName+".git" {
			return nil, errors.New("invalid GitHub review repository identity")
		}
		seen[item.ID] = true
		repositories = append(repositories, Repository{
			ID: item.ID, FullName: item.FullName, CloneURL: item.CloneURL, WebURL: item.WebURL,
		})
	}
	c.repositoryMu.Lock()
	c.repositories = repositories
	c.repositoryMu.Unlock()
	return repositories, nil
}

func (c *Client) repository(id int64) (Repository, error) {
	c.repositoryMu.RLock()
	defer c.repositoryMu.RUnlock()
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
	defer func() { _ = response.Body.Close() }()
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
