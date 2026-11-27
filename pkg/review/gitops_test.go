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
	for _, okBranch := range []string{"bot/fix-1", "agent/revert-abc123", "feat/add-tests"} {
		if got, err := validateTargetBranch(okBranch); err != nil || got != okBranch {
			t.Errorf("validateTargetBranch(%q) = (%q, %v), want (%q, nil)", okBranch, got, err, okBranch)
		}
	}
	for _, bad := range []string{"", "main", "master", "refs/heads/main", "HEAD", "develop", "production", "release", "--force", "feat..bar", "has space", "feat/tail/"} {
		if _, err := validateTargetBranch(bad); err == nil {
			t.Errorf("validateTargetBranch(%q) succeeded, expected error", bad)
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
	run(workDir, "checkout", "-b", "main")
	run(workDir, "add", "app.go")
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
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"" + argsFile + "\"\n"
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
