package fsperm

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRelaxCheckoutTreeReadableByNobody(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(repo, "prometheus.yml")
	if err := os.WriteFile(cfg, []byte("global: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCfg := filepath.Join(repo, ".git", "config")
	if err := os.WriteFile(gitCfg, []byte("[core]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Simulate v1.0.3 lockdown
	_ = os.Chmod(repo, 0700)
	_ = os.Chmod(cfg, 0600)

	if err := RelaxCheckoutTree(repo); err != nil {
		t.Fatal(err)
	}
	// TempDir parents are often 0700; make the path traversable like /opt/tide/runtime.
	for d := cfg; d != "/" && d != "."; d = filepath.Dir(d) {
		_ = os.Chmod(d, 0755)
		if d == "/tmp" {
			break
		}
	}

	info, err := os.Stat(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0004 == 0 {
		t.Fatalf("checkout file not world-readable: %o", info.Mode().Perm())
	}
	gitInfo, err := os.Stat(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if gitInfo.Mode().Perm()&0077 != 0 {
		t.Fatalf(".git must stay private: %o", gitInfo.Mode().Perm())
	}

	// nobody (65534) must be able to read the file. runuser only works as root;
	// GitHub-hosted runners have the binary but reject it, and the mode check above already covers that case.
	if os.Geteuid() != 0 {
		t.Log("skip nobody exec: not root")
		return
	}
	cmd := exec.Command("runuser", "-u", "nobody", "--", "cat", cfg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Environments without runuser/nobody: fall back to open as other via mode bits only.
		if _, lookErr := exec.LookPath("runuser"); lookErr != nil {
			t.Logf("skip nobody exec (no runuser): %v", err)
			return
		}
		t.Fatalf("nobody cannot read checkout file: %v (%s)", err, out)
	}
	if string(out) != "global: {}\n" {
		t.Fatalf("unexpected content: %q", out)
	}
}

func TestMigrateRuntimePermissions(t *testing.T) {
	runtime := t.TempDir()
	app := filepath.Join(runtime, "app1")
	repo := filepath.Join(app, "repo")
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0700)
	_ = os.MkdirAll(filepath.Join(app, ".envs"), 0700)
	f := filepath.Join(repo, "rules.yml")
	_ = os.WriteFile(f, []byte("x\n"), 0600)
	_ = os.Chmod(repo, 0700)
	_ = os.Chmod(app, 0700)

	if err := MigrateRuntimePermissions(runtime); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(f)
	if info.Mode().Perm()&0004 == 0 {
		t.Fatalf("expected world-readable after migrate: %o", info.Mode().Perm())
	}
	envs, _ := os.Stat(filepath.Join(app, ".envs"))
	if envs.Mode().Perm()&0077 != 0 {
		t.Fatalf(".envs must stay private: %o", envs.Mode().Perm())
	}
}
