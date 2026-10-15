package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	hey "github.com/basecamp/hey-sdk/go/pkg/hey"

	"github.com/basecamp/hey-cli/internal/apierr"
	"github.com/basecamp/hey-cli/internal/output"
	"github.com/basecamp/hey-cli/internal/terminal"
)

type screenerRescreenCommand struct {
	cmd    *cobra.Command
	dryRun bool
	box    string
	seen   bool
	spam   bool
}

func newScreenerRescreenCommand() *screenerRescreenCommand {
	rescreenCommand := &screenerRescreenCommand{}
	rescreenCommand.cmd = &cobra.Command{
		Use:   "rescreen <approve|deny> <clearance-id>...",
		Short: "Change your mind about a screened sender",
		Long:  "Approve again or deny again a sender already screened, by the ID from hey screener history.",
		Annotations: map[string]string{
			"agent_notes": "Works the decided list from `hey screener history`, not the queue: each ID is read back there first, so an ID still waiting — or one that does not exist — is reported as a failure and left alone. Every ID is rescreened on its own through the decided-senders endpoint; one failure does not stop the rest, and the command exits non-zero if any failed. A sender already in the requested state is reported unchanged rather than rewritten. Re-deciding a waiting sender takes `hey screener approve` or `hey screener deny`; --box, --seen and --spam are refused here because the rescreen endpoint answers with a status alone. --dry-run prints what would change without sending the request.",
		},
		Example: `  hey screener rescreen deny 12345
  hey screener rescreen approve 12345 67890
  hey screener rescreen deny 12345 --dry-run
  hey screener rescreen approve 12345 --json`,
		RunE: rescreenCommand.run,
		Args: usageMinArgs(2),
	}
	rescreenCommand.cmd.Flags().BoolVar(&rescreenCommand.dryRun, "dry-run", false, "Print what would change without sending the request")
	// The rescreen endpoint takes a status alone. The flags are accepted so the
	// refusal can name the command that does support them, instead of reading as
	// an unknown-flag typo.
	rescreenCommand.cmd.Flags().StringVar(&rescreenCommand.box, "box", "", "Not supported here — answer a waiting sender with hey screener approve")
	rescreenCommand.cmd.Flags().BoolVar(&rescreenCommand.seen, "seen", false, "Not supported here — answer a waiting sender with hey screener approve")
	rescreenCommand.cmd.Flags().BoolVar(&rescreenCommand.spam, "spam", false, "Not supported here — answer a waiting sender with hey screener deny")
	return rescreenCommand
}

// rescreenResult is one ID's answer, whether or not its request landed. Failed
// records keep whatever the read-back learned and carry the reason in Error.
type rescreenResult struct {
	ID          int64  `json:"id"`
	Status      string `json:"status,omitempty"`
	Name        string `json:"name,omitempty"`
	Email       string `json:"email_address,omitempty"`
	Changed     bool   `json:"changed"`
	WouldChange bool   `json:"would_change,omitempty"`
	Error       string `json:"error,omitempty"`

	previous string `json:"-"`
	code     string `json:"-"`
}

func (c *screenerRescreenCommand) run(cmd *cobra.Command, args []string) error {
	if err := requireAuth(); err != nil {
		return err
	}

	target, err := parseRescreenDecision(args[0])
	if err != nil {
		return err
	}
	ids, err := parseClearanceIDs(args[1:])
	if err != nil {
		return err
	}
	if err := c.rejectQueueFlags(); err != nil {
		return err
	}

	results := make([]rescreenResult, 0, len(ids))
	for _, id := range ids {
		result := c.rescreenOne(cmd.Context(), id, target)
		results = append(results, result)
		if writer.IsStyled() {
			fmt.Fprint(cmd.OutOrStdout(), rescreenLine(result, target, c.dryRun))
		}
	}

	if writer.IsStyled() {
		return rescreenAggregateError(results)
	}

	opts := []output.ResponseOption{
		output.WithSummary(rescreenSummary(results, c.dryRun)),
		output.WithBreadcrumbs(rescreenReverse(target)),
	}
	if c.dryRun {
		opts = append(opts, output.WithNotice("Dry run — nothing was changed"))
	}
	if err := writeOK(results, opts...); err != nil {
		return err
	}
	return rescreenAggregateError(results)
}

func (c *screenerRescreenCommand) rejectQueueFlags() error {
	if c.box != "" {
		return errRescreenQueueFlag("box", "approve")
	}
	if c.seen {
		return errRescreenQueueFlag("seen", "approve")
	}
	if c.spam {
		return errRescreenQueueFlag("spam", "deny")
	}
	return nil
}

func errRescreenQueueFlag(flag, command string) error {
	return apierr.ErrUsageHint(
		fmt.Sprintf("--%s only answers a sender still waiting in the Screener queue", flag),
		fmt.Sprintf("use hey screener %s <id>; rescreen only sets the status of a sender already decided", command),
	)
}

func parseRescreenDecision(value string) (string, error) {
	switch value {
	case "approve":
		return hey.ClearanceApproved, nil
	case "deny":
		return hey.ClearanceDenied, nil
	default:
		return "", apierr.ErrUsageHint(
			fmt.Sprintf("unknown decision %q: the first argument must be approve or deny", value),
			"Example: hey screener rescreen deny 12345",
		)
	}
}

// rescreenOne reads the decided record back, then writes the new status for that
// one ID. A failure anywhere answers only that ID, so the caller can keep going.
func (c *screenerRescreenCommand) rescreenOne(ctx context.Context, id int64, target string) rescreenResult {
	result := rescreenResult{ID: id}

	screened, err := findScreenedClearance(ctx, id)
	if err != nil {
		result.fail(err)
		return result
	}
	if screened == nil {
		result.fail(apierr.ErrNotFoundHint("screened sender", fmt.Sprintf("%d", id),
			"the ID must come from hey screener history; a sender still waiting is answered with hey screener approve or hey screener deny"))
		return result
	}

	result.Status = screened.Status
	result.Name = screened.Name
	result.Email = screened.Email
	result.previous = screened.Status

	if screened.Status == target {
		return result
	}
	if c.dryRun {
		result.WouldChange = true
		return result
	}

	clearance, err := sdk.Clearances().Rescreen(ctx, id, target)
	if err != nil {
		result.fail(apierr.FromSDK(err))
		return result
	}

	result.Changed = true
	result.Status = target
	if clearance != nil {
		if clearance.Status != "" {
			result.Status = clearance.Status
		}
		if clearance.Petitioner.Name != "" {
			result.Name = clearance.Petitioner.Name
		}
		if clearance.Petitioner.EmailAddress != "" {
			result.Email = clearance.Petitioner.EmailAddress
		}
	}
	return result
}

func (r *rescreenResult) fail(err error) {
	var apiErr *apierr.Error
	if errors.As(err, &apiErr) {
		r.code = apiErr.Code
	} else {
		r.code = apierr.CodeAPI
	}
	r.Error = err.Error()
}

// findScreenedClearance walks the decided list one page at a time until the ID
// shows up. nil without an error means the list was read to its end without the
// ID on it.
func findScreenedClearance(ctx context.Context, id int64) (*screenedClearance, error) {
	cursor := ""
	for pages := 0; pages < maxScreenerPages; pages++ {
		page, err := sdk.Clearances().ScreenedPage(ctx, cursor)
		if err != nil {
			return nil, apierr.FromSDK(err)
		}
		if page == nil {
			return nil, nil
		}
		for i := range page.Clearances {
			if page.Clearances[i].Id == id {
				screened := screenedClearanceFor(page.Clearances[i])
				return &screened, nil
			}
		}
		if page.NextPage == "" {
			return nil, nil
		}
		cursor = page.NextPage
	}
	return nil, apierr.ErrUsage(fmt.Sprintf("could not confirm clearance %d among screened senders: read stopped after %d pages", id, maxScreenerPages))
}

func rescreenLabel(r rescreenResult) string {
	if r.Name != "" {
		return r.Name
	}
	if r.Email != "" {
		return r.Email
	}
	return fmt.Sprintf("clearance %d", r.ID)
}

func rescreenVerb(status string, past bool) string {
	approved := "approve"
	denied := "deny"
	if past {
		approved, denied = "approved", "denied"
	}
	if status == hey.ClearanceDenied {
		return denied
	}
	return approved
}

// rescreenLine is the one human-readable line an answer gets, in the order the
// IDs were given.
func rescreenLine(r rescreenResult, target string, dryRun bool) string {
	label := terminal.SanitizeLine(rescreenLabel(r))
	switch {
	case r.Error != "":
		return fmt.Sprintf("%s: not rescreened — %s\n", label, terminal.SanitizeLine(r.Error))
	case dryRun && r.WouldChange:
		return fmt.Sprintf("Would %s %s (currently %s).\n", rescreenVerb(target, false), label, terminal.SanitizeLine(r.previous))
	case dryRun:
		return fmt.Sprintf("%s is already %s; no change.\n", label, terminal.SanitizeLine(r.Status))
	case r.Changed:
		return fmt.Sprintf("%s %s (was %s).\n", label, rescreenVerb(target, true), terminal.SanitizeLine(r.previous))
	default:
		return fmt.Sprintf("%s is already %s; no change.\n", label, terminal.SanitizeLine(r.Status))
	}
}

func rescreenSummary(results []rescreenResult, dryRun bool) string {
	var changed, unchanged, wouldChange, failed int
	for _, r := range results {
		switch {
		case r.Error != "":
			failed++
		case dryRun && r.WouldChange:
			wouldChange++
		case r.Changed:
			changed++
		default:
			unchanged++
		}
	}

	var parts []string
	switch {
	case dryRun:
		if wouldChange > 0 {
			parts = append(parts, fmt.Sprintf("%d would change", wouldChange))
		}
	default:
		if changed > 0 {
			parts = append(parts, fmt.Sprintf("%d %s rescreened", changed, senderNoun(changed)))
		}
	}
	if unchanged > 0 {
		parts = append(parts, fmt.Sprintf("%d unchanged", unchanged))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	summary := strings.Join(parts, ", ")
	if dryRun {
		return "Dry run: " + summary
	}
	return summary
}

// rescreenReverse breadcrumbs the decision that would undo this one — the same
// endpoint, with the opposite status.
func rescreenReverse(target string) output.Breadcrumb {
	opposite := hey.ClearanceApproved
	if target == hey.ClearanceApproved {
		opposite = hey.ClearanceDenied
	}
	return output.Breadcrumb{
		Action:      "rescreen",
		Command:     fmt.Sprintf("hey screener rescreen %s <id>", rescreenVerb(opposite, false)),
		Description: "Change this decision again",
	}
}

// rescreenAggregateError turns any per-ID failures into one non-zero exit while
// keeping the itemized report that was already written. Every failure sharing a
// code keeps that code's exit status; mixed failures exit as API errors.
func rescreenAggregateError(results []rescreenResult) error {
	failed := make([]rescreenResult, 0)
	for _, r := range results {
		if r.Error != "" {
			failed = append(failed, r)
		}
	}
	if len(failed) == 0 {
		return nil
	}

	details := make([]string, 0, len(failed))
	code := ""
	for _, r := range failed {
		details = append(details, fmt.Sprintf("%d: %s", r.ID, r.Error))
		if code == "" {
			code = r.code
		} else if code != r.code {
			code = apierr.CodeAPI
		}
	}
	return &apierr.Error{
		Code:    code,
		Message: fmt.Sprintf("%d of %d %s could not be rescreened: %s", len(failed), len(results), senderNoun(len(results)), strings.Join(details, "; ")),
	}
}
