package fleetcheck

import "testing"

func TestCheckProjectAllowsUnderscore(t *testing.T) {
	if CheckProject("") != nil || CheckProject("crm_backend") != nil || CheckProject("crm-backend") != nil {
		t.Fatal("underscore and hyphen are valid")
	}
	if CheckProject("CRM") == nil || CheckProject(string(make([]byte, 64))) == nil {
		t.Fatal("uppercase and length must fail")
	}
}

func TestProjectConflicts(t *testing.T) {
	issues := ProjectConflicts([]AppName{
		{Name: "a", Project: "demo"},
		{Name: "b", Project: "demo"},
		{Name: "c", Project: "other"},
	})
	if len(issues) != 2 {
		t.Fatalf("issues %+v", issues)
	}
	if issues[0].Message != "validation_failed: project_conflict a b" {
		t.Fatalf("message %s", issues[0].Message)
	}
}

func TestSummarizeCompose(t *testing.T) {
	raw := []byte(`{
	  "services": {
	    "web": {
	      "image": "registry.example/app:1@sha256:abc",
	      "container_name": "web",
	      "ports": [{"published": "7777", "target": 80}],
	      "volumes": [{"type": "bind", "source": "/var/lib/data", "target": "/data"}],
	      "labels": {"com.conops.oneshot": "rerun"}
	    },
	    "db": {"image": "postgres:16"}
	  },
	  "volumes": {"pg": {"name": "demo_pg", "external": true}}
	}`)
	sum, err := SummarizeCompose(raw)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Images["web"] == "" || len(sum.Published) != 1 || sum.Published[0] != 7777 {
		t.Fatalf("%+v", sum)
	}
	if len(sum.ExternalVols) != 1 || sum.ExternalVols[0] != "demo_pg" {
		t.Fatalf("vols %+v", sum.ExternalVols)
	}
	if len(DigestRequired(sum.Images)) != 1 || DigestRequired(sum.Images)[0].Message != "digest_required: db" {
		t.Fatalf("digest %+v", DigestRequired(sum.Images))
	}
	if got := BindViolations(sum.BindSources, []string{"/srv/slots"}); len(got) != 1 {
		t.Fatalf("bind %+v", got)
	}
	if got := BindViolations(sum.BindSources, nil); got != nil {
		t.Fatalf("empty prefixes must allow all, got %+v", got)
	}
	if ImageDigest(sum.Images["web"]) != "sha256:abc" {
		t.Fatal(ImageDigest(sum.Images["web"]))
	}
}

func TestPublicVarHidesSecrets(t *testing.T) {
	if !PublicVar("APP_TAG", nil) || !PublicVar("BACKEND_IMAGE", nil) {
		t.Fatal("tag and image keys are public")
	}
	if PublicVar("DB_PASSWORD", []string{"DB_PASSWORD"}) || PublicVar("API_TOKEN", nil) {
		t.Fatal("secrets stay hidden")
	}
	if !PublicVar("COLOR", []string{"COLOR"}) {
		t.Fatal("listed key is public")
	}
	kept := PublicVars(map[string]string{"APP_TAG": "1", "DB_PASSWORD": "secret", "COLOR": "blue"}, []string{"COLOR"})
	if kept["APP_TAG"] != "1" || kept["COLOR"] != "blue" || kept["DB_PASSWORD"] != "" {
		t.Fatalf("%v", kept)
	}
}
