package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// guardRepo is a throwaway repository shaped like the fork: a fork point, an
// upstream line that runs release-please, and a fork line that merges it.
type guardRepo struct {
	t   *testing.T
	dir string
}

func newGuardRepo(t *testing.T) *guardRepo {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the guard is a POSIX sh script run on ubuntu-latest")
	}
	r := &guardRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.write("CHANGELOG.md", "# Changelog\n\n## 1.0.0\n")
	r.write(".release-please-manifest.json", `{".": "1.0.0"}`+"\n")
	r.write("main.go", "package main\n")
	r.commit("fork point")
	r.git("branch", "upstream")
	return r
}

func (r *guardRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_COUNT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *guardRepo) write(path, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, path), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *guardRepo) commit(msg string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

// upstreamRelease advances the upstream branch by one release-please release.
func (r *guardRepo) upstreamRelease(version string) {
	r.t.Helper()
	r.git("checkout", "-q", "upstream")
	changelog, err := os.ReadFile(filepath.Join(r.dir, "CHANGELOG.md"))
	if err != nil {
		r.t.Fatal(err)
	}
	r.write("CHANGELOG.md", string(changelog)+"\n## "+version+"\n")
	r.write(".release-please-manifest.json", `{".": "`+version+`"}`+"\n")
	r.commit("chore(main): release " + version)
	r.git("checkout", "-q", "main")
}

func (r *guardRepo) guard(base, head string, upstream ...string) (string, bool) {
	r.t.Helper()
	script, err := filepath.Abs("scripts/guard-generated-files.sh")
	if err != nil {
		r.t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{script, base, head}, upstream...)...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			r.t.Fatalf("run guard: %v", err)
		}
	}
	return string(out), err == nil
}

// forkMergesUpstream returns the fork's base and the head of a PR branch that
// merged upstream (after one upstream release).
func forkMergesUpstream(r *guardRepo) (base, head string) {
	r.upstreamRelease("1.1.0")
	base = r.git("rev-parse", "main")
	r.git("checkout", "-q", "-b", "pr")
	r.git("merge", "-q", "--no-ff", "--no-edit", "upstream")
	return base, r.git("rev-parse", "HEAD")
}

func TestGuardGeneratedFiles_AMergeOfUpstreamCarriesItsReleaseFiles(t *testing.T) {
	r := newGuardRepo(t)
	base, head := forkMergesUpstream(r)

	out, ok := r.guard(base, head, "upstream")
	if !ok {
		t.Fatalf("merge of upstream refused:\n%s", out)
	}
	if !strings.Contains(out, "CHANGELOG.md: byte-identical to upstream") ||
		!strings.Contains(out, ".release-please-manifest.json: byte-identical to upstream") {
		t.Fatalf("guard did not name what it allowed:\n%s", out)
	}

	// Upstream cutting its next release must not turn the same merge red.
	r.upstreamRelease("1.2.0")
	if out, ok := r.guard(base, head, "upstream"); !ok {
		t.Fatalf("merge refused after upstream moved on:\n%s", out)
	}
}

func TestGuardGeneratedFiles_StaysStrictForAnyOtherChange(t *testing.T) {
	t.Run("hand edit on top of an upstream merge", func(t *testing.T) {
		r := newGuardRepo(t)
		base, _ := forkMergesUpstream(r)
		r.write("CHANGELOG.md", "# Changelog\n\n## 9.9.9 hand-written\n")
		head := r.commit("docs: edit changelog")
		if out, ok := r.guard(base, head, "upstream"); ok || !strings.Contains(out, "::error::This PR modifies release-please-generated files: CHANGELOG.md") {
			t.Fatalf("hand edit allowed (ok=%v):\n%s", ok, out)
		}
	})
	t.Run("copying upstream's newer files without merging it", func(t *testing.T) {
		r := newGuardRepo(t)
		r.upstreamRelease("1.1.0")
		base := r.git("rev-parse", "main")
		r.git("checkout", "-q", "-b", "pr")
		r.git("checkout", "upstream", "--", "CHANGELOG.md", ".release-please-manifest.json")
		head := r.commit("chore: copy release files")
		if out, ok := r.guard(base, head, "upstream"); ok {
			t.Fatalf("copied release files allowed:\n%s", out)
		}
	})
	t.Run("no upstream ref given", func(t *testing.T) {
		r := newGuardRepo(t)
		base, head := forkMergesUpstream(r)
		if out, ok := r.guard(base, head); ok {
			t.Fatalf("allowed without an upstream to compare against:\n%s", out)
		}
	})
	t.Run("upstream ref does not resolve", func(t *testing.T) {
		r := newGuardRepo(t)
		base, head := forkMergesUpstream(r)
		if out, ok := r.guard(base, head, "refs/remotes/upstream/main"); ok || !strings.Contains(out, "no upstream exemption") {
			t.Fatalf("unfetched upstream did not fail closed (ok=%v):\n%s", ok, out)
		}
	})
}

func TestGuardGeneratedFiles_OrdinaryChangePasses(t *testing.T) {
	r := newGuardRepo(t)
	base := r.git("rev-parse", "main")
	r.git("checkout", "-q", "-b", "pr")
	r.write("main.go", "package main\n\nfunc main() {}\n")
	head := r.commit("feat: main")
	if out, ok := r.guard(base, head, "upstream"); !ok {
		t.Fatalf("ordinary change refused:\n%s", out)
	}
}
