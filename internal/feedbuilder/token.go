package feedbuilder

// The token the GitHub API calls carry (release lookups, the Contents API):
// the unauthenticated limit of 60 requests an hour runs out on a feed with
// a few github sources.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	envGitHubToken = "GITHUB_TOKEN" //nolint:gosec // the variable's name, not a credential
	githubHost     = "github.com"
)

// githubToken resolves the GitHub API token and names where it came from:
// GITHUB_TOKEN in the environment wins, then the config's github_token_cmd
// (a failure there is an error: the user asked for it), then gh's own login
// (`gh auth token`) when gh is on PATH — silently none when it is not there
// or not logged in, the calls then run unauthenticated.
func githubToken(ctx context.Context, tokenCmd string) (string, string, error) {
	if token := os.Getenv(envGitHubToken); token != "" {
		return token, envGitHubToken, nil
	}

	if tokenCmd != "" {
		token, err := tokenOutput(ctx, "sh", "-c", tokenCmd)
		if err != nil {
			return "", "", fmt.Errorf("github_token_cmd %q: %w", tokenCmd, err)
		}

		if token == "" {
			return "", "", fmt.Errorf("%w: github_token_cmd %q produced no output", errToken, tokenCmd)
		}

		return token, "github_token_cmd", nil
	}

	if token := ghLoginToken(ctx); token != "" {
		return token, "gh auth token", nil
	}

	return "", "", nil
}

// ghLoginToken is the token of gh's login for github.com, "" when gh is not
// on PATH or not logged in.
func ghLoginToken(ctx context.Context) string {
	if _, err := exec.LookPath("gh"); err != nil {
		return ""
	}

	token, err := tokenOutput(ctx, "gh", "auth", "token", "--hostname", githubHost)
	if err != nil {
		return ""
	}

	return token
}

// tokenOutput runs a program and returns its trimmed stdout alone: a notice
// on stderr (gh's update check) must not get into the token, it goes into
// the error instead. Output of more than one word is not a token either.
func tokenOutput(ctx context.Context, name string, args ...string) (string, error) {
	var out, errOut bytes.Buffer

	cmd := command(ctx, name, args...)
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errOut.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}

		return "", fmt.Errorf("run: %w", err)
	}

	token := strings.TrimSpace(out.String())
	if strings.ContainsAny(token, " \t\n") {
		return "", fmt.Errorf("%w: %s printed more than a token", errToken, name)
	}

	return token, nil
}
