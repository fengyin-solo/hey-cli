package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/basecamp/hey-cli/internal/apierr"
	attachmentfiles "github.com/basecamp/hey-cli/internal/attachments"
	"github.com/basecamp/hey-cli/internal/output"
	"github.com/basecamp/hey-cli/internal/terminal"
)

type attachmentsExportCommand struct {
	cmd          *cobra.Command
	output       string
	force        bool
	dryRun       bool
	allowPartial bool
}

type exportedAttachmentFile struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	ByteSize    int64  `json:"byte_size"`
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
}

type plannedAttachmentFile struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	ByteSize    *int64 `json:"byte_size,omitempty"`
	Path        string `json:"path"`
	Replaces    bool   `json:"replaces,omitempty"`
	URL         string `json:"-"`
}

type attachmentExportFailure struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	Error    string `json:"error"`
}

type attachmentExportManifest struct {
	ThreadID   int64                     `json:"thread_id"`
	ExportedAt string                    `json:"exported_at"`
	Directory  string                    `json:"directory"`
	Files      []exportedAttachmentFile  `json:"files"`
	Succeeded  int                       `json:"succeeded"`
	Failed     int                       `json:"failed"`
	Failures   []attachmentExportFailure `json:"failures,omitempty"`
}

type attachmentExportReport struct {
	ThreadID   int64                     `json:"thread_id"`
	ExportedAt string                    `json:"exported_at,omitempty"`
	Directory  string                    `json:"directory"`
	DryRun     bool                      `json:"dry_run"`
	Planned    []plannedAttachmentFile   `json:"planned,omitempty"`
	Files      []exportedAttachmentFile  `json:"files,omitempty"`
	Succeeded  int                       `json:"succeeded"`
	Failed     int                       `json:"failed"`
	Failures   []attachmentExportFailure `json:"failures,omitempty"`
	Manifest   string                    `json:"manifest_path,omitempty"`
	Notice     string                    `json:"notice,omitempty"`
}

func newAttachmentsExportCommand() *attachmentsExportCommand {
	attachmentsExportCommand := &attachmentsExportCommand{}
	attachmentsExportCommand.cmd = &cobra.Command{
		Use:   "export <thread-id>",
		Short: "Export every attachment in a thread with a manifest",
		Long: "Download every downloadable attachment in a thread into one directory and write " +
			"manifest.json beside it. Filenames colliding with each other or with files already on " +
			"disk get a deterministic -2, -3 suffix and are never overwritten unless --force is set.",
		Annotations: map[string]string{
			"agent_notes": "Bulk-saves all attachments from `hey attachment list <thread-id>` into --output <directory> " +
				"together with manifest.json (IDs, sizes, paths and SHA-256 checksums). --dry-run prints the plan without " +
				"writing anything; --force replaces existing regular files but never follows symlinks or leaves the " +
				"directory. Per-file download failures are reported and do not stop the rest of the export.",
		},
		Example: `  hey attachment export 12345 --output ./thread-12345
  hey attachment export 12345 -o ./thread-12345 --dry-run
  hey attachment export 12345 -o ./thread-12345 --force`,
		RunE: attachmentsExportCommand.run,
		Args: usageExactOneArg(),
	}

	attachmentsExportCommand.cmd.Flags().StringVarP(&attachmentsExportCommand.output, "output", "o", "", "Directory to save the attachments and manifest in")
	attachmentsExportCommand.cmd.Flags().BoolVar(&attachmentsExportCommand.force, "force", false, "Replace existing destination files")
	attachmentsExportCommand.cmd.Flags().BoolVar(&attachmentsExportCommand.dryRun, "dry-run", false, "Show what would be exported without writing files")
	attachmentsExportCommand.cmd.Flags().BoolVar(&attachmentsExportCommand.allowPartial, "allow-partial", false,
		"Export what could be read of a thread that could only be read in part, with a notice saying what is missing")
	return attachmentsExportCommand
}

func (c *attachmentsExportCommand) run(cmd *cobra.Command, args []string) error {
	if err := requireAuth(); err != nil {
		return err
	}

	threadID, err := parsePositiveID(args[0], "thread")
	if err != nil {
		return err
	}
	if c.output == "" {
		return apierr.ErrUsage("export needs a destination directory: pass --output <directory>")
	}

	attachments, notice, err := attachmentsInThread(cmd.Context(), threadID)
	if err != nil {
		return err
	}
	if notice != "" && !c.allowPartial {
		return errPartialThread(threadID, notice)
	}

	directory := c.output
	if !c.dryRun {
		if err := prepareExportDirectory(directory); err != nil {
			return err
		}
	}
	absoluteDirectory, err := filepath.Abs(directory)
	if err != nil {
		return apierr.ErrAPI(0, fmt.Sprintf("could not resolve output directory: %v", err))
	}

	namer := attachmentfiles.NewExportNamer(absoluteDirectory, c.force)
	if !c.dryRun {
		namer.ReserveName(attachmentfiles.ManifestName)
	}

	planned := make([]plannedAttachmentFile, 0, len(attachments))
	failures := []attachmentExportFailure{}
	for _, attachment := range attachments {
		destination, replaces, reserveErr := namer.Reserve(attachment.Filename)
		if reserveErr != nil {
			failures = append(failures, attachmentExportFailure{
				ID:       attachment.ID,
				Filename: attachment.Filename,
				Error:    reserveErr.Error(),
			})
			continue
		}
		planned = append(planned, plannedAttachmentFile{
			ID:          attachment.ID,
			Filename:    attachment.Filename,
			ContentType: attachment.ContentType,
			ByteSize:    attachment.ByteSize,
			Path:        destination,
			Replaces:    replaces,
			URL:         attachment.URL,
		})
	}

	if c.dryRun {
		return c.writePlan(cmd, threadID, absoluteDirectory, planned, failures, notice)
	}

	files := make([]exportedAttachmentFile, 0, len(planned))
	for _, plan := range planned {
		written, checksum, downloadErr := saveExportAttachment(cmd.Context(), plan.Path, plan.URL, c.force)
		if downloadErr != nil {
			failures = append(failures, attachmentExportFailure{
				ID:       plan.ID,
				Filename: plan.Filename,
				Error:    downloadErr.Error(),
			})
			continue
		}
		files = append(files, exportedAttachmentFile{
			ID:          plan.ID,
			Filename:    plan.Filename,
			ContentType: plan.ContentType,
			ByteSize:    written,
			Path:        plan.Path,
			SHA256:      checksum,
		})
	}

	exportedAt := time.Now().UTC().Format(time.RFC3339Nano)
	manifest := attachmentExportManifest{
		ThreadID:   threadID,
		ExportedAt: exportedAt,
		Directory:  absoluteDirectory,
		Files:      files,
		Succeeded:  len(files),
		Failed:     len(failures),
		Failures:   failures,
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return apierr.ErrAPI(0, fmt.Sprintf("could not encode manifest: %v", err))
	}
	manifestPath, err := attachmentfiles.WriteManifest(absoluteDirectory, c.force, append(encoded, '\n'))
	if err != nil {
		return err
	}

	report := attachmentExportReport{
		ThreadID:   threadID,
		ExportedAt: exportedAt,
		Directory:  absoluteDirectory,
		Files:      files,
		Succeeded:  len(files),
		Failed:     len(failures),
		Failures:   failures,
		Manifest:   manifestPath,
		Notice:     notice,
	}
	return c.writeReport(cmd, report)
}

func prepareExportDirectory(directory string) error {
	info, err := os.Stat(directory)
	switch {
	case err == nil:
		if !info.IsDir() {
			return apierr.ErrUsage(fmt.Sprintf("output path is not a directory: %s", directory))
		}
		return nil
	case os.IsNotExist(err):
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return apierr.ErrAPI(0, fmt.Sprintf("could not create output directory: %v", err))
		}
		return nil
	default:
		return apierr.ErrAPI(0, fmt.Sprintf("could not inspect output directory: %v", err))
	}
}

func saveExportAttachment(ctx context.Context, destination, sourceURL string, force bool) (int64, string, error) {
	written, checksum, err := attachmentfiles.SaveHashed(ctx, sdk, destination, sourceURL, force)
	if err == nil {
		return written, checksum, nil
	}
	var attachmentErr *apierr.Error
	if errors.As(err, &attachmentErr) {
		return written, checksum, err
	}
	return written, checksum, apierr.FromSDK(err)
}

func (c *attachmentsExportCommand) writePlan(cmd *cobra.Command, threadID int64, directory string,
	planned []plannedAttachmentFile, failures []attachmentExportFailure, notice string,
) error {
	if writer.IsStyled() {
		if len(planned) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No attachments would be exported.")
		} else {
			noun := "attachments"
			if len(planned) == 1 {
				noun = "attachment"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Would export %d %s to %s:\n", len(planned), noun, directory)
			table := newTable(cmd.OutOrStdout())
			table.addRow([]string{"ID", "Filename", "Destination"})
			for _, plan := range planned {
				marker := ""
				if plan.Replaces {
					marker = " (replace)"
				}
				table.addRow([]string{plan.ID, terminal.SanitizeLine(plan.Filename), terminal.SanitizeLine(plan.Path + marker)})
			}
			table.print()
		}
		writeExportFailureLines(cmd, failures)
		if notice != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", notice)
		}
		return nil
	}

	report := attachmentExportReport{
		ThreadID:  threadID,
		Directory: directory,
		DryRun:    true,
		Planned:   planned,
		Failed:    len(failures),
		Failures:  failures,
		Notice:    notice,
	}
	if stderrNotice := paginationNoticeForStderr(writer.EffectiveFormat(), notice); stderrNotice != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), stderrNotice)
	}
	noun := "attachments"
	if len(planned) == 1 {
		noun = "attachment"
	}
	result := report
	if writer.EffectiveFormat() == output.FormatMarkdown {
		result = exportReportForMarkdown(report)
	}
	return writeOK(result, output.WithSummary(fmt.Sprintf("Would export %d %s to %s", len(planned), noun, directory)))
}

func (c *attachmentsExportCommand) writeReport(cmd *cobra.Command, report attachmentExportReport) error {
	if writer.IsStyled() {
		for _, file := range report.Files {
			fmt.Fprintf(cmd.OutOrStdout(), "Saved %s (%s)\n", terminal.SanitizeLine(file.Path), formatByteSize(file.ByteSize))
		}
		writeExportFailureLines(cmd, report.Failures)
		switch report.Failed {
		case 0:
			noun := "attachments"
			if report.Succeeded == 1 {
				noun = "attachment"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Exported %d %s to %s (manifest: %s)\n",
				report.Succeeded, noun, report.Directory, report.Manifest)
		default:
			fmt.Fprintf(cmd.OutOrStdout(), "Exported %d of %d attachments to %s; %d failed (manifest: %s)\n",
				report.Succeeded, report.Succeeded+report.Failed, report.Directory, report.Failed, report.Manifest)
		}
		if report.Notice != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", report.Notice)
		}
		return nil
	}

	if stderrNotice := paginationNoticeForStderr(writer.EffectiveFormat(), report.Notice); stderrNotice != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), stderrNotice)
	}
	summary := fmt.Sprintf("Exported %d attachments to %s", report.Succeeded, report.Directory)
	if report.Failed > 0 {
		summary = fmt.Sprintf("Exported %d of %d attachments to %s; %d failed",
			report.Succeeded, report.Succeeded+report.Failed, report.Directory, report.Failed)
	}
	result := report
	if writer.EffectiveFormat() == output.FormatMarkdown {
		result = exportReportForMarkdown(report)
	}
	return writeOK(result, output.WithSummary(summary), output.WithNotice(report.Notice))
}

func writeExportFailureLines(cmd *cobra.Command, failures []attachmentExportFailure) {
	for _, failure := range failures {
		fmt.Fprintf(cmd.ErrOrStderr(), "Failed %s (%s): %s\n",
			terminal.SanitizeLine(failure.ID), terminal.SanitizeLine(failure.Filename), terminal.SanitizeLine(failure.Error))
	}
}

func exportReportForMarkdown(report attachmentExportReport) attachmentExportReport {
	for index := range report.Planned {
		report.Planned[index].Filename = markdownSafeText(report.Planned[index].Filename)
		report.Planned[index].Path = markdownSafeText(report.Planned[index].Path)
	}
	for index := range report.Files {
		report.Files[index].Filename = markdownSafeText(report.Files[index].Filename)
		report.Files[index].Path = markdownSafeText(report.Files[index].Path)
	}
	for index := range report.Failures {
		report.Failures[index].Filename = markdownSafeText(report.Failures[index].Filename)
		report.Failures[index].Error = markdownSafeText(report.Failures[index].Error)
	}
	report.Directory = markdownSafeText(report.Directory)
	report.Manifest = markdownSafeText(report.Manifest)
	return report
}
