package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"release-assistant/internal/release"
)

func TestPinningWriterReportsBranchErrorBeforeLookingUpExistingRef(t *testing.T) {
	t.Setenv("GH_TOKEN", "fixture-credential")
	path := filepath.Join(t.TempDir(), "fake-gh")
	script := "#!/bin/sh\n" +
		"if [ \"$5\" = POST ]; then\n" +
		"  printf '%s\\n' 'gh: Forbidden (HTTP 403)' >&2\n" +
		"  printf '{\"errors\":[{\"message\":\"Branch creation restricted for %s\"}]}' \"$GH_TOKEN\"\n" +
		"else\n" +
		"  printf '%s\\n' 'gh: Not Found (HTTP 404)' >&2\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	writer := pinningCLI{path: path}
	writer.run = writer.api
	req := release.PinningRequest{Repo: "stackrox/stackrox", Branch: "release-5.0", Head: "base", Version: "5.0.1",
		Changes: []release.PinChange{{Path: "FACT_VERSION", Before: "0.3.76\n", After: "0.3.77\n"}}}
	_, err := writer.OpenPinningPR(t.Context(), req)
	if err == nil || !strings.Contains(err.Error(), "Forbidden (HTTP 403)") ||
		!strings.Contains(err.Error(), "Branch creation restricted for [redacted]") ||
		strings.Contains(err.Error(), "fixture-credential") || strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPinningWriterOpensRegularPR(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		req := release.PinningRequest{Repo: "stackrox/stackrox", Branch: "release-5.0", Head: "base", Version: "5.0.1",
			Changes: []release.PinChange{{Path: "FACT_VERSION", Before: "0.3.76\n", After: "0.3.77\n"}}}
		var operations []string
		writer := pinningCLI{run: func(_ context.Context, method, endpoint string, payload, response any) error {
			operations = append(operations, method+" "+endpoint)
			switch {
			case strings.HasSuffix(endpoint, "/git/refs"):
				body := payload.(map[string]string)
				if !strings.HasPrefix(body["ref"], "refs/heads/release-assistant/") || body["sha"] != req.Head {
					t.Fatalf("wrong ref payload: %v", payload)
				}
				if recovered {
					return fmt.Errorf("ref exists")
				}
				return nil
			case strings.Contains(endpoint, "/git/ref/heads/"):
				return json.Unmarshal([]byte(`{"object":{"sha":"committed"}}`), response)
			case strings.HasSuffix(endpoint, "/compare/base...committed"):
				*response.(*pinPRDiff) = factPinDiff
				return nil
			case endpoint == "graphql":
				input := payload.(map[string]any)["variables"].(map[string]any)["input"].(map[string]any)
				files := input["fileChanges"].(map[string]any)["additions"].([]map[string]string)
				if input["expectedHeadOid"] != req.Head || len(files) != 1 || files[0]["path"] != "FACT_VERSION" || files[0]["contents"] != "MC4zLjc3Cg==" {
					t.Fatalf("wrong commit payload: %v", input)
				}
				return json.Unmarshal([]byte(`{"data":{"createCommitOnBranch":{"commit":{"oid":"committed"}}}}`), response)
			case strings.HasSuffix(endpoint, "/pulls"):
				body := payload.(map[string]any)
				if body["base"] != req.Branch || body["draft"] != false || !strings.HasPrefix(body["head"].(string), "release-assistant/") {
					t.Fatalf("wrong PR payload: %v", body)
				}
				return json.Unmarshal([]byte(`{"number":123}`), response)
			default:
				t.Fatalf("unexpected request: %s %s", method, endpoint)
				return nil
			}
		}}
		number, err := writer.OpenPinningPR(t.Context(), req)
		if err != nil || number != 123 || slices.Contains(operations, http.MethodPost+" graphql") == recovered {
			t.Fatalf("number=%d err=%v operations=%v", number, err, operations)
		}
	}
}
