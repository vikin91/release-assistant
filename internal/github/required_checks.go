package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"release-assistant/internal/release"
)

type (
	requiredCheck struct {
		context string
		appID   int64
	}
	branchRule struct {
		Type       string `json:"type"`
		Parameters struct {
			Checks []struct {
				Context string `json:"context"`
				AppID   int64  `json:"integration_id"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
)

// requiredChecks retains verified requirements even when one policy source is unavailable.
func (c *Client) requiredChecks(ctx context.Context, src release.Source, classicEnforcement string) ([]requiredCheck, string) {
	base, branch := repoPath(src.Repo), segment(src.Branch)
	var classic branchProtectionResponse
	var checks []requiredCheck
	var problems []string
	if err := c.get(ctx, base+"/branches/"+branch+"/protection/required_status_checks", &classic); err != nil {
		// A 404 can also hide settings from the caller. Only explicit branch
		// metadata establishes that classic required checks are disabled.
		failure, ok := errors.AsType[*requestError](err)
		if !ok || failure.statusCode != http.StatusNotFound || classicEnforcement != "off" {
			problems = append(problems, "Classic branch protection: "+requestProblem(err))
		}
	} else if classic.Contexts == nil && classic.Checks == nil {
		problems = append(problems, "Classic branch protection omitted the required checks.")
	} else {
		for _, check := range classic.Checks {
			checks = append(checks, requiredCheck{check.Context, max(0, check.AppID)})
		}
		// GitHub also lists app-bound checks in the legacy contexts array.
		for _, name := range classic.Contexts {
			if !slices.ContainsFunc(checks, func(check requiredCheck) bool { return check.context == name }) {
				checks = append(checks, requiredCheck{context: name})
			}
		}
	}
	rules, err := c.branchRuleChecks(ctx, base+"/rules/branches/"+branch)
	if err != nil {
		problems = append(problems, "Branch rulesets: "+requestProblem(err))
	}
	for _, rule := range rules {
		if !slices.Contains(checks, rule) {
			checks = append(checks, rule)
		}
	}
	return checks, strings.Join(problems, " ")
}

func (c *Client) branchRuleChecks(ctx context.Context, endpoint string) ([]requiredCheck, error) {
	var checks []requiredCheck
	for page := 1; page <= maxPages; page++ {
		var rules []branchRule
		if err := c.get(ctx, fmt.Sprintf("%s?per_page=100&page=%d", endpoint, page), &rules); err != nil {
			return checks, err
		}
		if rules == nil {
			return checks, fmt.Errorf("GitHub omitted the branch rules")
		}
		for _, rule := range rules {
			switch rule.Type {
			case "":
				return checks, fmt.Errorf("GitHub omitted a branch rule type")
			case "required_status_checks":
				if rule.Parameters.Checks == nil {
					return checks, fmt.Errorf("GitHub omitted required status check parameters")
				}
				for _, check := range rule.Parameters.Checks {
					if check.Context == "" {
						return checks, fmt.Errorf("GitHub omitted a required status check context")
					}
					checks = append(checks, requiredCheck{check.Context, max(0, check.AppID)})
				}
			}
		}
		if len(rules) < 100 {
			return checks, nil
		}
	}
	return checks, fmt.Errorf("GitHub branch rules exceeded pagination limit")
}
