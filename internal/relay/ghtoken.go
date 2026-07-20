package relay

import (
	"context"
	"errors"
	"time"
)

// ErrNoGitHubApp is returned when a relay without App credentials is asked to
// broker a token.
var ErrNoGitHubApp = errors.New("relay has no github app configured")

// ErrRepoNotBound is returned when an agent asks for a token for a repository
// it does not deploy.
var ErrRepoNotBound = errors.New("repo not bound to this agent")

// GitHubTokenFor mints a repo-scoped installation token for agentName, after
// checking the full chain: the agent must have a binding for repo, and the
// account owning that agent must hold the installation the token comes from.
// Without the binding check a single compromised box could read every
// repository its account granted the App.
func (s *Store) GitHubTokenFor(ctx context.Context, app *GitHubApp, agentName, repo string) (string, time.Time, error) {
	// Binding check first — it is the authz boundary, and keeping it ahead of
	// the nil-App guard means a relay without App credentials still exercises
	// it (the control-op test would otherwise pass with the check deleted).
	bound, err := s.AgentBoundToRepo(agentName, repo)
	if err != nil {
		return "", time.Time{}, err
	}
	if !bound {
		return "", time.Time{}, ErrRepoNotBound
	}
	if app == nil {
		return "", time.Time{}, ErrNoGitHubApp
	}
	accountID, _, err := s.AgentAccount(agentName)
	if err != nil {
		return "", time.Time{}, err
	}
	installationID, err := s.InstallationForAccount(accountID)
	if err != nil {
		return "", time.Time{}, err
	}
	return app.RepoToken(ctx, installationID, repo)
}
