package review

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateTargetBranch(t *testing.T) {
	for _, okBranch := range []string{"bot/fix-1", "agent/revert-abc123", "feat/add-tests", "develop", "release"} {
		if got, err := validateTargetBranch(okBranch, nil); err != nil || got != okBranch {
			t.Errorf("validateTargetBranch(%q, nil) = (%q, %v), want (%q, nil)", okBranch, got, err, okBranch)
		}
	}
	for _, bad := range []string{"", "main", "master", "refs/heads/main", "HEAD", "--force", "feat..bar", "has space", "feat/tail/"} {
		if _, err := validateTargetBranch(bad, nil); err == nil {
			t.Errorf("validateTargetBranch(%q, nil) succeeded, expected error", bad)
		}
	}

	custom := []string{"release", "production"}
	if got, err := validateTargetBranch("main", custom); err != nil || got != "main" {
		t.Errorf("validateTargetBranch(\"main\", custom) = (%q, %v), want (\"main\", nil)", got, err)
	}
	for _, bad := range []string{"release", "production"} {
		if _, err := validateTargetBranch(bad, custom); err == nil {
			t.Errorf("validateTargetBranch(%q, custom) succeeded, expected error", bad)
		}
	}

	noneProtected := []string{}
	if got, err := validateTargetBranch("main", noneProtected); err != nil || got != "main" {
		t.Errorf("validateTargetBranch(\"main\", []string{}) = (%q, %v), want (\"main\", nil)", got, err)
	}
	for _, bad := range []string{"--force", "feat..bar"} {
		if _, err := validateTargetBranch(bad, noneProtected); err == nil {
			t.Errorf("validateTargetBranch(%q, []string{}) succeeded, expected ref syntax error", bad)
		}
	}
}

func TestValidateChangedPaths(t *testing.T) {
	if err := validateChangedPaths(nil); err == nil {
		t.Fatal("expected error for empty changed paths")
	}
	if err := validateChangedPaths([]string{"pkg/review/poster.go", "README.md"}); err != nil {
		t.Fatalf("unexpected error for safe paths: %v", err)
	}
	for _, bad := range []string{".github/workflows/ci.yml", "./.github/WORKFLOWS/deploy.yaml", ".github/CODEOWNERS", "CODEOWNERS"} {
		if err := validateChangedPaths([]string{"pkg/a.go", bad}); err == nil {
			t.Errorf("validateChangedPaths(%q) succeeded, expected blocked path error", bad)
		}
	}
}

func TestParsePorcelainZRenamesAndBlockedPaths(t *testing.T) {
	cases := []struct {
		name        string
		porcelain   string
		wantPaths   []string
		wantBlocked bool
	}{
		{
			name:        "rename into workflow directory",
			porcelain:   "R  .github/workflows/ci.yml\x00safe.go\x00",
			wantPaths:   []string{".github/workflows/ci.yml", "safe.go"},
			wantBlocked: true,
		},
		{
			name:        "rename out of workflow directory",
			porcelain:   "R  safe.go\x00.github/workflows/ci.yml\x00",
			wantPaths:   []string{"safe.go", ".github/workflows/ci.yml"},
			wantBlocked: true,
		},
		{
			name:        "copy followed by safe modification preserves subsequent token",
			porcelain:   "C  copy.go\x00src.go\x00M  other.go\x00",
			wantPaths:   []string{"copy.go", "src.go", "other.go"},
			wantBlocked: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePorcelainZ(tc.porcelain)
			if len(got) != len(tc.wantPaths) {
				t.Fatalf("parsePorcelainZ() = %v, want %v", got, tc.wantPaths)
			}
			for i := range tc.wantPaths {
				if got[i] != tc.wantPaths[i] {
					t.Fatalf("parsePorcelainZ()[%d] = %q, want %q (all: %v)", i, got[i], tc.wantPaths[i], got)
				}
			}
			err := validateChangedPaths(got)
			if tc.wantBlocked && err == nil {
				t.Fatalf("expected validateChangedPaths(%v) to reject blocked path", got)
			}
			if !tc.wantBlocked && err != nil {
				t.Fatalf("expected validateChangedPaths(%v) to pass, got %v", got, err)
			}
		})
	}
}

func initTestRepoWithBareRemote(t *testing.T) (bareDir, workDir string) {
	t.Helper()
	root := t.TempDir()
	bareDir = filepath.Join(root, "remote.git")
	workDir = filepath.Join(root, "work")

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, string(out))
		}
	}

	if out, err := exec.Command("git", "init", "--bare", "-b", "main", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare failed: %v\n%s", err, string(out))
	}
	if out, err := exec.Command("git", "clone", bareDir, workDir).CombinedOutput(); err != nil {
		t.Fatalf("git clone failed: %v\n%s", err, string(out))
	}
	if err := os.WriteFile(filepath.Join(workDir, "app.go"), []byte("package app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "remove.go"), []byte("package app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(workDir, "checkout", "-b", "main")
	run(workDir, "add", "app.go", "remove.go")
	run(workDir, "commit", "-m", "initial commit")
	run(workDir, "push", "-u", "origin", "main")
	return bareDir, workDir
}

func TestGhPosterBranchCommitEndToEnd(t *testing.T) {
	bareDir, workDir := initTestRepoWithBareRemote(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(workDir, "app.go"), []byte("package app\n\nfunc Fixed() bool { return true }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "added.go"), []byte("package app\n\nfunc Added() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(workDir, "remove.go")); err != nil {
		t.Fatal(err)
	}

	poster := &GhPoster{TokenFile: tokenFile}
	err := poster.PostReview(context.Background(), Request{
		Workspace:    workDir,
		Repo:         "org/repo",
		Output:       OutputBranchCommit,
		TargetBranch: "bot/fix-42",
	}, &Result{
		Verdict: "APPROVE",
		Title:   "fix: add Fixed helper",
		Summary: "Implemented Fixed and verified locally.",
	})
	if err != nil {
		t.Fatalf("PostReview branch-commit failed: %v", err)
	}

	// Bare remote must now have refs/heads/bot/fix-42 while main stays untouched at initial commit.
	showBranch, err := exec.Command("git", "-C", bareDir, "rev-parse", "--verify", "refs/heads/bot/fix-42").CombinedOutput()
	if err != nil {
		t.Fatalf("expected refs/heads/bot/fix-42 on bare remote: %v (%s)", err, string(showBranch))
	}
	mainHead, _ := exec.Command("git", "-C", bareDir, "rev-parse", "refs/heads/main").CombinedOutput()
	if strings.TrimSpace(string(showBranch)) == strings.TrimSpace(string(mainHead)) {
		t.Fatal("expected bot/fix-42 to advance ahead of main")
	}

	treeOut, err := exec.Command("git", "-C", bareDir, "ls-tree", "--name-only", "refs/heads/bot/fix-42").CombinedOutput()
	if err != nil {
		t.Fatalf("git ls-tree failed: %v (%s)", err, string(treeOut))
	}
	tree := string(treeOut)
	if !strings.Contains(tree, "app.go") || !strings.Contains(tree, "added.go") || strings.Contains(tree, "remove.go") {
		t.Fatalf("unexpected tree on bot/fix-42 (want app.go & added.go, no remove.go):\n%s", tree)
	}
}

func TestGhPosterBranchCommitStagedGitMvAndRm(t *testing.T) {
	bareDir, workDir := initTestRepoWithBareRemote(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	if out, err := exec.Command("git", "-C", workDir, "mv", "app.go", "renamed.go").CombinedOutput(); err != nil {
		t.Fatalf("git mv failed: %v\n%s", err, string(out))
	}
	if out, err := exec.Command("git", "-C", workDir, "rm", "remove.go").CombinedOutput(); err != nil {
		t.Fatalf("git rm failed: %v\n%s", err, string(out))
	}
	// Also append an unstaged edit to renamed.go (RM state in porcelain) to cover mixed staged+unstaged rename.
	if err := os.WriteFile(filepath.Join(workDir, "renamed.go"), []byte("package app\n\nfunc Renamed() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	poster := &GhPoster{TokenFile: tokenFile}
	err := poster.PostReview(context.Background(), Request{
		Workspace:    workDir,
		Repo:         "org/repo",
		Output:       OutputBranchCommit,
		TargetBranch: "bot/rename-42",
	}, &Result{
		Verdict: "APPROVE",
		Title:   "refactor: rename app.go and drop remove.go",
		Summary: "Renamed app.go via git mv and deleted remove.go via git rm.",
	})
	if err != nil {
		t.Fatalf("PostReview after git mv/rm failed: %v", err)
	}

	treeOut, err := exec.Command("git", "-C", bareDir, "ls-tree", "--name-only", "refs/heads/bot/rename-42").CombinedOutput()
	if err != nil {
		t.Fatalf("git ls-tree failed: %v (%s)", err, string(treeOut))
	}
	tree := string(treeOut)
	if !strings.Contains(tree, "renamed.go") || strings.Contains(tree, "app.go") || strings.Contains(tree, "remove.go") {
		t.Fatalf("unexpected tree on bot/rename-42 (want only renamed.go):\n%s", tree)
	}
	contentOut, err := exec.Command("git", "-C", bareDir, "show", "refs/heads/bot/rename-42:renamed.go").CombinedOutput()
	if err != nil || !strings.Contains(string(contentOut), "func Renamed()") {
		t.Fatalf("expected unstaged edit on renamed.go to be included in commit, got %q (%v)", string(contentOut), err)
	}
}

func TestGhPosterBranchCommitRejectsWorkflowEdit(t *testing.T) {
	_, workDir := initTestRepoWithBareRemote(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	wfDir := filepath.Join(workDir, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "ci.yml"), []byte("name: hacked\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	poster := &GhPoster{TokenFile: tokenFile}
	err := poster.PostReview(context.Background(), Request{
		Workspace:    workDir,
		Repo:         "org/repo",
		Output:       OutputBranchCommit,
		TargetBranch: "bot/fix-42",
	}, &Result{Verdict: "COMMENT", Summary: "attempt"})
	if err == nil || !strings.Contains(err.Error(), "blocked path") {
		t.Fatalf("expected blocked path error for workflow edit, got %v", err)
	}
}

func TestGhPosterRevertPROpensPR(t *testing.T) {
	bareDir, workDir := initTestRepoWithBareRemote(t)
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(dir, "gh-args.txt")
	ghScript := filepath.Join(dir, "fake-gh.sh")
	script := "#!/bin/sh\nif [ \"$1\" = \"pr\" ] && [ \"$2\" = \"list\" ]; then\n  printf '[]\\n'\n  exit 0\nfi\nprintf '%s\\n' \"$*\" >> \"" + argsFile + "\"\n"
	if err := os.WriteFile(ghScript, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(workDir, "app.go"), []byte("// reverted\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	poster := &GhPoster{TokenFile: tokenFile, GHBin: ghScript}
	err := poster.PostReview(context.Background(), Request{
		Workspace: workDir,
		Repo:      "org/repo",
		Output:    OutputRevertPR,
		HeadSHA:   "abcdef1234567890",
		BaseRef:   "main",
	}, &Result{
		Verdict: "REQUEST_CHANGES",
		Title:   "revert: roll back abcdef1",
		Summary: "Smoke check failed.",
	})
	if err != nil {
		t.Fatalf("PostReview revert-pr failed: %v", err)
	}

	if out, err := exec.Command("git", "-C", bareDir, "rev-parse", "--verify", "refs/heads/bot/revert-abcdef123456").CombinedOutput(); err != nil {
		t.Fatalf("expected revert branch on bare remote: %v (%s)", err, string(out))
	}

	ghArgsRaw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	ghArgs := string(ghArgsRaw)
	for _, want := range []string{"pr create", "--repo org/repo", "--base main", "--head bot/revert-abcdef123456"} {
		if !strings.Contains(ghArgs, want) {
			t.Fatalf("expected %q in gh pr create args, got %q", want, ghArgs)
		}
	}
}

func TestGhPosterOpenPRSkipsCreateWhenExistingOpenPR(t *testing.T) {
	bareDir, workDir := initTestRepoWithBareRemote(t)
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(dir, "gh-args.txt")
	ghScript := filepath.Join(dir, "fake-gh.sh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + argsFile + "\"\nif [ \"$1\" = \"pr\" ] && [ \"$2\" = \"list\" ]; then\n  printf '[{\"number\":42}]\\n'\n  exit 0\nfi\necho \"unexpected gh call: $*\" >&2\nexit 1\n"
	if err := os.WriteFile(ghScript, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(workDir, "app.go"), []byte("// fix attempt 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	poster := &GhPoster{TokenFile: tokenFile, GHBin: ghScript}
	err := poster.PostReview(context.Background(), Request{
		Workspace:    workDir,
		Repo:         "org/repo",
		Output:       OutputBranchCommit,
		TargetBranch: "bot/fix-42",
		OpenPR:       true,
	}, &Result{
		Verdict: "COMMENT",
		Title:   "fix: retry update",
		Summary: "Updated existing PR branch.",
	})
	if err != nil {
		t.Fatalf("expected existing PR check to succeed without gh pr create, got: %v", err)
	}

	if out, err := exec.Command("git", "-C", bareDir, "rev-parse", "--verify", "refs/heads/bot/fix-42").CombinedOutput(); err != nil {
		t.Fatalf("expected branch on bare remote: %v (%s)", err, string(out))
	}
	ghArgsRaw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ghArgsRaw), "pr create") {
		t.Fatalf("did not expect gh pr create when open PR already exists; calls:\n%s", string(ghArgsRaw))
	}
}

func TestGhPosterDetachedHEADResolution(t *testing.T) {
	t.Run("uses hydrated HeadRef in detached HEAD checkout", func(t *testing.T) {
		bareDir, workDir := initTestRepoWithBareRemote(t)
		cmd := exec.Command("git", "-C", workDir, "checkout", "--detach", "HEAD")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git checkout --detach failed: %v\n%s", err, string(out))
		}

		tokenFile := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(tokenFile, []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workDir, "app.go"), []byte("package app\n\nfunc FromPRHead() {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		poster := &GhPoster{TokenFile: tokenFile}
		err := poster.PostReview(context.Background(), Request{
			Workspace: workDir,
			Repo:      "org/repo",
			Output:    OutputBranchCommit,
			HeadRef:   "feat/pr-head-fix",
		}, &Result{Verdict: "APPROVE", Summary: "Applied fix on PR head branch."})
		if err != nil {
			t.Fatalf("expected detached HEAD with HeadRef to push to feat/pr-head-fix, got: %v", err)
		}

		if out, err := exec.Command("git", "-C", bareDir, "rev-parse", "--verify", "refs/heads/feat/pr-head-fix").CombinedOutput(); err != nil {
			t.Fatalf("expected refs/heads/feat/pr-head-fix on bare remote: %v (%s)", err, string(out))
		}
	})

	t.Run("fails clearly on detached HEAD without HeadRef or TargetBranch", func(t *testing.T) {
		_, workDir := initTestRepoWithBareRemote(t)
		cmd := exec.Command("git", "-C", workDir, "checkout", "--detach", "HEAD")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git checkout --detach failed: %v\n%s", err, string(out))
		}

		tokenFile := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(tokenFile, []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workDir, "app.go"), []byte("// change\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		poster := &GhPoster{TokenFile: tokenFile}
		err := poster.PostReview(context.Background(), Request{
			Workspace: workDir,
			Repo:      "org/repo",
			Output:    OutputBranchCommit,
		}, &Result{Verdict: "COMMENT", Summary: "Attempt without branch"})
		if err == nil || !strings.Contains(err.Error(), "detached HEAD") {
			t.Fatalf("expected detached HEAD error, got %v", err)
		}
	})
}
