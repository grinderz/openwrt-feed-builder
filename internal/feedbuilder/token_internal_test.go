package feedbuilder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGH puts a gh stub on PATH that answers `gh auth token --hostname
// github.com` with the given script body.
func fakeGH(t *testing.T, body string) {
	t.Helper()

	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"[ \"$1 $2 $3 $4\" = \"auth token --hostname github.com\" ] || exit 2\n" +
		body

	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), execPerm); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir)
}

func TestGithubTokenEnvWins(t *testing.T) {
	t.Setenv(envGitHubToken, "ghp_env")

	token, from, err := githubToken(context.Background(), "echo ghp_cmd")
	if err != nil {
		t.Fatal(err)
	}

	if token != "ghp_env" || from != envGitHubToken {
		t.Fatalf("got %q from %q, want the environment token", token, from)
	}
}

func TestGithubTokenCmd(t *testing.T) {
	t.Setenv(envGitHubToken, "")

	token, from, err := githubToken(context.Background(), "echo notice >&2; echo ' ghp_cmd '")
	if err != nil {
		t.Fatal(err)
	}

	if token != "ghp_cmd" || from != "github_token_cmd" {
		t.Fatalf("got %q from %q, want the command's token", token, from)
	}
}

func TestGithubTokenCmdFailures(t *testing.T) {
	t.Setenv(envGitHubToken, "")

	cases := []struct {
		name, cmd, want string
	}{
		{"exit status", "echo broken >&2; exit 3", "broken"},
		{"no output", "true", "produced no output"},
		{"several words", "echo one two", "more than a token"},
	}
	for _, tcase := range cases {
		_, _, err := githubToken(context.Background(), tcase.cmd)
		if err == nil || !strings.Contains(err.Error(), tcase.want) {
			t.Fatalf("%s: err = %v, want it to mention %q", tcase.name, err, tcase.want)
		}
	}
}

func TestGithubTokenNoGH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv(envGitHubToken, "")

	token, from, err := githubToken(context.Background(), "")
	if err != nil || token != "" || from != "" {
		t.Fatalf("got %q from %q, err %v; want none without gh", token, from, err)
	}
}

func TestGithubTokenGH(t *testing.T) {
	t.Setenv(envGitHubToken, "")
	fakeGH(t, "echo 'A new release of gh is available' >&2\necho gho_gh\n")

	token, from, err := githubToken(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	if token != "gho_gh" || from != "gh auth token" {
		t.Fatalf("got %q from %q, want gh's token", token, from)
	}
}

func TestGithubTokenGHNotLoggedIn(t *testing.T) {
	t.Setenv(envGitHubToken, "")

	cases := []struct{ name, body string }{
		{"exit status", "echo 'no oauth token found' >&2\nexit 1\n"},
		{"empty", "exit 0\n"},
		{"several words", "echo not a token\n"},
	}
	for _, tcase := range cases {
		fakeGH(t, tcase.body)

		token, from, err := githubToken(context.Background(), "")
		if err != nil || token != "" || from != "" {
			t.Fatalf("%s: got %q from %q, err %v; want none, silently", tcase.name, token, from, err)
		}
	}
}
