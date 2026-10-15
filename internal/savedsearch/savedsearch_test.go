package savedsearch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	stateDir := t.TempDir()
	return New(stateDir), stateDir
}

func sampleQuery(name string) Query {
	return Query{
		Name:      name,
		CreatedAt: time.Date(2026, 10, 15, 9, 30, 0, 0, time.UTC),
		Sources: map[string]string{
			"query": "argument",
			"from":  "--from",
		},
		Conditions: Conditions{
			Query: "planning",
			From:  "jane@example.com",
			Date:  "last_30_days",
		},
	}
}

func TestSaveAndGetRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	want := sampleQuery("invoices")
	if err := store.Save("all", want, false); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get("all", "invoices")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSaveRefusesDuplicateWithoutForce(t *testing.T) {
	store, _ := newTestStore(t)
	first := sampleQuery("invoices")
	first.From = "jane@example.com"
	if err := store.Save("all", first, false); err != nil {
		t.Fatal(err)
	}

	second := sampleQuery("invoices")
	second.From = "mike@example.org"
	err := store.Save("all", second, false)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("error = %v, want ErrExists", err)
	}

	got, err := store.Get("all", "invoices")
	if err != nil {
		t.Fatal(err)
	}
	if got.From != "jane@example.com" {
		t.Fatalf("from = %q, want the original preserved", got.From)
	}
}

func TestSaveForceOverwritesAndKeepsUnknownFields(t *testing.T) {
	store, _ := newTestStore(t)
	if err := store.Save("all", sampleQuery("invoices"), false); err != nil {
		t.Fatal(err)
	}

	// A newer hey-cli's field on the record, plus an unknown top-level key.
	path, err := store.Path("all")
	if err != nil {
		t.Fatal(err)
	}
	document := map[string]json.RawMessage{}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	var queries map[string]json.RawMessage
	if err := json.Unmarshal(document["queries"], &queries); err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(queries["invoices"], &record); err != nil {
		t.Fatal(err)
	}
	record["future_refinement"] = json.RawMessage(`{"kind":"semantic"}`)
	queries["invoices"], _ = json.Marshal(record)
	document["queries"], _ = json.Marshal(queries)
	document["future_setting"] = json.RawMessage(`true`)
	encoded, _ := json.MarshalIndent(document, "", "  ")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	overwrite := sampleQuery("invoices")
	overwrite.From = "mike@example.org"
	if err := store.Save("all", overwrite, true); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get("all", "invoices")
	if err != nil {
		t.Fatal(err)
	}
	if got.From != "mike@example.org" {
		t.Errorf("from = %q, want overwrite", got.From)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `"future_refinement"`) {
		t.Errorf("overwrite dropped the record's unknown field:\n%s", text)
	}
	if !strings.Contains(text, `"future_setting"`) {
		t.Errorf("rewrite dropped the unknown top-level key:\n%s", text)
	}
}

func TestGetMissingFileAndNameAreNotFound(t *testing.T) {
	store, _ := newTestStore(t)
	for _, name := range []string{"missing", "invoices"} {
		if name != "missing" {
			if err := store.Save("all", sampleQuery("other"), false); err != nil {
				t.Fatal(err)
			}
		}
		_, err := store.Get("all", name)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) error = %v, want ErrNotFound", name, err)
		}
	}
}

func TestListIsSortedAndScoped(t *testing.T) {
	store, _ := newTestStore(t)
	for _, name := range []string{"zebra", "Alpha", "middle"} {
		q := sampleQuery(name)
		if err := store.Save("all", q, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Save("101", sampleQuery("work-only"), false); err != nil {
		t.Fatal(err)
	}

	got, err := store.List("all")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, query := range got {
		names = append(names, query.Name)
	}
	want := []string{"Alpha", "middle", "zebra"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("names = %v, want %v", names, want)
	}

	other, err := store.List("101")
	if err != nil || len(other) != 1 || other[0].Name != "work-only" {
		t.Errorf("scoped list = %+v, %v", other, err)
	}

	empty, err := store.List("999")
	if err != nil || len(empty) != 0 {
		t.Errorf("missing scope list = %+v, %v, want empty", empty, err)
	}
}

func TestDeleteRemovesAndReportsMissing(t *testing.T) {
	store, _ := newTestStore(t)
	if err := store.Save("all", sampleQuery("a"), false); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("all", sampleQuery("b"), false); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete("all", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("all", "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted Get error = %v, want ErrNotFound", err)
	}
	if err := store.Delete("all", "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete error = %v, want ErrNotFound", err)
	}
	if err := store.Delete("all", "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing name error = %v, want ErrNotFound", err)
	}

	// Deleting the last query removes the scope file entirely.
	if err := store.Delete("all", "b"); err != nil {
		t.Fatal(err)
	}
	path, _ := store.Path("all")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("scope file still exists after last delete: %s", path)
	}
}

func TestAccountsAreIsolated(t *testing.T) {
	store, _ := newTestStore(t)
	if err := store.Save("101", sampleQuery("shared-name"), false); err != nil {
		t.Fatal(err)
	}
	other := sampleQuery("shared-name")
	other.From = "work@example.com"
	if err := store.Save("202", other, false); err != nil {
		t.Fatal(err)
	}

	first, err := store.Get("101", "shared-name")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Get("202", "shared-name")
	if err != nil {
		t.Fatal(err)
	}
	if first.From == second.From {
		t.Errorf("accounts share state: both from = %q", first.From)
	}

	paths := map[string]bool{}
	for _, account := range []string{"101", "202"} {
		path, err := store.Path(account)
		if err != nil {
			t.Fatal(err)
		}
		if paths[path] {
			t.Fatalf("accounts share file %s", path)
		}
		paths[path] = true
		if _, err := os.Stat(path); err != nil {
			t.Errorf("scope file missing: %v", err)
		}
	}
}

func TestWritesAreAtomicAndPrivate(t *testing.T) {
	store, stateDir := newTestStore(t)
	if err := store.Save("all", sampleQuery("a"), false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		q := sampleQuery("q" + string(rune('b'+i)))
		if err := store.Save("all", q, false); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(stateDir, dirName))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}

	path, _ := store.Path("all")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Errorf("directory mode = %o, want 0700", mode)
	}

	// The finished file is always complete JSON.
	var document map[string]json.RawMessage
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("file is not valid JSON: %v", err)
	}
}

func TestCorruptFileIsRejected(t *testing.T) {
	store, stateDir := newTestStore(t)
	dir := filepath.Join(stateDir, dirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "all.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Get("all", "x"); err == nil || !strings.Contains(err.Error(), "corrupted") {
		t.Errorf("Get error = %v, want corruption error", err)
	}
	if _, err := store.List("all"); err == nil || !strings.Contains(err.Error(), "corrupted") {
		t.Errorf("List error = %v, want corruption error", err)
	}
	if err := store.Save("all", sampleQuery("x"), false); err == nil || !strings.Contains(err.Error(), "corrupted") {
		t.Errorf("Save error = %v, want corruption error instead of overwriting", err)
	}
	if err := store.Delete("all", "x"); err == nil || !strings.Contains(err.Error(), "corrupted") {
		t.Errorf("Delete error = %v, want corruption error", err)
	}
}

func TestUnsafeScopeIsRejected(t *testing.T) {
	store, _ := newTestStore(t)
	for _, account := range []string{"", "../escape", "a/b", "a:b"} {
		if err := store.Save(account, sampleQuery("x"), false); err == nil {
			t.Errorf("Save(%q) succeeded, want error", account)
		}
	}
}

func TestEmptyStateDirIsRejected(t *testing.T) {
	if err := New("").Save("all", sampleQuery("x"), false); err == nil {
		t.Fatal("save with empty state dir succeeded")
	}
}
