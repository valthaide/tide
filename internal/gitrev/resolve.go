package gitrev

import (
	"fmt"
	"strings"
)

// Kind is how a fleet revision is resolved.
type Kind string

const (
	KindBranch Kind = "branch"
	KindTag    Kind = "tag"
	KindSHA    Kind = "sha"
)

// Ref is one line from git ls-remote.
type Ref struct {
	Name string
	Hash string
}

// Target is the commit a revision should deploy.
type Target struct {
	Kind Kind
	Name string
	SHA  string
}

// IsFullSHA reports a full 40-character git SHA-1.
func IsFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// Resolve picks a branch, tag, or pinned SHA.
// An empty revision tracks branch. A full SHA is pinned and does not use refs.
// Otherwise a tag of that name wins over a branch of the same name.
func Resolve(revision, branch string, refs []Ref) (Target, error) {
	revision = strings.TrimSpace(revision)
	branch = strings.TrimSpace(branch)
	if branch == "" {
		branch = "main"
	}
	if IsFullSHA(revision) {
		return Target{Kind: KindSHA, Name: strings.ToLower(revision), SHA: strings.ToLower(revision)}, nil
	}
	name := revision
	if name == "" {
		name = branch
	}
	if sha := tagCommit(refs, name); revision != "" && sha != "" {
		return Target{Kind: KindTag, Name: name, SHA: sha}, nil
	}
	if sha := branchCommit(refs, name); sha != "" {
		return Target{Kind: KindBranch, Name: name, SHA: sha}, nil
	}
	if revision == "" {
		return Target{}, fmt.Errorf("branch %q not found", name)
	}
	return Target{}, fmt.Errorf("revision %q not found", revision)
}

func branchCommit(refs []Ref, name string) string {
	want := "refs/heads/" + name
	for _, ref := range refs {
		if ref.Name == want {
			return strings.TrimSpace(ref.Hash)
		}
	}
	return ""
}

func tagCommit(refs []Ref, name string) string {
	peeled := "refs/tags/" + name + "^{}"
	plain := "refs/tags/" + name
	var plainHash string
	for _, ref := range refs {
		if ref.Name == peeled && strings.TrimSpace(ref.Hash) != "" {
			return strings.TrimSpace(ref.Hash)
		}
		if ref.Name == plain {
			plainHash = strings.TrimSpace(ref.Hash)
		}
	}
	return plainHash
}
