package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"release-assistant/internal/app"
	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

// Writer contains the only GitHub write operation: posting one retest comment.
type Writer struct {
	client *Client
	log    *app.Log
}

// NewWriter binds the comment writer to the activity log used before each POST.
func NewWriter(client *Client, log *app.Log) *Writer { return &Writer{client: client, log: log} }

// Write posts exactly one approved comment. A transport failure or server error is uncertain.
func (w *Writer) Write(ctx context.Context, proposal release.Proposal) (dispatch.Delivery, error) {
	if w == nil || w.client == nil || !proposal.Target.Valid() || proposal.SHA == "" || proposal.Payload == "" {
		return dispatch.Delivery{Status: "failed"}, fmt.Errorf("invalid GitHub comment proposal")
	}
	var endpoint string
	payload := map[string]string{"body": proposal.CommentBody()}
	if proposal.Target.PR > 0 {
		endpoint = repoPath(proposal.Target.Repo) + "/issues/" + strconv.Itoa(proposal.Target.PR) + "/comments"
	} else {
		endpoint = repoPath(proposal.Target.Repo) + "/commits/" + segment(proposal.SHA) + "/comments"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return dispatch.Delivery{Status: "failed"}, fmt.Errorf("encode GitHub comment")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.client.baseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return dispatch.Delivery{Status: "failed"}, fmt.Errorf("build GitHub comment request")
	}
	w.client.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	if w.log == nil {
		return dispatch.Delivery{Status: "failed"}, fmt.Errorf("cannot post GitHub comment without an activity log")
	}
	if err := w.log.GitHubCommentAttempt(ctx, proposal); err != nil {
		return dispatch.Delivery{Status: "failed"}, fmt.Errorf("cannot log GitHub comment before posting: %w", err)
	}
	resp, err := w.client.http.Do(req)
	if err != nil {
		return dispatch.Delivery{Status: "uncertain"}, fmt.Errorf("GitHub comment delivery uncertain")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode >= 500 {
			return dispatch.Delivery{Status: "uncertain"}, fmt.Errorf("GitHub comment delivery uncertain (HTTP %d)", resp.StatusCode)
		}
		return dispatch.Delivery{Status: "failed"}, fmt.Errorf("GitHub comment rejected (HTTP %d)", resp.StatusCode)
	}
	var result struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		// The successful response means the server accepted the write even if its link is unreadable.
		return dispatch.Delivery{Status: "sent"}, nil
	}
	return dispatch.Delivery{Status: "sent", URL: result.HTMLURL}, nil
}
