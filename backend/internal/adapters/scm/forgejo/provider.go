package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ProviderOptions configures the Forgejo SCM provider.
type ProviderOptions struct {
	Client             *Client
	HTTPClient         *http.Client
	Token              TokenSource
	SkipTokenPreflight bool
	UserAgent          string
	Logger             *slog.Logger

	// AllowedHosts is the list of self-hosted Forgejo hosts the provider is
	// permitted to talk to. Every host must appear here (there is no default
	// public Forgejo host); a host not in this list is rejected before any
	// credential is attached — preventing a remote like forgejo.attacker.example
	// from receiving the configured bearer token. Each entry may include a port
	// (e.g. "forgejo.internal:3000").
	AllowedHosts []string

	// HostTokens maps a host to a token override. Hosts in AllowedHosts without
	// an explicit entry fall back to the default Token (typically
	// AO_FORGEJO_TOKEN / FORGEJO_TOKEN). The per-host selection ensures one
	// instance's token is not attached to another.
	HostTokens map[string]TokenSource
}

// Provider implements the provider-neutral scm.Provider interface for Forgejo.
// It supports multiple self-hosted instances by maintaining a per-host Client
// map. Each host's API base is <scheme>://<host>/api/v1, where the scheme
// comes from the git remote (http for local/plain-HTTP instances, https
// otherwise).
//
// Hosts are restricted to the explicit allowlist
// (ProviderOptions.AllowedHosts). A host that is not in the allowlist is
// rejected before any credential is attached — no HTTP request is made to
// non-allowlisted hosts.
type Provider struct {
	client *Client // default client (carries the default token; hosts use lazily-created clients)
	logger *slog.Logger

	// allowedHosts is the set of self-hosted hosts the provider may talk to.
	allowedHosts map[string]bool

	// hostTokens maps a host to its TokenSource. Hosts without an entry fall
	// back to the default token (opts.Token).
	hostTokens map[string]TokenSource

	// Per-host clients for self-hosted Forgejo instances. Lazily populated.
	hostClients   map[string]*Client
	hostClientCfg ClientOptions
	hostMu        sync.Mutex

	// hostIdentities caches the authenticated account per-host so each host
	// is resolved at most once for the provider's lifetime. The key is the
	// normalized host string.
	hostIdentities map[string]ports.SCMIdentity
	identityMu     sync.Mutex
}

// NewProvider creates a Forgejo SCM provider. If SkipTokenPreflight is false
// and a token source is supplied, the token is resolved immediately to fail
// fast on misconfiguration.
func NewProvider(opts ProviderOptions) (*Provider, error) {
	if opts.Client == nil && opts.Token != nil && !opts.SkipTokenPreflight {
		_, err := opts.Token.Token(context.Background())
		if err != nil {
			return nil, err
		}
	}
	userAgent := opts.UserAgent
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	c := opts.Client
	if c == nil {
		// Always create a default client so the default token source is carried
		// even when every real host uses a lazily-created per-host client.
		// SCMCredentialsAvailable consults p.client.tokens for the default token.
		c = NewClient(ClientOptions{
			HTTPClient: opts.HTTPClient,
			Token:      opts.Token,
			UserAgent:  userAgent,
		})
	}

	allowed := make(map[string]bool, len(opts.AllowedHosts))
	for _, h := range opts.AllowedHosts {
		h = NormalizeHost(h)
		if h != "" {
			allowed[h] = true
		}
	}

	hostTokens := make(map[string]TokenSource, len(opts.HostTokens))
	for h, src := range opts.HostTokens {
		h = NormalizeHost(h)
		if h != "" && src != nil {
			hostTokens[h] = src
		}
	}

	return &Provider{
		client:       c,
		logger:       logger,
		allowedHosts: allowed,
		hostTokens:   hostTokens,
		hostClientCfg: ClientOptions{
			HTTPClient: opts.HTTPClient,
			Token:      opts.Token,
			UserAgent:  userAgent,
		},
		hostClients: map[string]*Client{},
	}, nil
}

// isHostAllowed reports whether host is a Forgejo host the provider is
// permitted to talk to. Unlike GitLab there is no default public host: every
// host must be in the configured allowlist.
func (p *Provider) isHostAllowed(host string) bool {
	return p.allowedHosts[NormalizeHost(host)]
}

// ErrHostNotAllowed is returned when a remote's host is not in the configured
// allowlist. The provider rejects such hosts before attaching any credential.
var ErrHostNotAllowed = fmt.Errorf("forgejo scm: host not in allowlist")

// defaultSchemeForRemote returns the API scheme for a remote whose scheme is
// not set. Forgejo instances default to HTTPS.
func defaultSchemeForRemote(scheme string) string {
	s := strings.ToLower(strings.TrimSpace(scheme))
	if s == "http" || s == "https" {
		return s
	}
	return "https"
}

// clientForRepoErr returns the client for a repo's host, or an error if the
// host is not allowed. This is the guarded variant of clientForHost for use in
// methods that need to return an error rather than panic on a nil client.
func (p *Provider) clientForRepoErr(repo ports.SCMRepo) (*Client, error) {
	c := p.clientForHost(repo.Host, repo.Scheme)
	if c == nil {
		return nil, fmt.Errorf("forgejo scm: host %q not in allowlist: %w", repo.Host, ErrHostNotAllowed)
	}
	return c, nil
}

// clientForHost returns the client whose API base matches the given host and
// scheme. Self-hosted hosts must be in the allowlist; a non-allowlisted host
// is rejected — nil is returned so callers fail closed before attaching any
// credential. The scheme (from the git remote) selects the API base scheme so
// plain-HTTP instances are addressed correctly.
func (p *Provider) clientForHost(host, scheme string) *Client {
	h := NormalizeHost(host)
	// Reject non-allowlisted hosts before any credential is attached.
	if !p.allowedHosts[h] {
		return nil
	}
	// If the default client's API base already matches this host (e.g. a test
	// server whose host happens to be in the allowlist), reuse it.
	if p.client != nil && apiBaseMatches(p.client.apiBaseURL(), h, scheme) {
		return p.client
	}

	p.hostMu.Lock()
	defer p.hostMu.Unlock()
	clientKey := h + "|" + defaultSchemeForRemote(scheme)
	if c, ok := p.hostClients[clientKey]; ok {
		return c
	}

	cfg := p.hostClientCfg
	// Derive the API base from the host, preserving any port (e.g.
	// "forgejo.internal:3000" → "<scheme>://forgejo.internal:3000/api/v1").
	cfg.APIBase = defaultSchemeForRemote(scheme) + "://" + h + "/api/v1"
	// Select the per-host token if one is configured; otherwise the default
	// token (cfg.Token) applies.
	if src, ok := p.hostTokens[h]; ok {
		cfg.Token = src
	}
	c := NewClient(cfg)
	p.hostClients[clientKey] = c
	return c
}

// apiBaseMatches reports whether host (+ optional port) is the hostname of
// apiBase and, when scheme is non-empty, the URL scheme matches too.
func apiBaseMatches(apiBase, host, scheme string) bool {
	if apiBase == "" {
		return false
	}
	u, err := url.Parse(apiBase)
	if err != nil {
		return false
	}
	// host may carry a port (e.g. "127.0.0.1:3000"); match the full Host or
	// just the hostname so both forms are accepted.
	if !strings.EqualFold(u.Host, host) && !strings.EqualFold(u.Hostname(), host) {
		return false
	}
	if scheme != "" && !strings.EqualFold(u.Scheme, scheme) {
		return false
	}
	return true
}

// AuthenticatedIdentity returns the account associated with the active Forgejo
// token for the default host. Forgejo has no typed Bot flag, so
// human/bot classification reuses the isBotAuthor heuristic.
func (p *Provider) AuthenticatedIdentity(ctx context.Context) (ports.SCMIdentity, error) {
	return p.AuthenticatedIdentityForHost(ctx, "", "")
}

// AuthenticatedIdentityForHost resolves the account associated with the
// Forgejo token for the given host and API scheme. A host that is not in the
// allowlist returns an error. The scheme (from the git remote, "http" for
// plain-HTTP instances) selects the API base scheme, so a plain-HTTP instance
// is probed with http:// instead of the https default — a wrong scheme
// surfaces as "server gave HTTP response to HTTPS client" and would
// otherwise break author attribution and spam the observer log every tick.
// Successful results are cached per host+scheme for the provider's lifetime —
// a second call for the same host+scheme does not hit the API.
func (p *Provider) AuthenticatedIdentityForHost(ctx context.Context, host, scheme string) (ports.SCMIdentity, error) {
	key := NormalizeHost(host) + "|" + defaultSchemeForRemote(scheme)
	p.identityMu.Lock()
	defer p.identityMu.Unlock()
	if p.hostIdentities == nil {
		p.hostIdentities = make(map[string]ports.SCMIdentity)
	}
	if ident, ok := p.hostIdentities[key]; ok {
		return ident, nil
	}

	client := p.clientForHost(host, scheme)
	if client == nil {
		return ports.SCMIdentity{}, fmt.Errorf("forgejo scm: host %q not in allowlist: %w", host, ErrHostNotAllowed)
	}

	resp, err := client.doREST(ctx, http.MethodGet, "/user", nil)
	if err != nil {
		return ports.SCMIdentity{}, err
	}

	var user struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(resp.Body, &user); err != nil {
		return ports.SCMIdentity{}, fmt.Errorf("forgejo scm: decode authenticated user: %w", err)
	}

	login := strings.TrimSpace(user.Login)
	ident := ports.SCMIdentity{
		Login: login,
		Human: !isBotAuthor(login),
	}
	p.hostIdentities[key] = ident
	return ident, nil
}

// SCMCredentialsAvailable reports whether usable Forgejo credentials exist.
// It checks the default token source first, then falls back to per-host token
// sources (AO_FORGEJO_HOST_TOKENS). If any token source (default or
// host-specific) is usable, it returns true.
func (p *Provider) SCMCredentialsAvailable(ctx context.Context) (bool, error) {
	// Check the default token source first.
	if p.client != nil && p.client.tokens != nil {
		_, err := p.client.tokens.Token(ctx)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, ErrNoToken) {
			return false, err
		}
	}

	// Default token is not available — check per-host tokens.
	for _, src := range p.hostTokens {
		if src == nil {
			continue
		}
		_, err := src.Token(ctx)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, ErrNoToken) {
			return false, err
		}
	}

	return false, nil
}
