package forgejo

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var _ ports.SCMReviewRequester = (*Provider)(nil)
var _ ports.SCMReviewResolver = (*Provider)(nil)

// RequestReview asks Forgejo to request another review from the supplied user.
// Unlike GitLab, Forgejo's endpoint takes usernames directly (no numeric id
// lookup), so the reviewer is forwarded as-is.
func (p *Provider) RequestReview(ctx context.Context, request ports.SCMReviewRequest) error {
	if p == nil || p.client == nil {
		return fmt.Errorf("forgejo scm: review requester is not configured")
	}
	if request.PR.Number <= 0 || strings.TrimSpace(request.PR.Repo.Owner) == "" || strings.TrimSpace(request.PR.Repo.Name) == "" {
		return fmt.Errorf("forgejo scm: invalid pull request reference")
	}
	reviewer := strings.TrimSpace(strings.TrimPrefix(request.Reviewer, "@"))
	if reviewer == "" {
		return fmt.Errorf("forgejo scm: reviewer is required")
	}

	client, err := p.clientForRepoErr(request.PR.Repo)
	if err != nil {
		return err
	}
	payload := struct {
		Reviewers []string `json:"reviewers"`
	}{Reviewers: []string{reviewer}}
	resp, err := client.doREST(ctx, http.MethodPost,
		repoPath(request.PR.Repo.Owner, request.PR.Repo.Name, "pulls", strconv.Itoa(request.PR.Number), "requested_reviewers"),
		payload)
	if err != nil {
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %w", ports.ErrSCMNotFound, err)
		}
		return err
	}
	return nil
}

// ResolveReviewThread is not supported by the Forgejo API: there is no endpoint
// to resolve a review thread/file comment. The review service maps this to a
// clean ErrInvalid so the UI degrades gracefully instead of erroring.
func (p *Provider) ResolveReviewThread(ctx context.Context, request ports.SCMReviewResolveRequest) error {
	return fmt.Errorf("%w: forgejo does not expose a review-thread resolve endpoint", ports.ErrSCMUnsupported)
}
