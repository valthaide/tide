package gitrev

import "testing"

func TestResolveBranchTagSHA(t *testing.T) {
	refs := []Ref{
		{Name: "refs/heads/main", Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{Name: "refs/heads/dev", Hash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{Name: "refs/tags/v1.2.3", Hash: "cccccccccccccccccccccccccccccccccccccccc"},
		{Name: "refs/tags/v1.2.3^{}", Hash: "dddddddddddddddddddddddddddddddddddddddd"},
		{Name: "refs/tags/same", Hash: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"},
		{Name: "refs/heads/same", Hash: "ffffffffffffffffffffffffffffffffffffffff"},
	}

	got, err := Resolve("", "main", refs)
	if err != nil || got.Kind != KindBranch || got.SHA != refs[0].Hash {
		t.Fatalf("default branch: %+v %v", got, err)
	}

	got, err = Resolve("dev", "main", refs)
	if err != nil || got.Kind != KindBranch || got.Name != "dev" {
		t.Fatalf("named branch: %+v %v", got, err)
	}

	got, err = Resolve("v1.2.3", "main", refs)
	if err != nil || got.Kind != KindTag || got.SHA != "dddddddddddddddddddddddddddddddddddddddd" {
		t.Fatalf("peeled tag: %+v %v", got, err)
	}

	got, err = Resolve("same", "main", refs)
	if err != nil || got.Kind != KindTag {
		t.Fatalf("tag wins over branch: %+v %v", got, err)
	}

	sha := "0123456789abcdef0123456789abcdef01234567"
	got, err = Resolve(sha, "main", nil)
	if err != nil || got.Kind != KindSHA || got.SHA != sha {
		t.Fatalf("sha: %+v %v", got, err)
	}

	if _, err := Resolve("missing", "main", refs); err == nil {
		t.Fatal("expected not found")
	}
}
