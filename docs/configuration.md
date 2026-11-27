# Configure GitHub PR review

Use a dedicated GitHub user in the organization that owns the reviewed repositories. Give the user
write access to those repositories so GitHub can request a review from it. Create a fine-grained
personal access token limited to those repositories with **Pull requests: Read and write**.

Set the connector environment:

| Setting | Value |
| --- | --- |
| `GITHUB_TOKEN` | The reviewer user's token |
| `GITHUB_REPOSITORIES` | Comma-separated `owner/repo` names for one organization on github.com |
| `ORPHEUS_BASE_URL` | Orpheus API base URL |
| `ORPHEUS_API_KEY` | Orpheus API key |
| `ORPHEUS_WEB_BASE_URL` | Public Orpheus URL for session links. Defaults to the API base URL. |
| `WORKFLOWS_DIR` | Directory of Markdown workflows. Defaults to `workflows`. |
| `POLL_INTERVAL_SECONDS` | Poll interval. Defaults to 30 seconds. |
| `MAX_CONCURRENT_REVIEWS` | Maximum number of active reviews. Defaults to 4. |

Run one connector process per Orpheus instance, GitHub organization and reviewer identity.
Use Orpheus v0.18.0 or later. The connector can run as a binary or as the
`retailcrm/orpheus-github-pr-review` image. Mount the workflow directory read-only.

## Sandbox

Configure the selected Orpheus template with Git, GitHub CLI, Python 3, base64, gzip and sha256sum.
Give the sandbox a separate fine-grained token with **Contents: Read-only** and **Pull requests:
Read-only** for the target repositories. Configure the Git HTTPS credential helper and `gh` to
use that credential. The connector reads the diff from GitHub and checks that the sandbox sees
the same bytes and head SHA. Fork pull requests use the target repository's
`refs/pull/<number>/head` ref.

## Workflows

A workflow is a Markdown file with YAML front matter. The body contains the agent's review
instructions. Use [the default example](../examples/workflows/default.md) to review all configured
repositories. Set `repository_ids` to direct a workflow to specific numeric GitHub repository IDs.
Explicit routes take priority over the one optional default workflow.

| Field | Purpose |
| --- | --- |
| `id` | Stable workflow identifier |
| `profile`, `model` | Orpheus profile and optional model override |
| `sandbox_template` | Orpheus sandbox template |
| `services` | Explicit sandbox service codes, such as `[github]` |
| `language` | Optional language for agent-authored reviews, such as `en` |
| `notes.success`, `notes.fail` | Optional Markdown templates for visible outcomes |

The success template can include `{{ .FindingsCount }}`. In the failure template,
`{{ .Reason }}` gives the public Orpheus error or a safe description of the connector failure.
`{{ .SessionURL }}` links to the session when available. The connector keeps the chosen templates
with the session so restarts publish the same outcome.

## Request a review

Choose the technical user under **Reviewers** on an open pull request. You can also use
`gh pr edit PR_NUMBER --repo OWNER/REPO --add-reviewer BOT_LOGIN`. The connector sees the
personal request on the next poll and starts a single-run review when it has capacity.

The result appears as a GitHub review with comments and optional inline findings. GitHub clears
the request for the technical user when the review is submitted. Request review again to start a
new run. A change to the pinned diff while the run is active cancels that run and prompts a
new request.
