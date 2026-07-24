package github

import (
	"context"

	"github.com/piperbox/piper/internal/source"
)

// RelayTokens is the brokered TokenSource: the box holds no GitHub App key, so
// every token comes from the relay, already scoped to the one repository.
// Ask is the tunnel client's gh-token control op.
type RelayTokens struct {
	Ask func(repo string) (string, error)
}

func (r RelayTokens) Token(_ context.Context, ev source.Event) (string, error) {
	return r.Ask(ev.Repo)
}
