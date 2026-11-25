package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

const commitPins = `mutation($input: CreateCommitOnBranchInput!) {
  createCommitOnBranch(input: $input) { commit { oid } }
}`

type pinningCLI struct {
	path string
	run  func(context.Context, string, string, any, any) error
}

// NewPinningWriter uses the local GitHub CLI credential without a Git checkout.
func NewPinningWriter() dispatch.PinningWriter {
	path, err := exec.LookPath("gh")
	if err != nil {
		return nil
	}
	writer := &pinningCLI{path: path}
	writer.run = writer.api
	return writer
}

func (w *pinningCLI) OpenPinningPR(ctx context.Context, req release.PinningRequest) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	prefix := "repos/" + req.Repo
	signature := release.PinSignature(req.Changes)
	branch := fmt.Sprintf("release-assistant/pin-%s-%s-%s", strings.TrimPrefix(req.Version, "v"), req.Head[:min(12, len(req.Head))], signature[:12])
	ref := "refs/heads/" + branch
	title := "chore(release): update component pins for " + req.Version
	var body strings.Builder
	body.WriteString("| File | Before | After |\n| --- | --- | --- |\n")
	additions := make([]map[string]string, 0, len(req.Changes))
	for _, change := range req.Changes {
		additions = append(additions, map[string]string{
			"path": change.Path, "contents": base64.StdEncoding.EncodeToString([]byte(change.After)),
		})
		fmt.Fprintf(&body, "| `%s` | `%s` | `%s` |\n", change.Path, strings.TrimSpace(change.Before), strings.TrimSpace(change.After))
	}

	head := req.Head
	if err := w.run(ctx, http.MethodPost, prefix+"/git/refs", map[string]string{"ref": ref, "sha": head}, nil); err != nil {
		// A prior attempt can leave the branch at the release head or at the pin commit.
		var existing struct{ Object struct{ SHA string } }
		endpoint := prefix + "/git/ref/heads/" + branch
		if err := w.run(ctx, http.MethodGet, endpoint, nil, &existing); err != nil || existing.Object.SHA == "" {
			return 0, fmt.Errorf("gh could not create branch %s; check the active GitHub account's write access to %s", branch, req.Repo)
		}
		head = existing.Object.SHA
	}
	if head == req.Head {
		input := map[string]any{
			"branch":          map[string]string{"repositoryNameWithOwner": req.Repo, "branchName": branch},
			"expectedHeadOid": req.Head,
			"message":         map[string]string{"headline": title},
			"fileChanges":     map[string]any{"additions": additions},
		}
		var commit struct {
			Data struct {
				CreateCommitOnBranch struct{ Commit struct{ OID string } }
			}
		}
		if err := w.run(ctx, http.MethodPost, "graphql", map[string]any{"query": commitPins, "variables": map[string]any{"input": input}}, &commit); err != nil || commit.Data.CreateCommitOnBranch.Commit.OID == "" {
			return 0, fmt.Errorf("gh did not confirm the pin commit on branch %s", branch)
		}
	} else {
		paths := make(map[string]bool, len(req.Changes))
		for _, change := range req.Changes {
			paths[change.Path] = true
		}
		var diff pinPRDiff
		endpoint := prefix + "/compare/" + segment(req.Head) + "..." + segment(head)
		if err := w.run(ctx, http.MethodGet, endpoint, nil, &diff); err != nil || exactPinDiff(string(diff), paths) != signature {
			return 0, fmt.Errorf("the existing branch %s could not be verified to contain only the reviewed pin changes", branch)
		}
	}
	var pr struct{ Number int }
	err := w.run(ctx, http.MethodPost, prefix+"/pulls", map[string]any{
		"head": branch, "base": req.Branch, "title": title, "body": body.String(), "draft": false,
	}, &pr)
	if err != nil || pr.Number <= 0 {
		return 0, fmt.Errorf("gh did not confirm a pull request from branch %s; check GitHub for the PR", branch)
	}
	return pr.Number, nil
}

func (w *pinningCLI) api(ctx context.Context, method, endpoint string, payload, result any) error {
	args := []string{"api", "--hostname", "github.com", "--method", method, endpoint}
	var input []byte
	if payload != nil {
		var err error
		input, err = json.Marshal(payload)
		if err != nil {
			return err
		}
		args = append(args, "--input", "-")
	}
	diff, rawDiff := result.(*pinPRDiff)
	if rawDiff {
		args = append(args, "--header", "Accept: application/vnd.github.diff")
	}
	cmd := exec.CommandContext(ctx, w.path, args...)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GH_DEBUG=") && !strings.HasPrefix(value, "GH_PROMPT_DISABLED=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GH_PROMPT_DISABLED=1")
	cmd.Stdin = bytes.NewReader(input)
	data, err := cmd.Output()
	if err != nil || result == nil {
		return err
	}
	if rawDiff {
		*diff = pinPRDiff(data)
		return nil
	}
	return json.Unmarshal(data, result)
}
