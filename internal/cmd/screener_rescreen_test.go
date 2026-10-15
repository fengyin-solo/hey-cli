package cmd

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/basecamp/hey-cli/internal/apierr"
	"github.com/basecamp/hey-cli/internal/output"
)

// rescreenPatches lists the decided-sender PATCH requests in the order they
// were sent.
func rescreenPatches(t *testing.T, requests []recordedScreenerRequest) []string {
	t.Helper()
	var paths []string
	for _, req := range requests {
		if req.Method == http.MethodPatch && strings.HasPrefix(req.Path, "/my/clearances/") {
			paths = append(paths, req.Path)
		}
		if req.Path == "/clearances/bulk.json" {
			t.Errorf("rescreen must not use the bulk endpoint, got %s %s", req.Method, req.Path)
		}
		if req.Method == http.MethodPatch && strings.HasPrefix(req.Path, "/clearances/") {
			t.Errorf("rescreen must not use the queue endpoint, got %s %s", req.Method, req.Path)
		}
	}
	return paths
}

func TestScreenerRescreenChangesOneScreenedSender(t *testing.T) {
	server, recorded := screenerServer(t)

	resp, err := runScreener(t, server, "rescreen", "deny", "91")
	if err != nil {
		t.Fatalf("rescreen failed: %v; requests=%+v", err, recorded.snapshot())
	}

	results := decodeScreenerData[[]rescreenResult](t, resp.Data)
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	result := results[0]
	if result.ID != 91 || result.Status != "denied" || result.Name != "Glenn" || result.Email != "glenn@example.com" || !result.Changed || result.Error != "" {
		t.Errorf("result = %+v", result)
	}
	if resp.Summary != "1 sender rescreened" {
		t.Errorf("summary = %q", resp.Summary)
	}

	requests := recorded.snapshot()
	if len(requests) != 2 ||
		requests[0].Method != http.MethodGet || requests[0].Path != "/my/clearances.json" ||
		requests[1].Method != http.MethodPatch || requests[1].Path != "/my/clearances/91.json" {
		t.Fatalf("requests = %+v", requests)
	}
	var body map[string]any
	_ = json.Unmarshal(requests[1].Body, &body)
	if body["status"] != "denied" {
		t.Errorf("body = %+v", body)
	}
	for _, key := range []string{"spam", "designation_box_id", "mark_topics_as_seen"} {
		if _, sent := body[key]; sent {
			t.Errorf("rescreen sends status alone, but body carried %s: %+v", key, body)
		}
	}
}

// Several IDs each go through the decided-sender endpoint on their own — never
// the bulk endpoint — and answers stay in the order the IDs were given.
func TestScreenerRescreenChangesSeveralSendersOneByOne(t *testing.T) {
	server, recorded := screenerServer(t)

	resp, err := runScreener(t, server, "rescreen", "deny", "91", "93")
	if err != nil {
		t.Fatalf("rescreen failed: %v; requests=%+v", err, recorded.snapshot())
	}

	results := decodeScreenerData[[]rescreenResult](t, resp.Data)
	if len(results) != 2 || !results[0].Changed || !results[1].Changed {
		t.Fatalf("results = %+v", results)
	}
	if results[0].ID != 91 || results[0].Name != "Glenn" || results[0].Status != "denied" {
		t.Errorf("results[0] = %+v", results[0])
	}
	if results[1].ID != 93 || results[1].Name != "Priya Natarajan" || results[1].Status != "denied" {
		t.Errorf("results[1] = %+v", results[1])
	}
	if resp.Summary != "2 senders rescreened" {
		t.Errorf("summary = %q", resp.Summary)
	}

	if paths := rescreenPatches(t, recorded.snapshot()); len(paths) != 2 ||
		paths[0] != "/my/clearances/91.json" || paths[1] != "/my/clearances/93.json" {
		t.Errorf("patches = %+v", paths)
	}
}

// One refusal is reported against its ID and the IDs after it are still
// answered; the command exits non-zero and says which ID failed.
func TestScreenerRescreenKeepsGoingAfterAFailure(t *testing.T) {
	server, recorded := screenerServer(t)
	recorded.statuses[http.MethodPatch+" /my/clearances/92.json"] = http.StatusInternalServerError

	resp, err := runScreener(t, server, "rescreen", "approve", "91", "92", "93", "94")
	if err == nil {
		t.Fatal("expected one failed rescreen to fail the command")
	}
	if code := output.ExitCodeFor(err); code != output.ExitAPI {
		t.Errorf("exit code = %d, want %d", code, output.ExitAPI)
	}
	if !strings.Contains(err.Error(), "92") || !strings.Contains(err.Error(), "1 of 4") {
		t.Errorf("error = %q", err.Error())
	}

	results := decodeScreenerData[[]rescreenResult](t, resp.Data)
	if len(results) != 4 {
		t.Fatalf("results = %+v", results)
	}
	if results[0].ID != 91 || results[0].Changed || results[0].Status != "approved" {
		t.Errorf("results[0] = %+v, want 91 reported unchanged", results[0])
	}
	if results[1].ID != 92 || results[1].Changed || results[1].Error == "" || results[1].Name != "Spammer" {
		t.Errorf("results[1] = %+v, want the failed 92 with its sender and reason", results[1])
	}
	if results[2].ID != 93 || results[2].Changed {
		t.Errorf("results[2] = %+v, want 93 still processed", results[2])
	}
	if results[3].ID != 94 || !results[3].Changed || results[3].Status != "approved" {
		t.Errorf("results[3] = %+v, want 94 rescreened after the failure", results[3])
	}
	if !strings.Contains(resp.Summary, "1 failed") {
		t.Errorf("summary = %q", resp.Summary)
	}

	if paths := rescreenPatches(t, recorded.snapshot()); len(paths) != 2 ||
		paths[0] != "/my/clearances/92.json" || paths[1] != "/my/clearances/94.json" {
		t.Errorf("patches = %+v, want 92 attempted and 94 still attempted after it", paths)
	}
}

// A sender already in the requested state is reported unchanged without a
// write, never as an update.
func TestScreenerRescreenReportsTheSameDecisionAsUnchanged(t *testing.T) {
	server, recorded := screenerServer(t)

	resp, err := runScreener(t, server, "rescreen", "approve", "91")
	if err != nil {
		t.Fatalf("rescreen failed: %v", err)
	}

	results := decodeScreenerData[[]rescreenResult](t, resp.Data)
	if len(results) != 1 || results[0].Changed || results[0].Status != "approved" || results[0].Error != "" {
		t.Errorf("results = %+v", results)
	}
	if resp.Summary != "1 unchanged" {
		t.Errorf("summary = %q", resp.Summary)
	}

	for _, req := range recorded.snapshot() {
		if req.Method == http.MethodPatch {
			t.Fatalf("unchanged decision sent no PATCH, got %+v", req)
		}
	}
}

// The rescreen endpoint answers with a status alone. The queue-only flags are
// refused with the command that does support them, before any request goes out.
func TestScreenerRescreenRejectsQueueOnlyFlags(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"rescreen", "approve", "91", "--box", "The Feed"}, "hey screener approve"},
		{[]string{"rescreen", "deny", "91", "--seen"}, "hey screener approve"},
		{[]string{"rescreen", "approve", "91", "--spam"}, "hey screener deny"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			server, recorded := screenerServer(t)

			_, err := runScreener(t, server, tc.args...)
			if err == nil {
				t.Fatal("expected the queue-only flag to be refused")
			}
			var apiErr *apierr.Error
			if !errors.As(err, &apiErr) || apiErr.Code != apierr.CodeUsage {
				t.Fatalf("error = %v, want a usage error", err)
			}
			if !strings.Contains(apiErr.Hint, tc.want) {
				t.Errorf("hint = %q, want it to name %q", apiErr.Hint, tc.want)
			}
			if requests := recorded.snapshot(); len(requests) != 0 {
				t.Errorf("expected no request, got %+v", requests)
			}
		})
	}
}

func TestScreenerRescreenRejectsBadDecisionAndIDs(t *testing.T) {
	for _, args := range [][]string{
		{"rescreen", "maybe", "91"},
		{"rescreen", "deny", "abc"},
	} {
		server, recorded := screenerServer(t)

		if _, err := runScreener(t, server, args...); err == nil {
			t.Errorf("expected %v to be refused", args)
		}
		if requests := recorded.snapshot(); len(requests) != 0 {
			t.Errorf("expected no request for %v, got %+v", args, requests)
		}
	}
}

// --dry-run reads the decision back and reports what would change, but writes
// nothing.
func TestScreenerRescreenDryRunWritesNothing(t *testing.T) {
	server, recorded := screenerServer(t)

	resp, err := runScreener(t, server, "rescreen", "deny", "91", "--dry-run")
	if err != nil {
		t.Fatalf("dry run failed: %v", err)
	}

	results := decodeScreenerData[[]rescreenResult](t, resp.Data)
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	if results[0].Changed || !results[0].WouldChange || results[0].Status != "approved" {
		t.Errorf("results[0] = %+v, want the current status with would_change", results[0])
	}
	if resp.Summary != "Dry run: 1 would change" || resp.Notice != "Dry run — nothing was changed" {
		t.Errorf("summary = %q notice = %q", resp.Summary, resp.Notice)
	}

	for _, req := range recorded.snapshot() {
		if req.Method == http.MethodPatch {
			t.Fatalf("dry run sent no PATCH, got %+v", req)
		}
	}
}

// An ID the decided list does not hold is a failure for that ID: it may still
// be waiting in the queue, which approve/deny answer.
func TestScreenerRescreenFailsForAnIDThatWasNeverScreened(t *testing.T) {
	server, recorded := screenerServer(t)

	_, err := runScreener(t, server, "rescreen", "deny", "99")
	if err == nil {
		t.Fatal("expected an unknown screened ID to fail")
	}
	if code := output.ExitCodeFor(err); code != output.ExitNotFound {
		t.Errorf("exit code = %d, want %d", code, output.ExitNotFound)
	}
	if paths := rescreenPatches(t, recorded.snapshot()); len(paths) != 0 {
		t.Errorf("patches = %+v, want none for an unknown ID", paths)
	}
}

func TestScreenerRescreenHumanLines(t *testing.T) {
	t.Run("changed", func(t *testing.T) {
		server, _ := screenerServer(t)

		out, err := runScreenerRaw(t, server, "--styled", "rescreen", "deny", "91")
		if err != nil {
			t.Fatalf("rescreen failed: %v", err)
		}
		if !strings.Contains(out, "Glenn") || !strings.Contains(out, "denied") {
			t.Errorf("output = %q", out)
		}
	})

	t.Run("unchanged", func(t *testing.T) {
		server, _ := screenerServer(t)

		out, err := runScreenerRaw(t, server, "--styled", "rescreen", "approve", "91")
		if err != nil {
			t.Fatalf("rescreen failed: %v", err)
		}
		if !strings.Contains(out, "already approved") {
			t.Errorf("output = %q", out)
		}
	})

	t.Run("dry run", func(t *testing.T) {
		server, recorded := screenerServer(t)

		out, err := runScreenerRaw(t, server, "--styled", "rescreen", "deny", "91", "--dry-run")
		if err != nil {
			t.Fatalf("dry run failed: %v", err)
		}
		if !strings.Contains(out, "Would deny Glenn") {
			t.Errorf("output = %q", out)
		}
		for _, req := range recorded.snapshot() {
			if req.Method == http.MethodPatch {
				t.Fatalf("dry run sent a PATCH: %+v", req)
			}
		}
	})

	t.Run("failure names the sender", func(t *testing.T) {
		server, recorded := screenerServer(t)
		recorded.statuses[http.MethodPatch+" /my/clearances/92.json"] = http.StatusInternalServerError

		out, err := runScreenerRaw(t, server, "--styled", "rescreen", "approve", "92")
		if err == nil {
			t.Fatal("expected the refused rescreen to fail")
		}
		if !strings.Contains(out, "Spammer: not rescreened") {
			t.Errorf("output = %q", out)
		}
	})
}
