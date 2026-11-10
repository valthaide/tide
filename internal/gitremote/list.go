package gitremote

import (
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/valthaide/tide/internal/gitrev"
	"github.com/valthaide/tide/internal/repoauth"
)

// List is git ls-remote. Peeled tag commits are returned as refs/tags/<name>^{}.
func List(repoURL string, auth transport.AuthMethod) ([]gitrev.Ref, error) {
	repoURL = repoauth.CleanHTTPSURL(repoURL)
	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin",
		URLs: []string{repoURL},
	})
	listed, err := remote.List(&git.ListOptions{
		Auth:          auth,
		PeelingOption: git.AppendPeeled,
	})
	if err != nil {
		return nil, err
	}
	out := make([]gitrev.Ref, 0, len(listed))
	for _, ref := range listed {
		if ref == nil {
			continue
		}
		out = append(out, gitrev.Ref{Name: ref.Name().String(), Hash: ref.Hash().String()})
	}
	return out, nil
}
