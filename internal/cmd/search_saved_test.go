package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/basecamp/hey-cli/internal/apierr"
	"github.com/basecamp/hey-cli/internal/output"
)

// executeSavedSearchCmd drives a search subtree command in a fresh state dir.
// With authenticated false it leaves HEY_TOKEN unset, so purely local
// subcommands (save/list/delete) never talk to a server; account scopes the
// run through HEY_ACCOUNT_ID when non-empty. It returns the state root and the
// decoded JSON envelope.
func executeSavedSearchCmd(t *testing.T, server *httptest.Server, authenticated bool, account string, args ...string) (output.Response, string, error) {
	t.Helper()
	return executeSavedSearchCmdInDir(t, server, t.TempDir(), authenticated, account, args...)
}

func executeSavedSearchCmdInDir(t *testing.T, server *httptest.Server, tmpDir string, authenticated bool, account string, args ...string) (output.Response, string, error) {
	t.Helper()
	if authenticated {
		t.Setenv("HEY_TOKEN", "test-token")
		t.Setenv("HEY_NO_KEYRING", "1")
	} else {
		t.Setenv("HEY_NO_KEYRING", "1")
	}
	t.Setenv("HEY_BASE_URL", "")
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	t.Setenv("XDG_STATE_HOME", tmpDir)
	t.Setenv("XDG_CACHE_HOME", tmpDir)
	if account != "" {
		t.Setenv("HEY_ACCOUNT_ID", account)
	}

	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	fullArgs := []string{"search", "--json"}
	if server != nil {
		fullArgs = append(fullArgs, "--base-url", server.URL)
	}
	root.SetArgs(append(fullArgs, args...))

	err := root.Execute()
	var resp output.Response
	if buf.Len() > 0 {
		_ = json.Unmarshal(buf.Bytes(), &resp)
	}
	return resp, tmpDir, err
}

func decodeSavedSearches(t *testing.T, data any) []savedSearchListItem {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var items []savedSearchListItem
	if err := json.Unmarshal(encoded, &items); err != nil {
		t.Fatal(err)
	}
	return items
}

func TestSearchSaveStoresConditionsSourcesAndTime(t *testing.T) {
	server, recorded := searchServer(t)
	stateDir := t.TempDir()
	_, _, err := executeSavedSearchCmdInDir(t, server, stateDir, true, "",
		"save", "planning", "--name", "plans",
		"--required", "budget",
		"--from", "jane@example.com",
		"--date", "last_30_days",
	)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.requests != 0 {
		t.Errorf("save made %d requests, want 0", recorded.requests)
	}

	resp, _, err := executeSavedSearchCmdInDir(t, server, stateDir, true, "", "list")
	if err != nil {
		t.Fatal(err)
	}
	items := decodeSavedSearches(t, resp.Data)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1: %+v", len(items), resp.Data)
	}
	item := items[0]
	if item.Name != "plans" || item.Query != "planning" || item.Required != "budget" ||
		item.From != "jane@example.com" || item.Date != "last_30_days" {
		t.Errorf("unexpected saved item: %+v", item)
	}
	if item.CreatedAt.IsZero() {
		t.Error("created_at was not recorded")
	}
	if item.Sources["query"] != "argument" || item.Sources["required"] != "--required" ||
		item.Sources["from"] != "--from" || item.Sources["date"] != "--date" {
		t.Errorf("sources = %+v", item.Sources)
	}
}

func TestSearchSaveValidatesLikeSearch(t *testing.T) {
	server, recorded := searchServer(t)
	tests := [][]string{
		{"save", "--from", "jane@example.com"}, // missing --name
		{"save", "--name", "empty"},            // no criteria
		{"save", "planning", "--name", "bad-date", "--date", "yesterday"},
		{"save", "planning", "--name", "bad-box", "--in", "set-aside"},
		{"save", "planning", "--name", "bad-kind", "--attachment", "pdf"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, _, err := executeSavedSearchCmd(t, server, true, "", args...)
			var cliErr *apierr.Error
			if !errors.As(err, &cliErr) || cliErr.Code != "usage" {
				t.Fatalf("error = %v, want usage error", err)
			}
		})
	}
	if recorded.requests != 0 {
		t.Errorf("requests = %d, want 0", recorded.requests)
	}
}

func TestSearchSaveDuplicateNeedsForce(t *testing.T) {
	server, _ := searchServer(t)
	stateDir := t.TempDir()
	run := func(args ...string) error {
		_, _, err := executeSavedSearchCmdInDir(t, server, stateDir, true, "", args...)
		return err
	}

	if err := run("save", "--name", "jane", "--from", "jane@example.com"); err != nil {
		t.Fatal(err)
	}
	err := run("save", "--name", "jane", "--from", "mike@example.org")
	var cliErr *apierr.Error
	if !errors.As(err, &cliErr) || cliErr.Code != "usage" || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate error = %v, want usage error naming the conflict", err)
	}

	// The saved value is untouched by the refused save.
	resp, _, err := executeSavedSearchCmdInDir(t, server, stateDir, true, "", "list")
	if err != nil {
		t.Fatal(err)
	}
	if items := decodeSavedSearches(t, resp.Data); items[0].From != "jane@example.com" {
		t.Errorf("from = %q, want the original kept", items[0].From)
	}

	if err := run("save", "--name", "jane", "--from", "mike@example.org", "--force"); err != nil {
		t.Fatal(err)
	}
	resp, _, err = executeSavedSearchCmdInDir(t, server, stateDir, true, "", "list")
	if err != nil {
		t.Fatal(err)
	}
	if items := decodeSavedSearches(t, resp.Data); len(items) != 1 || items[0].From != "mike@example.org" {
		t.Errorf("after force items = %+v, want one mike record", items)
	}
}

func TestSearchDeleteRemovesAndMissingIsNotFound(t *testing.T) {
	server, _ := searchServer(t)
	stateDir := t.TempDir()
	if _, _, err := executeSavedSearchCmdInDir(t, server, stateDir, true, "", "save", "--name", "jane", "--from", "jane@example.com"); err != nil {
		t.Fatal(err)
	}

	_, _, err := executeSavedSearchCmdInDir(t, server, stateDir, true, "", "delete", "ghost")
	var cliErr *apierr.Error
	if !errors.As(err, &cliErr) || cliErr.Code != apierr.CodeNotFound {
		t.Fatalf("delete missing error = %v, want not_found", err)
	}

	if _, _, err := executeSavedSearchCmdInDir(t, server, stateDir, true, "", "delete", "jane"); err != nil {
		t.Fatal(err)
	}
	resp, _, err := executeSavedSearchCmdInDir(t, server, stateDir, true, "", "list")
	if err != nil {
		t.Fatal(err)
	}
	if items := decodeSavedSearches(t, resp.Data); len(items) != 0 {
		t.Errorf("items = %+v, want empty", items)
	}
}

func TestSearchRunLoadsSavedConditions(t *testing.T) {
	server, recorded := searchServer(t)
	stateDir := t.TempDir()
	if _, _, err := executeSavedSearchCmdInDir(t, server, stateDir, true, "",
		"save", "--name", "jane", "--from", "jane@example.com", "--subject", "invoice"); err != nil {
		t.Fatal(err)
	}

	if _, _, err := executeSavedSearchCmdInDir(t, server, stateDir, true, "", "run", "jane"); err != nil {
		t.Fatal(err)
	}
	if recorded.requests != 1 || len(recorded.queries) != 1 {
		t.Fatalf("requests = %d queries = %d, want 1", recorded.requests, len(recorded.queries))
	}
	query := recorded.queries[0]
	if query.Get("refine[from]") != "jane@example.com" || query.Get("refine[subject]") != "invoice" {
		t.Errorf("saved conditions not sent: %v", query)
	}
	if query.Get("q") != "" {
		t.Errorf("q = %q, want empty", query.Get("q"))
	}
}

func TestSearchRunCommandLineConditionsOverrideSaved(t *testing.T) {
	server, recorded := searchServer(t)
	stateDir := t.TempDir()
	if _, _, err := executeSavedSearchCmdInDir(t, server, stateDir, true, "",
		"save", "--name", "jane",
		"--from", "jane@example.com",
		"--date", "last_30_days",
		"--subject", "invoice"); err != nil {
		t.Fatal(err)
	}

	// Override --from and --date, keep saved --subject, override free text positionally.
	if _, _, err := executeSavedSearchCmdInDir(t, server, stateDir, true, "",
		"run", "jane", "overdue",
		"--from", "mike@example.org",
		"--date", "last_7_days"); err != nil {
		t.Fatal(err)
	}
	sent := recorded.queries[len(recorded.queries)-1]
	wants := map[string]string{
		"q":               "overdue",
		"refine[from]":    "mike@example.org",
		"refine[date]":    "last_7_days",
		"refine[subject]": "invoice",
	}
	for key, want := range wants {
		if got := sent.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	// Running plain again keeps the saved values — overrides never rewrite the file.
	if _, _, err := executeSavedSearchCmdInDir(t, server, stateDir, true, "", "run", "jane"); err != nil {
		t.Fatal(err)
	}
	again := recorded.queries[len(recorded.queries)-1]
	if again.Get("refine[from]") != "jane@example.com" || again.Get("q") != "" {
		t.Errorf("saved record changed by override run: %v", again)
	}
}

func TestSearchRunMissingAndCorruptFailClearly(t *testing.T) {
	server, recorded := searchServer(t)

	_, _, err := executeSavedSearchCmd(t, server, true, "", "run", "ghost")
	var cliErr *apierr.Error
	if !errors.As(err, &cliErr) || cliErr.Code != apierr.CodeNotFound {
		t.Fatalf("missing error = %v, want not_found", err)
	}
	if recorded.requests != 0 {
		t.Errorf("missing run made %d requests, want 0", recorded.requests)
	}

	// A valid save followed by a hand-corrupted scope file must be named, not
	// read as an empty query.
	stateDir := t.TempDir()
	if _, _, err := executeSavedSearchCmdInDir(t, server, stateDir, true, "", "save", "--name", "jane", "--from", "jane@example.com"); err != nil {
		t.Fatal(err)
	}
	scopeFile := filepath.Join(stateDir, "hey-cli", "saved-searches", "all.json")
	if err := os.WriteFile(scopeFile, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = executeSavedSearchCmdInDir(t, server, stateDir, true, "", "run", "jane")
	if err == nil || (!strings.Contains(err.Error(), "corrupt") && !strings.Contains(err.Error(), "corrupted")) {
		t.Fatalf("corrupt error = %v, want an error naming the corrupted file", err)
	}
	if recorded.requests != 0 {
		t.Errorf("corrupt run made %d requests, want 0", recorded.requests)
	}
}

func TestSearchRunValidatesOverridesBeforeRequest(t *testing.T) {
	server, recorded := searchServer(t)
	stateDir := t.TempDir()
	if _, _, err := executeSavedSearchCmdInDir(t, server, stateDir, true, "",
		"save", "--name", "jane", "--from", "jane@example.com"); err != nil {
		t.Fatal(err)
	}
	_, _, err := executeSavedSearchCmdInDir(t, server, stateDir, true, "", "run", "jane", "--in", "set-aside")
	var cliErr *apierr.Error
	if !errors.As(err, &cliErr) || cliErr.Code != "usage" {
		t.Fatalf("error = %v, want usage error", err)
	}
	if recorded.requests != 0 {
		t.Errorf("requests = %d, want 0", recorded.requests)
	}
}

func TestSavedSearchesAreIsolatedByAccount(t *testing.T) {
	// Local commands need no login: with no token the pre-run leaves the
	// HEY_ACCOUNT_ID scope on cfg without selecting the account over HTTP.
	stateDir := t.TempDir()
	t.Setenv("HEY_NO_KEYRING", "1")
	t.Setenv("HEY_BASE_URL", "")
	t.Setenv("XDG_CONFIG_HOME", stateDir)
	t.Setenv("XDG_STATE_HOME", stateDir)
	t.Setenv("XDG_CACHE_HOME", stateDir)

	run := func(account string, args ...string) (output.Response, error) {
		t.Setenv("HEY_ACCOUNT_ID", account)
		var buf bytes.Buffer
		root := newRootCmd()
		root.SetOut(&buf)
		root.SetErr(&buf)
		root.SetArgs(append([]string{"search", "--json"}, args...))
		err := root.Execute()
		var resp output.Response
		if buf.Len() > 0 {
			_ = json.Unmarshal(buf.Bytes(), &resp)
		}
		return resp, err
	}

	if _, err := run("101", "save", "--name", "shared", "--from", "work@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("202", "save", "--name", "shared", "--from", "personal@example.com"); err != nil {
		t.Fatal(err)
	}

	for account, want := range map[string]string{"101": "work@example.com", "202": "personal@example.com"} {
		path := filepath.Join(stateDir, "hey-cli", "saved-searches", account+".json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("account %s scope file missing: %v", account, err)
		}
		if !strings.Contains(string(data), want) {
			t.Errorf("account %s file %s does not contain %q", account, path, want)
		}
	}

	resp, err := run("101", "list")
	if err != nil {
		t.Fatal(err)
	}
	items := decodeSavedSearches(t, resp.Data)
	if len(items) != 1 || items[0].From != "work@example.com" {
		t.Errorf("scoped list = %+v, want only the 101 record", items)
	}

	resp, err = run("202", "list")
	if err != nil {
		t.Fatal(err)
	}
	if items := decodeSavedSearches(t, resp.Data); len(items) != 1 || items[0].From != "personal@example.com" {
		t.Errorf("scoped list = %+v, want only the 202 record", items)
	}
}

func TestSearchSavedSubcommandHelp(t *testing.T) {
	root := newRootCmd()
	find := func(parts ...string) *cobra.Command {
		command, _, err := root.Find(parts)
		if err != nil {
			t.Fatal(err)
		}
		return command
	}

	save := find("search", "save")
	if save.Short == "" || save.Example == "" || save.Annotations["agent_notes"] == "" {
		t.Errorf("save help is incomplete: %+v", save)
	}
	for _, name := range []string{"name", "force", "required", "any", "none", "exact", "from", "to", "subject", "date", "in", "label", "attachment"} {
		if save.Flags().Lookup(name) == nil {
			t.Errorf("save is missing --%s", name)
		}
	}
	if save.Flags().Lookup("page") != nil || save.Flags().Lookup("all") != nil {
		t.Errorf("save must not carry execution flags page/all")
	}

	run := find("search", "run")
	if run.Short == "" || !strings.Contains(run.Long, "overrides") {
		t.Errorf("run help does not explain overrides: %q", run.Long)
	}
	for _, name := range []string{"required", "from", "date", "page", "all"} {
		if run.Flags().Lookup(name) == nil {
			t.Errorf("run is missing --%s", name)
		}
	}

	list := find("search", "list")
	if list.Short == "" {
		t.Error("list has no short description")
	}
	deleteCommand := find("search", "delete")
	if deleteCommand.Short == "" {
		t.Error("delete has no short description")
	}
}
