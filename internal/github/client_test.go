package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

const testDiff = `diff --git a/old name.go b/new name.go
similarity index 75%
rename from old name.go
rename to new name.go
index 7898192..6178079 100644
--- a/old name.go
+++ b/new name.go
@@ -1,2 +1,2 @@
 keep
-old
+new
`

func TestPinnedDiffRequiresAllFilesAndLines(t *testing.T) {
	t.Parallel()

	diff, err := parseDiff([]byte(testDiff), 1, 1, 1)
	require.NoError(t, err)
	require.Len(t, diff.Digest, 64)
	require.Equal(t, "old name.go", diff.Files[0].OldName)
	require.Equal(t, "new name.go", diff.Files[0].NewName)
	for _, test := range []struct {
		body                 string
		files                int
		additions, deletions int64
	}{
		{testDiff, 2, 1, 1},
		{testDiff, 1, 2, 1},
		{strings.TrimSuffix(testDiff, "+new\n"), 1, 1, 1},
		{"GitHub error\n" + testDiff, 1, 1, 1},
	} {
		_, err := parseDiff([]byte(test.body), test.files, test.additions, test.deletions)
		require.ErrorIs(t, err, errInvalidDiff)
	}
}

func TestGetReviewInputBindsTimelineRequestToDiff(t *testing.T) {
	t.Parallel()

	const repo = "/repos/org/repo"
	const pulls = repo + "/pulls/23"
	viewer := User{ID: 17, Username: "reviewer"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+pulls, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == diffMediaType {
			_, _ = w.Write([]byte(testDiff))
			return
		}
		_, _ = w.Write([]byte(`{
		  "id":246,"number":23,"state":"open","draft":true,"body":"PR context","html_url":"https://github.com/org/repo/pull/23",
		  "base":{"ref":"main","sha":"base","repo":{"id":12}},"head":{"ref":"fork","sha":"head","repo":null},
		  "requested_reviewers":[{"id":17,"login":"reviewer"}],"changed_files":1,"additions":1,"deletions":1
		}`))
	})
	mux.HandleFunc("GET "+repo+"/issues/23/timeline", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
		  {"id":99,"event":"review_requested","requested_reviewer":{"id":17}},
		  {"id":100,"event":"review_request_removed","requested_reviewer":{"id":17}},
		  {"id":101,"event":"review_requested","requested_reviewer":{"id":17}},
		  {"id":102,"event":"review_requested","requested_reviewer":{"id":19}},
		  {"id":103,"event":"review_requested","requested_team":{"id":17}}
		]`))
	})
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"repository":{"databaseId":12,"pullRequest":{"reviewThreads":{"nodes":[]}}}}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := &Client{http: server.Client(), origin: server.URL, token: "test", viewer: viewer,
		repositories: []Repository{{ID: 12, FullName: "org/repo", CloneURL: "https://github.com/org/repo.git"}}}
	input, err := client.GetReviewInput(t.Context(), 12, 23)
	require.NoError(t, err)
	require.EqualValues(t, 101, input.PullRequest.AssignmentID)
	require.True(t, input.PullRequest.Draft)
	require.Equal(t, "head", input.PullRequest.DiffRefs.HeadSHA)
	require.Len(t, input.PullRequest.DiffRefs.Digest, 64)
	_, err = client.GetReviewInput(t.Context(), 44, 23)
	require.ErrorIs(t, err, ErrRepositoryOutsideScope)
}

func TestSecondaryRateLimitPausesBothReadAndWrite(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	client := &Client{http: server.Client(), origin: server.URL}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		err := client.json(t.Context(), method, "/user", nil, nil)
		require.ErrorIs(t, err, ErrRateLimited)
		require.True(t, IsRetryable(err))
	}
	require.EqualValues(t, 1, calls.Load())
}

func TestSubmitReviewUsesNativeCommentReview(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/org/repo/pulls/23/reviews/11/events" {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["event"] != "COMMENT" || body["body"] != "finished" {
			http.Error(w, "invalid review payload", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client := &Client{http: server.Client(), origin: server.URL,
		repositories: []Repository{{ID: 12, FullName: "org/repo"}}}
	require.NoError(t, client.SubmitReview(context.Background(), 12, 23, 11, "finished"))
}
