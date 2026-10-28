package compose

import "testing"

func TestExternalVolumeAction(t *testing.T) {
	pattern := `^dev-[a-z0-9]+(?:-[a-z0-9]+)*-(pgdata|redisdata|rabbitdata)$`
	cases := []struct {
		name    string
		exists  bool
		create  bool
		pattern string
		want    string
	}{
		{name: "dev-demo-pgdata", exists: true, create: true, pattern: pattern, want: "keep"},
		{name: "dev-demo-pgdata", exists: false, create: false, pattern: pattern, want: "missing"},
		{name: "dev-demo-pgdata", exists: false, create: true, pattern: "", want: "missing"},
		{name: "dev-demo-pgdata", exists: false, create: true, pattern: pattern, want: "create"},
		{name: "dev-test-ru-redisdata", exists: false, create: true, pattern: pattern, want: "create"},
		{name: "dev-demo-database", exists: false, create: true, pattern: pattern, want: "rejected"},
		{name: "prod-demo-pgdata", exists: false, create: true, pattern: pattern, want: "rejected"},
	}
	for _, tc := range cases {
		got, err := externalVolumeAction(tc.name, tc.exists, tc.create, tc.pattern)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
	if _, err := externalVolumeAction("dev-demo-pgdata", false, true, "("); err == nil {
		t.Fatal("invalid pattern was accepted")
	}
}
