package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	exportNotesTag  = `<action-text-attachment sgid="sgid-notes" content-type="text/plain" url="/rails/active_storage/blobs/notes.txt" filename="notes.txt" filesize="14"></action-text-attachment>`
	exportBrokenTag = `<action-text-attachment sgid="sgid-broken" content-type="application/pdf" url="/rails/active_storage/blobs/broken.pdf" filename="broken.pdf" filesize="7"></action-text-attachment>`
	exportEvilTag   = `<action-text-attachment sgid="sgid-evil" content-type="text/plain" url="/rails/active_storage/blobs/evil.txt" filename="../../evil.txt" filesize="13"></action-text-attachment>`
)

// The thread carries a broken download, a traversal-named file, a twice-embedded
// image and a plain attachment: success, failure and collision paths in one thread.
func attachmentExportServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/topics/42/entries.json":
			_, _ = w.Write([]byte(`[{"id":101,"kind":"message"},{"id":102,"kind":"message"}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/messages/101.json":
			// A rendered inbound HTML body embeds the same logo twice, then a direct file.
			logoTag := `<action-text-attachment sgid="sgid-logo" content-type="image/png" url="/rails/active_storage/blobs/logo.png" filename="logo.png" filesize="10"></action-text-attachment>`
			content := renderedEmbeddedHTMLFigure(t, logoTag+logoTag) + exportNotesTag
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 101, "content": content})
		case r.Method == http.MethodGet && r.URL.Path == "/messages/102.json":
			content := exportBrokenTag + exportEvilTag
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 102, "content": content})
		case r.Method == http.MethodGet && r.URL.Path == "/rails/active_storage/blobs/logo.png":
			_, _ = w.Write([]byte("logo bytes"))
		case r.Method == http.MethodGet && r.URL.Path == "/rails/active_storage/blobs/notes.txt":
			_, _ = w.Write([]byte("notes contents"))
		case r.Method == http.MethodGet && r.URL.Path == "/rails/active_storage/blobs/evil.txt":
			_, _ = w.Write([]byte("evil contents"))
		case r.Method == http.MethodGet && r.URL.Path == "/rails/active_storage/blobs/broken.pdf":
			http.Error(w, "download failed", http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func decodeExportReport(t *testing.T, stdout string) attachmentExportReport {
	t.Helper()
	var response struct {
		OK   bool                   `json:"ok"`
		Data attachmentExportReport `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &response); err != nil {
		t.Fatalf("decode response: %v\n%s", err, stdout)
	}
	if !response.OK {
		t.Fatalf("response not ok: %s", stdout)
	}
	return response.Data
}

func assertFileText(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Errorf("%s content = %q, want %q", path, data, want)
	}
}

func readExportManifest(t *testing.T, dir string) attachmentExportManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest attachmentExportManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode manifest: %v\n%s", err, data)
	}
	return manifest
}

func TestAttachmentExportDownloadsThreadWithManifestAndChecksums(t *testing.T) {
	server := attachmentExportServer(t)
	dir := filepath.Join(t.TempDir(), "export")

	stdout, err := runAttachmentCommand(t, server, "attachment", "export", "42", "--output", dir)
	if err != nil {
		t.Fatal(err)
	}
	report := decodeExportReport(t, stdout)
	if report.ThreadID != 42 || report.DryRun || report.Succeeded != 4 || report.Failed != 1 {
		t.Errorf("report = %+v", report)
	}
	if len(report.Failures) != 1 || report.Failures[0].ID != "102:1" || report.Failures[0].Error == "" {
		t.Errorf("failures = %+v, want the broken blob with a reason", report.Failures)
	}

	manifest := readExportManifest(t, dir)
	if manifest.ThreadID != 42 || manifest.Succeeded != 4 || manifest.Failed != 1 {
		t.Errorf("manifest counts = %+v", manifest)
	}
	if _, err := time.Parse(time.RFC3339, manifest.ExportedAt); err != nil {
		t.Errorf("exported_at = %q: %v", manifest.ExportedAt, err)
	}
	if len(manifest.Files) != 4 || len(manifest.Failures) != 1 {
		t.Fatalf("manifest files = %+v failures = %+v", manifest.Files, manifest.Failures)
	}

	byID := make(map[string]exportedAttachmentFile, len(manifest.Files))
	names := make(map[string]bool)
	for _, file := range manifest.Files {
		byID[file.ID] = file
		names[filepath.Base(file.Path)] = true

		data, readErr := os.ReadFile(file.Path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		digest := sha256.Sum256(data)
		if file.SHA256 != hex.EncodeToString(digest[:]) {
			t.Errorf("%s manifest checksum = %q, want %x", file.Filename, file.SHA256, digest)
		}
		if file.ByteSize != int64(len(data)) {
			t.Errorf("%s byte_size = %d, want %d", file.Filename, file.ByteSize, len(data))
		}
		if filepath.Dir(file.Path) != dir {
			t.Errorf("%s landed outside the export directory: %s", file.Filename, file.Path)
		}
	}
	for _, name := range []string{"evil.txt", "logo.png", "logo-2.png", "notes.txt"} {
		if !names[name] {
			t.Errorf("manifest is missing %s; got %v", name, names)
		}
	}

	// The twice-embedded logo is one logical file represented twice: both copies
	// download and get distinct IDs and disk paths with matching content hashes.
	logoIDs := 0
	var logos []exportedAttachmentFile
	for id, file := range byID {
		if strings.Contains(id, ":e-") && file.Filename == "logo.png" {
			logoIDs++
			logos = append(logos, file)
		}
	}
	if logoIDs != 2 || logos[0].SHA256 != logos[1].SHA256 || logos[0].Path == logos[1].Path {
		t.Errorf("duplicate embedded logos = %+v, want two IDs with same hash", logos)
	}
	if !strings.HasSuffix(logos[0].ID, ".2") && !strings.HasSuffix(logos[1].ID, ".2") {
		t.Errorf("duplicate logos IDs = %q, %q, want an occurrence suffix", logos[0].ID, logos[1].ID)
	}

	assertFileText(t, filepath.Join(dir, "evil.txt"), "evil contents")
	assertFileText(t, filepath.Join(dir, "logo.png"), "logo bytes")
	assertFileText(t, filepath.Join(dir, "notes.txt"), "notes contents")
	if _, err := os.Stat(filepath.Join(dir, "broken.pdf")); !os.IsNotExist(err) {
		t.Errorf("failed download left broken.pdf behind: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".hey-") {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestAttachmentExportDryRunWritesNothingAndPlansReplacements(t *testing.T) {
	server := attachmentExportServer(t)
	dir := filepath.Join(t.TempDir(), "planned-export")

	stdout, err := runAttachmentCommand(t, server, "attachment", "export", "42", "--output", dir, "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dry run created the output directory: %v", err)
	}
	report := decodeExportReport(t, stdout)
	if !report.DryRun || report.Succeeded != 0 || report.Failed != 0 || len(report.Planned) != 5 {
		t.Errorf("dry-run report = %+v", report)
	}
	plannedNames := map[string]bool{}
	for _, plan := range report.Planned {
		plannedNames[filepath.Base(plan.Path)] = true
		if filepath.Dir(plan.Path) != dir {
			t.Errorf("planned path outside directory: %s", plan.Path)
		}
	}
	for _, name := range []string{"broken.pdf", "evil.txt", "logo.png", "logo-2.png", "notes.txt"} {
		if !plannedNames[name] {
			t.Errorf("dry-run plan missing %s: %v", name, plannedNames)
		}
	}

	// With a file already on disk, --dry-run --force marks it for replacement and
	// still writes nothing.
	populated := t.TempDir()
	if err := os.WriteFile(filepath.Join(populated, "logo.png"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, err = runAttachmentCommand(t, server, "attachment", "export", "42", "--output", populated, "--dry-run", "--force")
	if err != nil {
		t.Fatal(err)
	}
	report = decodeExportReport(t, stdout)
	replaced := 0
	for _, plan := range report.Planned {
		if plan.Replaces {
			replaced++
		}
	}
	if replaced != 1 {
		t.Errorf("replaces count = %d, want exactly logo.png", replaced)
	}
	assertFileText(t, filepath.Join(populated, "logo.png"), "old")
	entries, err := os.ReadDir(populated)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("dry run wrote files: %v", entries)
	}
}

func TestAttachmentExportWithoutForceKeepsExistingFiles(t *testing.T) {
	server := attachmentExportServer(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("old notes"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := runAttachmentCommand(t, server, "attachment", "export", "42", "--output", dir); err != nil {
		t.Fatal(err)
	}
	assertFileText(t, filepath.Join(dir, "notes.txt"), "old notes")
	assertFileText(t, filepath.Join(dir, "notes-2.txt"), "notes contents")
}

func TestAttachmentExportForceReplacesFilesAndManifest(t *testing.T) {
	server := attachmentExportServer(t)
	dir := t.TempDir()

	first, err := runAttachmentCommand(t, server, "attachment", "export", "42", "--output", dir)
	if err != nil {
		t.Fatal(err)
	}
	firstReport := decodeExportReport(t, first)
	firstManifest := readExportManifest(t, dir)

	second, err := runAttachmentCommand(t, server, "attachment", "export", "42", "--output", dir, "--force")
	if err != nil {
		t.Fatal(err)
	}
	secondReport := decodeExportReport(t, second)
	if secondReport.Succeeded != 4 || secondReport.Failed != 1 || secondReport.Manifest != firstReport.Manifest {
		t.Errorf("forced re-export report = %+v", secondReport)
	}
	secondManifest := readExportManifest(t, dir)
	if secondManifest.ExportedAt == "" || secondManifest.ExportedAt == firstManifest.ExportedAt {
		t.Errorf("manifest was not rewritten on forced export: %q vs %q", firstManifest.ExportedAt, secondManifest.ExportedAt)
	}
	names := map[string]bool{}
	for _, file := range secondManifest.Files {
		names[filepath.Base(file.Path)] = true
	}
	for _, name := range []string{"evil.txt", "logo.png", "logo-2.png", "notes.txt"} {
		if !names[name] {
			t.Errorf("forced export moved %s: %v", name, names)
		}
	}
	assertFileText(t, filepath.Join(dir, "logo.png"), "logo bytes")
	assertFileText(t, filepath.Join(dir, "notes.txt"), "notes contents")
}

func TestAttachmentExportForceDoesNotFollowSymlinksOrLeaveDirectory(t *testing.T) {
	server := attachmentExportServer(t)
	dir := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside-notes.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal(err)
	}

	stdout, err := runAttachmentCommand(t, server, "attachment", "export", "42", "--output", dir, "--force")
	if err != nil {
		t.Fatal(err)
	}
	report := decodeExportReport(t, stdout)
	for _, file := range report.Files {
		if rel, relErr := filepath.Rel(dir, file.Path); relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			t.Errorf("exported file escaped the directory: %s", file.Path)
		}
		if filepath.Base(file.Path) == "notes.txt" {
			t.Errorf("export replaced through the notes.txt symlink: %s", file.Path)
		}
	}
	assertFileText(t, outside, "outside")
	if info, err := os.Lstat(filepath.Join(dir, "notes.txt")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink was modified: %v", err)
	}
	assertFileText(t, filepath.Join(dir, "notes-2.txt"), "notes contents")
	assertFileText(t, filepath.Join(dir, "evil.txt"), "evil contents")
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.txt")); !os.IsNotExist(err) {
		t.Errorf("traversal filename escaped to %s", filepath.Join(filepath.Dir(dir), "evil.txt"))
	}
}

func TestAttachmentExportRequiresOutputDirectory(t *testing.T) {
	server := attachmentExportServer(t)
	_, err := runAttachmentCommand(t, server, "attachment", "export", "42")
	if err == nil || !strings.Contains(err.Error(), "--output") {
		t.Fatalf("missing --output error = %v", err)
	}
}

func TestAttachmentExportCreatesOutputDirectory(t *testing.T) {
	server := attachmentExportServer(t)
	dir := filepath.Join(t.TempDir(), "created", "nested")
	if _, err := runAttachmentCommand(t, server, "attachment", "export", "42", "--output", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatalf("export did not create %s: %v", dir, err)
	}
}

func TestAttachmentExportRejectsOutputPathThatIsAFile(t *testing.T) {
	server := attachmentExportServer(t)
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runAttachmentCommand(t, server, "attachment", "export", "42", "--output", file); err == nil {
		t.Fatal("export accepted a regular file as the output directory")
	}
}

func TestExportReportForMarkdownEscapesUntrustedFields(t *testing.T) {
	report := attachmentExportReport{
		Directory: "<tmp>",
		Planned:   []plannedAttachmentFile{{Filename: "a|b.pdf", Path: "<tmp>/a|b.pdf"}},
		Files:     []exportedAttachmentFile{{Filename: "c|d.pdf", Path: "<tmp>/c|d.pdf", SHA256: "abcd"}},
		Failures:  []attachmentExportFailure{{Filename: "e|f.pdf", Error: "bad|luck"}},
	}
	safe := exportReportForMarkdown(report)
	if !strings.Contains(safe.Planned[0].Filename, `a\|b`) || !strings.Contains(safe.Files[0].Path, `c\|d`) ||
		!strings.Contains(safe.Failures[0].Error, `bad\|luck`) || safe.Directory != `\<tmp\>` {
		t.Errorf("markdown report = %+v", safe)
	}
}
