package review

import (
	"encoding/json"
	"os"
	"strings"
)

// HydrateFromEvent enriches req with context fields parsed from GitHub's
// GITHUB_EVENT_PATH JSON payload without overwriting values already set on req.
func HydrateFromEvent(req Request) Request {
	if req.EventPath == "" {
		return req
	}
	raw, err := os.ReadFile(req.EventPath)
	if err != nil {
		return req
	}

	var payload struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		PullRequest struct {
			Number int `json:"number"`
			Base   struct {
				Ref string `json:"ref"`
			} `json:"base"`
			Head struct {
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
		Issue struct {
			Number      int             `json:"number"`
			PullRequest json.RawMessage `json:"pull_request"`
		} `json:"issue"`
		WorkflowRun struct {
			ID           int64  `json:"id"`
			HeadSHA      string `json:"head_sha"`
			PullRequests []struct {
				Number int `json:"number"`
				Base   struct {
					Ref string `json:"ref"`
				} `json:"base"`
			} `json:"pull_requests"`
		} `json:"workflow_run"`
		Deployment struct {
			SHA         string `json:"sha"`
			Environment string `json:"environment"`
		} `json:"deployment"`
		DeploymentStatus struct {
			EnvironmentURL string `json:"environment_url"`
		} `json:"deployment_status"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return req
	}

	if req.Repo == "" {
		req.Repo = payload.Repository.FullName
	}

	// pull_request event
	if req.PRNumber == 0 {
		req.PRNumber = payload.PullRequest.Number
	}
	if req.BaseRef == "" {
		req.BaseRef = payload.PullRequest.Base.Ref
	}
	if req.HeadSHA == "" {
		req.HeadSHA = payload.PullRequest.Head.SHA
	}

	// issues / issue_comment event
	if req.IssueNumber == 0 && payload.Issue.Number > 0 {
		req.IssueNumber = payload.Issue.Number
	}
	if req.PRNumber == 0 && payload.Issue.Number > 0 && isPresentJSON(payload.Issue.PullRequest) {
		req.PRNumber = payload.Issue.Number
	}

	// workflow_run event
	if req.WorkflowRunID == 0 {
		req.WorkflowRunID = payload.WorkflowRun.ID
	}
	if req.HeadSHA == "" {
		req.HeadSHA = payload.WorkflowRun.HeadSHA
	}
	if req.PRNumber == 0 && len(payload.WorkflowRun.PullRequests) > 0 {
		req.PRNumber = payload.WorkflowRun.PullRequests[0].Number
	}
	if req.BaseRef == "" && len(payload.WorkflowRun.PullRequests) > 0 {
		req.BaseRef = payload.WorkflowRun.PullRequests[0].Base.Ref
	}

	// push (tags) event
	if req.Tag == "" && strings.HasPrefix(payload.Ref, "refs/tags/") {
		req.Tag = strings.TrimPrefix(payload.Ref, "refs/tags/")
	}
	if req.HeadSHA == "" && payload.After != "" {
		req.HeadSHA = payload.After
	}

	// deployment_status / deployment event
	if req.Environment == "" {
		req.Environment = payload.Deployment.Environment
	}
	if req.EnvironmentURL == "" {
		req.EnvironmentURL = payload.DeploymentStatus.EnvironmentURL
	}
	if req.HeadSHA == "" {
		req.HeadSHA = payload.Deployment.SHA
	}

	return req
}

func isPresentJSON(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}
