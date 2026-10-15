package attachments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportNamerNumbersDuplicateNamesDeterministically(t *testing.T) {
	dir := t.TempDir()
	namer := NewExportNamer(dir, false)

	var got []string
	for range 3 {
		destination, replacing, err := namer.Reserve("report.pdf")
		if err != nil {
			t.Fatal(err)
		}
		if replacing {
			t.Errorf("fresh export marked %s as replacing an existing file", destination)
		}
		got = append(got, filepath.Base(destination))
	}
	want := []string{"report.pdf", "report-2.pdf", "report-3.pdf"}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("reservation %d = %q, want %q", index, got[index], want[index])
		}
	}
}

func TestExportNamerSuffixesNamesWithoutExtensions(t *testing.T) {
	namer := NewExportNamer(t.TempDir(), false)
	first, _, err := namer.Reserve("README")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := namer.Reserve("README")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(first) != "README" || filepath.Base(second) != "README-2" {
		t.Errorf("got %q, %q", first, second)
	}
}

func TestExportNamerSkipsFilesAlreadyOnDiskUnlessForced(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.pdf"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report-2.pdf"), []byte("oldest"), 0o600); err != nil {
		t.Fatal(err)
	}

	withoutForce := NewExportNamer(dir, false)
	destination, replacing, err := withoutForce.Reserve("report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if replacing || filepath.Base(destination) != "report-3.pdf" {
		t.Errorf("without force got %q (replacing=%t), want report-3.pdf", destination, replacing)
	}
	assertFileContent(t, filepath.Join(dir, "report.pdf"), "old")
	assertFileContent(t, filepath.Join(dir, "report-2.pdf"), "oldest")

	withForce := NewExportNamer(dir, true)
	destination, replacing, err = withForce.Reserve("report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if !replacing || filepath.Base(destination) != "report.pdf" {
		t.Errorf("with force got %q (replacing=%t), want report.pdf", destination, replacing)
	}
	second, replacing, err := withForce.Reserve("report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if !replacing || filepath.Base(second) != "report-2.pdf" {
		t.Errorf("second reserve got %q (replacing=%t), want report-2.pdf marked for replacement", second, replacing)
	}
}

func TestExportNamerCleansTraversalNamesIntoDirectory(t *testing.T) {
	dir := t.TempDir()
	namer := NewExportNamer(dir, false)
	destination, _, err := namer.Reserve("../../evil.txt")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(destination) != "evil.txt" {
		t.Errorf("destination = %q, want evil.txt inside the export directory", destination)
	}
	if err := EnsureWithin(dir, destination); err != nil {
		t.Errorf("cleaned destination escapes the directory: %v", err)
	}
}

func TestExportNamerNeverReplacesSymlinksEvenWithForce(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	namer := NewExportNamer(dir, true)
	destination, replacing, err := namer.Reserve("linked.txt")
	if err != nil {
		t.Fatal(err)
	}
	if replacing || filepath.Base(destination) != "linked-2.txt" {
		t.Errorf("symlink got %q (replacing=%t), want linked-2.txt", destination, replacing)
	}
	assertFileContent(t, outside, "outside")
}

func TestExportNamerReservesManifestName(t *testing.T) {
	dir := t.TempDir()
	namer := NewExportNamer(dir, false)
	namer.ReserveName(ManifestName)
	destination, _, err := namer.Reserve(ManifestName)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(destination) != "manifest-2.json" {
		t.Errorf("attachment claimed the manifest name: %q", destination)
	}
}

func TestEnsureWithinRejectsPathsOutsideDirectory(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "evil.txt")
	if err := EnsureWithin(dir, outside); err == nil {
		t.Errorf("EnsureWithin accepted %s", outside)
	}
	inside := filepath.Join(dir, "nested", "..", "report.pdf")
	if err := EnsureWithin(dir, inside); err != nil {
		t.Errorf("EnsureWithin rejected a contained path: %v", err)
	}
}

func TestWriteManifestHonorsForceAndRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()
	destination, err := WriteManifest(dir, false, []byte("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(destination) != ManifestName {
		t.Errorf("manifest path = %q", destination)
	}
	assertFileContent(t, destination, "{}\n")

	if _, err := WriteManifest(dir, false, []byte("{}\n")); err == nil {
		t.Error("second manifest write without force should fail")
	}
	if _, err := WriteManifest(dir, true, []byte(`{"ok":true}`+"\n")); err != nil {
		t.Fatalf("forced manifest write: %v", err)
	}
	assertFileContent(t, destination, `{"ok":true}`+"\n")

	linkedDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside-manifest.json")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(linkedDir, ManifestName)); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteManifest(linkedDir, true, []byte("{}\n")); err == nil {
		t.Error("manifest write replaced a symlink")
	}
	assertFileContent(t, outside, "outside")
}

func TestPrepareTargetRejectsDirectories(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareTarget(dir, "nested", true); err == nil {
		t.Error("PrepareTarget accepted a directory")
	}
}

func TestSaveHashedReturnsChecksumOfSavedContent(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "report.pdf")
	content := "hash me please"
	written, checksum, err := SaveHashed(context.Background(), writeDownload(content),
		destination, "/rails/active_storage/blobs/redirect/signed/report.pdf", false)
	if err != nil {
		t.Fatal(err)
	}
	if written != int64(len(content)) {
		t.Errorf("written = %d, want %d", written, len(content))
	}
	digest := sha256.Sum256([]byte(content))
	if checksum != hex.EncodeToString(digest[:]) {
		t.Errorf("checksum = %q, want %x", checksum, digest)
	}
	assertFileContent(t, destination, content)

	// Save keeps its original behavior on top of the hashed implementation.
	if _, err := Save(context.Background(), writeDownload(content),
		filepath.Join(dir, "other.pdf"), "/rails/active_storage/blobs/redirect/signed/other.pdf", false); err != nil {
		t.Fatalf("Save through SaveHashed: %v", err)
	}
}

func TestSaveHashedKeepsRejectionErrorForNonBlobURLs(t *testing.T) {
	_, _, err := SaveHashed(context.Background(), writeDownload("x"),
		filepath.Join(t.TempDir(), "x.pdf"), "/identity.json", true)
	if !strings.Contains(err.Error(), "HEY blob") {
		t.Fatalf("error = %v, want a HEY blob rejection", err)
	}
}
