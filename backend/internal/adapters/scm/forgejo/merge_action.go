package forgejo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var mergeHeadSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)

var _ ports.SCMMerger = (*Provider)(nil)

// MergePullRequest performs a Forgejo squash merge guarded by the reviewed head
// SHA. Forgejo's head_commit_id field is a compare-and-swap precondition: if
// the live head has advanced, Forgejo responds 409 (ErrSHADoesNotMatch) and the
// merge is rejected. The merge endpoint returns an empty body on success, so
// the resulting merge commit SHA is recovered by re-fetching the pull request.
func (p *Provider) MergePullRequest(ctx context.Context, request ports.SCMMergeRequest) (ports.SCMMergeResult, error) {
	if p == nil || p.client == nil {
		return ports.SCMMergeResult{}, fmt.Errorf("forgejo scm: merge provider is not configured")
	}
	if request.PR.Number <= 0 {
		return ports.SCMMergeResult{}, fmt.Errorf("forgejo scm: invalid pull request number %d", request.PR.Number)
	}
	if strings.TrimSpace(request.PR.Repo.Owner) == "" || strings.TrimSpace(request.PR.Repo.Name) == "" {
		return ports.SCMMergeResult{}, fmt.Errorf("forgejo scm: invalid repository owner/name")
	}
	if request.Method != ports.SCMMergeSquash {
		return ports.SCMMergeResult{}, fmt.Errorf("forgejo scm: unsupported merge method %q", request.Method)
	}
	expectedHead := strings.TrimSpace(request.ExpectedHeadSHA)
	if !mergeHeadSHAPattern.MatchString(expectedHead) {
		return ports.SCMMergeResult{}, fmt.Errorf("forgejo scm: invalid expected head sha")
	}

	client, err := p.clientForRepoErr(request.PR.Repo)
	if err != nil {
		return ports.SCMMergeResult{}, err
	}

	path := repoPath(request.PR.Repo.Owner, request.PR.Repo.Name, "pulls", strconv.Itoa(request.PR.Number), "merge")
	q := url.Values{}
	_ = q // query unused; the CAS guard is a JSON body field
	payload := struct {
		Do           string `json:"do"`
		HeadCommitID string `json:"head_commit_id,omitempty"`
	}{Do: string(request.Method), HeadCommitID: expectedHead}

	resp, err := client.doREST(ctx, http.MethodPost, path, payload)
	if err != nil {
		switch resp.StatusCode {
		case http.StatusNotFound:
			return ports.SCMMergeResult{}, fmt.Errorf("%w: %w", ports.ErrSCMNotFound, err)
		case http.StatusConflict:
			// 409 covers both a head-advanced (ErrSHADoesNotMatch) and a
			// merge-conflict. Both mean "do not merge the head you saw".
			return ports.SCMMergeResult{}, fmt.Errorf("%w: %w", ports.ErrSCMHeadChanged, err)
		case http.StatusMethodNotAllowed, http.StatusLocked:
			return ports.SCMMergeResult{}, fmt.Errorf("%w: %w", ports.ErrSCMNotMergeable, err)
		default:
			return ports.SCMMergeResult{}, err
		}
	}

	// Forgejo's merge endpoint returns an empty body on success; re-fetch the
	// PR to recover the merge_commit_sha.
	mergeResp, err := client.doREST(ctx, http.MethodGet, repoPath(request.PR.Repo.Owner, request.PR.Repo.Name, "pulls", strconv.Itoa(request.PR.Number)), nil)
	if err != nil {
		return ports.SCMMergeResult{}, fmt.Errorf("forgejo scm: fetch merged PR: %w", err)
	}
	var pr struct {
		MergedCommitID *string `json:"merge_commit_sha"`
	}
	if err := json.Unmarshal(mergeResp.Body, &pr); err != nil {
		return ports.SCMMergeResult{}, fmt.Errorf("forgejo scm: decode merged PR: %w", err)
	}
	if pr.MergedCommitID == nil || *pr.MergedCommitID == "" {
		return ports.SCMMergeResult{}, fmt.Errorf("forgejo scm: merge response missing commit sha")
	}
	return ports.SCMMergeResult{MergeCommitSHA: *pr.MergedCommitID}, nil
}
