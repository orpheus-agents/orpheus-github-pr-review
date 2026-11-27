package workflow

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/orpheus-agents/orpheus-github-pr-review/internal/orpheus"
)

const (
	helperRoot        = ".orpheus/github-review/code"
	beforeRunTemplate = `#!/bin/sh
set -eu
umask 077

for command_name in git gh python3 base64 gzip sha256sum; do
  command -v "$command_name" >/dev/null 2>&1 || {
    echo "before_run: required command is unavailable: $command_name" >&2
    exit 2
  }
done

if [ ! -d .git ]; then
  git clone --no-checkout -- "$ORPHEUS_GITHUB_CLONE_URL" . 1>&2
else
  test "$(git remote get-url origin)" = "$ORPHEUS_GITHUB_CLONE_URL" || {
    echo "before_run: existing checkout origin does not match contract" >&2
    exit 2
  }
fi

git fetch --no-tags origin "+refs/pull/$ORPHEUS_GITHUB_PR_NUMBER/head:refs/remotes/origin/orpheus-pr-$ORPHEUS_GITHUB_PR_NUMBER" 1>&2
test "$(git rev-parse "refs/remotes/origin/orpheus-pr-$ORPHEUS_GITHUB_PR_NUMBER^{commit}")" = "$ORPHEUS_GITHUB_DIFF_HEAD_SHA" || {
  echo "before_run: pull request ref does not match pinned head SHA" >&2
  exit 2
}

git cat-file -e "$ORPHEUS_GITHUB_DIFF_BASE_SHA^{commit}"
git cat-file -e "$ORPHEUS_GITHUB_DIFF_HEAD_SHA^{commit}"
git checkout --detach "$ORPHEUS_GITHUB_DIFF_HEAD_SHA" 1>&2

helper_path='%s'
helper_dir=${helper_path%%/review-pack}
mkdir -p "$helper_dir" "$ORPHEUS_GITHUB_REVIEW_DIR/findings" "$ORPHEUS_GITHUB_REVIEW_DIR/confirmed" "$ORPHEUS_GITHUB_REVIEW_DIR/rejected" "$ORPHEUS_GITHUB_REVIEW_DIR/recommendations" "$ORPHEUS_GITHUB_REVIEW_DIR/resolutions" "$ORPHEUS_GITHUB_REVIEW_DIR/tmp"
if [ ! -f "$helper_path" ]; then
  helper_tmp="$helper_path.tmp.$$"
  trap 'rm -f "$helper_tmp"' EXIT HUP INT TERM
  printf '%%s' '%s' | base64 -d | gzip -d >"$helper_tmp"
  test "$(sha256sum "$helper_tmp" | cut -d ' ' -f 1)" = '%s' || {
    echo "before_run: installed helper digest mismatch" >&2
    exit 2
  }
  chmod 0500 "$helper_tmp"
  mv "$helper_tmp" "$helper_path"
  trap - EXIT HUP INT TERM
fi
test "$(sha256sum "$helper_path" | cut -d ' ' -f 1)" = '%s' || {
  echo "before_run: helper digest mismatch" >&2
  exit 2
}


diff_tmp="$ORPHEUS_GITHUB_REVIEW_DIR/diff.patch.tmp.$$"
trap 'rm -f "$diff_tmp"' EXIT HUP INT TERM
gh api --hostname github.com -H 'Accept: application/vnd.github.diff' -H 'X-GitHub-Api-Version: 2022-11-28' \
  "repos/$ORPHEUS_GITHUB_REPOSITORY/pulls/$ORPHEUS_GITHUB_PR_NUMBER" >"$diff_tmp"
test "$(sha256sum "$diff_tmp" | cut -d ' ' -f 1)" = "$ORPHEUS_GITHUB_DIFF_DIGEST" || {
  echo "before_run: GitHub diff does not match the pinned digest" >&2
  exit 2
}
mv "$diff_tmp" "$ORPHEUS_GITHUB_REVIEW_DIR/diff.patch"
trap - EXIT HUP INT TERM

contract_tmp="$ORPHEUS_GITHUB_REVIEW_DIR/contract.json.tmp.$$"
trap 'rm -f "$contract_tmp"' EXIT HUP INT TERM
printf '%%s' '%s' | base64 -d >"$contract_tmp"
mv "$contract_tmp" "$ORPHEUS_GITHUB_REVIEW_DIR/contract.json"
trap - EXIT HUP INT TERM
`
	afterRunTemplate = `#!/bin/sh
set -eu
helper_path='%s'
test -f "$helper_path" || {
  echo "after_run: review helper is missing" >&2
  exit 2
}
test "$(sha256sum "$helper_path" | cut -d ' ' -f 1)" = '%s' || {
  echo "after_run: review helper digest mismatch" >&2
  exit 2
}
exec python3 "$helper_path" "$ORPHEUS_GITHUB_REVIEW_DIR"
`
)

type renderedHooks struct {
	Hooks        orpheus.HooksInput
	Environment  map[string]string
	HelperSHA256 string
}

func renderHooks(metadata MetadataV1, cloneURL string, pullRequestNumber int64, timeoutSeconds int) (renderedHooks, error) {
	contractJSON, err := json.Marshal(metadata)
	if err != nil {
		return renderedHooks{}, fmt.Errorf("encode hook contract: %w", err)
	}
	helperDigest := sha256.Sum256([]byte(helperAsset))
	helperHex := hex.EncodeToString(helperDigest[:])
	helperSHA256 := "sha256:" + helperHex
	if metadata.Protocol.HelperSHA256 != helperSHA256 {
		return renderedHooks{}, fmt.Errorf("render hooks: helper digest does not match metadata")
	}
	helperPayload, err := compressedBase64(helperAsset)
	if err != nil {
		return renderedHooks{}, fmt.Errorf("compress review helper: %w", err)
	}

	helperPath := helperRoot + "/" + helperHex + "/review-pack"
	beforeRun := fmt.Sprintf(
		beforeRunTemplate,
		helperPath,
		helperPayload,
		helperHex,
		helperHex,
		base64.StdEncoding.EncodeToString(contractJSON),
	)
	afterRun := fmt.Sprintf(afterRunTemplate, helperPath, helperHex)

	return renderedHooks{
		Hooks: orpheus.HooksInput{
			BeforeRun:      new(beforeRun),
			AfterRun:       new(afterRun),
			TimeoutSeconds: new(timeoutSeconds),
		},
		Environment: map[string]string{
			"ORPHEUS_GITHUB_CLONE_URL":     cloneURL,
			"ORPHEUS_GITHUB_REPOSITORY":    metadata.GitHub.RepositoryPath,
			"ORPHEUS_GITHUB_PR_NUMBER":     fmt.Sprintf("%d", pullRequestNumber),
			"ORPHEUS_GITHUB_DIFF_BASE_SHA": metadata.DiffRefs.BaseSHA,
			"ORPHEUS_GITHUB_DIFF_DIGEST":   metadata.DiffRefs.Digest,
			"ORPHEUS_GITHUB_DIFF_HEAD_SHA": metadata.DiffRefs.HeadSHA,
			"ORPHEUS_GITHUB_REVIEW_DIR":    metadata.Review.ArtifactsPath,
		},
		HelperSHA256: helperSHA256,
	}, nil
}

func helperDigest() string {
	digest := sha256.Sum256([]byte(helperAsset))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func compressedBase64(value string) (string, error) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(value)); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(compressed.Bytes()), nil
}
