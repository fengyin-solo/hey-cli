package attachments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/basecamp/hey-cli/internal/apierr"
)

type Downloader interface {
	DownloadBlob(context.Context, string, io.Writer) (int64, http.Header, error)
}

const heyBlobPathPrefix = "/rails/active_storage/blobs/"

// IsHEYBlobURL reports whether source is a clean relative path to HEY's blob storage.
func IsHEYBlobURL(source string) bool {
	parsed, err := url.Parse(source)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	if parsed.Path == "" || strings.Contains(parsed.Path, `\`) || path.Clean(parsed.Path) != parsed.Path || !strings.HasPrefix(parsed.Path, heyBlobPathPrefix) {
		return false
	}
	for _, character := range parsed.Path {
		if unicode.IsControl(character) {
			return false
		}
	}
	return len(parsed.Path) > len(heyBlobPathPrefix)
}

func Destination(outputPath, filename string) (string, error) {
	if outputPath == "" {
		return PortableFilename(filename)
	}
	info, err := os.Stat(outputPath)
	if err == nil && info.IsDir() {
		safeFilename, filenameErr := PortableFilename(filename)
		if filenameErr != nil {
			return "", filenameErr
		}
		return filepath.Join(outputPath, safeFilename), nil
	}
	if err != nil && !os.IsNotExist(err) {
		return "", apierr.ErrAPI(0, fmt.Sprintf("could not inspect output path: %v", err))
	}
	return filepath.Clean(outputPath), nil
}

func PortableFilename(filename string) (string, error) {
	filename = strings.TrimSpace(filename)
	filename = path.Base(strings.ReplaceAll(filename, `\`, "/"))
	if filename == "" || filename == "." || filename == ".." || filename == "/" {
		return "", apierr.ErrUsage("attachment has no safe filename")
	}
	filename = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, filename)
	filename = strings.TrimRight(filename, ". ")
	if filename == "" {
		filename = "attachment"
	}
	if windowsReservedFilename(filename) {
		filename = "_" + filename
	}
	return filename, nil
}

// SaveBytes safely writes data to destination. Existing paths are preserved unless force is set.
func SaveBytes(destination string, data []byte, force bool) (int64, error) {
	written, _, err := saveHashed(context.Background(), byteDownloader(data), destination, "", force)
	return written, err
}

type byteDownloader []byte

func (data byteDownloader) DownloadBlob(_ context.Context, _ string, writer io.Writer) (int64, http.Header, error) {
	written, err := io.Copy(writer, bytes.NewReader(data))
	return written, nil, err
}

func Save(ctx context.Context, downloader Downloader, destination, sourceURL string, force bool) (int64, error) {
	written, _, err := SaveHashed(ctx, downloader, destination, sourceURL, force)
	return written, err
}

// SaveHashed behaves like Save but also returns the hex SHA-256 of the saved content.
func SaveHashed(ctx context.Context, downloader Downloader, destination, sourceURL string, force bool) (int64, string, error) {
	if !IsHEYBlobURL(sourceURL) {
		return 0, "", apierr.ErrAPI(0, "attachment URL does not identify a HEY blob")
	}
	return saveHashed(ctx, downloader, destination, sourceURL, force)
}

func saveHashed(ctx context.Context, downloader Downloader, destination, sourceURL string, force bool) (int64, string, error) {
	if !force {
		if _, err := os.Lstat(destination); err == nil {
			return 0, "", apierr.ErrUsage(fmt.Sprintf("destination already exists: %s (use --force to replace it)", destination))
		} else if !os.IsNotExist(err) {
			return 0, "", apierr.ErrAPI(0, fmt.Sprintf("could not inspect destination: %v", err))
		}
	}

	directory := filepath.Dir(destination)
	temporary, err := os.CreateTemp(directory, ".hey-file-*")
	if err != nil {
		return 0, "", apierr.ErrAPI(0, fmt.Sprintf("could not create temporary file: %v", err))
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()

	hasher := sha256.New()
	written, _, err := downloader.DownloadBlob(ctx, sourceURL, io.MultiWriter(temporary, hasher))
	if err != nil {
		_ = temporary.Close()
		return written, "", err
	}
	if err := temporary.Close(); err != nil {
		return written, "", apierr.ErrAPI(0, fmt.Sprintf("could not close temporary file: %v", err))
	}
	checksum := hex.EncodeToString(hasher.Sum(nil))

	if force {
		if err := commitStagedFile(temporaryPath, destination, true); err != nil {
			return written, checksum, err
		}
		return written, checksum, nil
	}
	if err := commitStagedFile(temporaryPath, destination, false); err != nil {
		return written, checksum, err
	}
	return written, checksum, nil
}

func commitStagedFile(temporaryPath, destination string, force bool) error {
	if force {
		if err := replaceFile(temporaryPath, destination); err != nil {
			return apierr.ErrAPI(0, fmt.Sprintf("could not replace file: %v", err))
		}
		return nil
	}
	if err := commitFileNoReplace(temporaryPath, destination); os.IsExist(err) {
		return apierr.ErrUsage(fmt.Sprintf("destination already exists: %s (use --force to replace it)", destination))
	} else if err != nil {
		return apierr.ErrAPI(0, fmt.Sprintf("could not save file: %v", err))
	}
	return nil
}

func windowsReservedFilename(filename string) bool {
	stem := filename
	if dot := strings.IndexByte(stem, '.'); dot >= 0 {
		stem = stem[:dot]
	}
	stem = strings.ToUpper(stem)
	if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || stem == "CONIN$" || stem == "CONOUT$" {
		return true
	}
	runes := []rune(stem)
	if len(runes) != 4 || (string(runes[:3]) != "COM" && string(runes[:3]) != "LPT") {
		return false
	}
	if runes[3] == '¹' || runes[3] == '²' || runes[3] == '³' {
		return true
	}
	return runes[3] >= '1' && runes[3] <= '9'
}
