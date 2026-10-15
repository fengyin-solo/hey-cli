package attachments

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/basecamp/hey-cli/internal/apierr"
)

// ManifestName is the manifest file written alongside an export.
const ManifestName = "manifest.json"

// ExportNamer hands out unique destination paths inside one export directory.
type ExportNamer struct {
	dir   string
	force bool
	taken map[string]bool
}

// NewExportNamer reserves names inside dir. With force, regular files already
// present in dir may be handed back for replacement.
func NewExportNamer(dir string, force bool) *ExportNamer {
	return &ExportNamer{dir: dir, force: force, taken: make(map[string]bool)}
}

// ReserveName pins a name so no attachment can claim it. The export manifest
// reserves its own name this way.
func (n *ExportNamer) ReserveName(name string) {
	n.taken[name] = true
}

// Reserve returns the destination for an attachment's filename, cleaned by
// PortableFilename. When the preferred name is taken — by another attachment
// in this export or by an entry already on disk — a deterministic "-2", "-3",
// … suffix is inserted before the extension. With force, an existing regular
// file is handed back for replacement and replacing is true; directories,
// symlinks and other non-regular entries are never replaced and always take
// the next numbered name instead.
func (n *ExportNamer) Reserve(filename string) (destination string, replacing bool, err error) {
	portable, err := PortableFilename(filename)
	if err != nil {
		return "", false, err
	}
	name := portable
	for suffix := 2; ; suffix++ {
		if !n.taken[name] {
			occupied, replaceable, inspectErr := n.inspect(name)
			if inspectErr != nil {
				return "", false, inspectErr
			}
			if !occupied {
				n.taken[name] = true
				candidate := filepath.Join(n.dir, name)
				if ensureErr := EnsureWithin(n.dir, candidate); ensureErr != nil {
					return "", false, ensureErr
				}
				return candidate, replaceable, nil
			}
		}
		name = numberedFilename(portable, suffix)
	}
}

func (n *ExportNamer) inspect(name string) (occupied, replaceable bool, err error) {
	info, statErr := os.Lstat(filepath.Join(n.dir, name))
	if os.IsNotExist(statErr) {
		return false, false, nil
	}
	if statErr != nil {
		return false, false, apierr.ErrAPI(0, fmt.Sprintf("could not inspect destination: %v", statErr))
	}
	if !info.Mode().IsRegular() {
		// Never replace directories, symlinks or special files, even with force.
		return true, false, nil
	}
	return !n.force, n.force, nil
}

// numberedFilename inserts "-n" before the final extension: report.pdf becomes
// report-2.pdf; a name without an extension gets the suffix at the end.
func numberedFilename(filename string, suffix int) string {
	dot := strings.LastIndexByte(filename, '.')
	if dot <= 0 {
		return fmt.Sprintf("%s-%d", filename, suffix)
	}
	return fmt.Sprintf("%s-%d%s", filename[:dot], suffix, filename[dot:])
}

// PrepareTarget validates that dir/name stays inside dir and may be written per
// force. It creates and modifies nothing.
func PrepareTarget(dir, name string, force bool) (string, error) {
	destination := filepath.Join(dir, name)
	if err := EnsureWithin(dir, destination); err != nil {
		return "", err
	}
	info, err := os.Lstat(destination)
	if os.IsNotExist(err) {
		return destination, nil
	}
	if err != nil {
		return "", apierr.ErrAPI(0, fmt.Sprintf("could not inspect destination: %v", err))
	}
	if !info.Mode().IsRegular() {
		return "", apierr.ErrUsage(fmt.Sprintf("refusing to replace non-regular file: %s", destination))
	}
	if !force {
		return "", apierr.ErrUsage(fmt.Sprintf("destination already exists: %s (use --force to replace it)", destination))
	}
	return destination, nil
}

// EnsureWithin reports an error unless target is dir itself or a path below it.
func EnsureWithin(dir, target string) error {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return apierr.ErrAPI(0, fmt.Sprintf("could not resolve export directory: %v", err))
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return apierr.ErrAPI(0, fmt.Sprintf("could not resolve destination: %v", err))
	}
	if absTarget != absDir {
		rel, err := filepath.Rel(absDir, absTarget)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return apierr.ErrUsage("refusing to write a file outside the export directory")
		}
	}
	return nil
}

// WriteManifest atomically writes data as dir/manifest.json. An existing
// manifest is replaced only with force; symlinks and paths outside dir are
// never written.
func WriteManifest(dir string, force bool, data []byte) (string, error) {
	destination, err := PrepareTarget(dir, ManifestName, force)
	if err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(dir, ".hey-manifest-*")
	if err != nil {
		return "", apierr.ErrAPI(0, fmt.Sprintf("could not create temporary file: %v", err))
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return "", apierr.ErrAPI(0, fmt.Sprintf("could not write manifest: %v", err))
	}
	if err := temporary.Close(); err != nil {
		return "", apierr.ErrAPI(0, fmt.Sprintf("could not close temporary file: %v", err))
	}
	if err := commitStagedFile(temporaryPath, destination, force); err != nil {
		return "", err
	}
	return destination, nil
}
