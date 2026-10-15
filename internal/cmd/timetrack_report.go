package cmd

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/basecamp/hey-sdk/go/pkg/generated"

	"github.com/basecamp/hey-cli/internal/apierr"
	"github.com/basecamp/hey-cli/internal/output"
)

// Report grouping values. A time bucket is named on the reader's own clock.
const (
	reportGroupByCategory = "category"
	reportGroupByDay      = "day"
	reportGroupByWeek     = "week"
	reportGroupByMonth    = "month"
)

// unfiledCategoryGroup names the bucket a track filed under nothing lands in. An empty
// string would be invisible in a table and read as a missing field in CSV.
const unfiledCategoryGroup = "(none)"

var reportGroupBys = []string{reportGroupByCategory, reportGroupByDay, reportGroupByWeek, reportGroupByMonth}

// report

type timetrackReportCommand struct {
	cmd      *cobra.Command
	groupBy  string
	category int64
	since    string
	until    string
	all      bool
	csv      bool
}

func newTimetrackReportCommand() *timetrackReportCommand {
	timetrackReportCommand := &timetrackReportCommand{}
	timetrackReportCommand.cmd = &cobra.Command{
		Use:   "report",
		Short: "Summarize completed time tracks",
		Long: `Total tracked time over the completed tracks list, grouped by category, day, week
or month. Each group reports how many tracks fell in it, their combined length, and the
earliest start and latest end among them.

--since and --until are whole days, YYYY-MM-DD, on your own clock, both ends included.
A track is filed and bucketed by the day it started, so one that ran past midnight counts
to the day before. Weeks run Monday to Sunday and are named by ISO week (2026-W34).

Like list, one page arrives by default; --all reads on until the list runs out or the
100-page cap stops it, and --category narrows the read server-side by category ID. The
styled, JSON, Markdown and CSV (--csv) views all render the one aggregated table; CSV
columns are group, records, duration, first_start and last_end, with duration as H:MM:SS.`,
		Annotations: map[string]string{
			"agent_notes": "Aggregates the same completed time tracks list reads. --group-by is category, day, week (ISO, Monday start) or month, defaulting to day. --category takes a category ID like list; --since/--until are inclusive YYYY-MM-DD days in the caller's time zone and bucket by the track's local start day, including tracks that cross midnight. --csv writes fixed columns group,records,duration,first_start,last_end.",
		},
		Example: `  hey timetrack report
  hey timetrack report --group-by category --since 2026-08-01 --until 2026-08-07
  hey timetrack report --group-by week --all
  hey timetrack report --group-by month --category 42 --csv`,
		RunE: timetrackReportCommand.run,
		Args: cobra.NoArgs,
	}

	timetrackReportCommand.cmd.Flags().StringVar(&timetrackReportCommand.groupBy, "group-by", reportGroupByDay, "Group totals by "+joinReportGroupBys())
	timetrackReportCommand.cmd.Flags().Int64Var(&timetrackReportCommand.category, "category", 0, "Only tracks filed under this category ID")
	timetrackReportCommand.cmd.Flags().StringVar(&timetrackReportCommand.since, "since", "", "First day to count, YYYY-MM-DD in your time zone (inclusive)")
	timetrackReportCommand.cmd.Flags().StringVar(&timetrackReportCommand.until, "until", "", "Last day to count, YYYY-MM-DD in your time zone (inclusive)")
	timetrackReportCommand.cmd.Flags().BoolVar(&timetrackReportCommand.all, "all", false, "Read every page before summarizing")
	timetrackReportCommand.cmd.Flags().BoolVar(&timetrackReportCommand.csv, "csv", false, "Write the summary as CSV with fixed columns: group, records, duration, first_start, last_end")

	return timetrackReportCommand
}

func joinReportGroupBys() string {
	return reportGroupByCategory + ", " + reportGroupByDay + ", " + reportGroupByWeek + " or " + reportGroupByMonth
}

func (c *timetrackReportCommand) run(cmd *cobra.Command, args []string) error {
	if err := requireAuth(); err != nil {
		return err
	}
	if cmd.Flags().Changed("category") && c.category <= 0 {
		return apierr.ErrUsage("--category takes a category ID; list them with hey timetrack categories")
	}
	if !validReportGroupBy(c.groupBy) {
		return apierr.ErrUsage(fmt.Sprintf("--group-by takes one of %s, not %q", joinReportGroupBys(), c.groupBy))
	}
	since, err := parseReportDay("--since", c.since)
	if err != nil {
		return err
	}
	until, err := parseReportDay("--until", c.until)
	if err != nil {
		return err
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		return apierr.ErrUsage(fmt.Sprintf("--until %s is before --since %s", c.until, c.since))
	}
	if c.csv && timetrackReportStructuredOutputRequested(cmd) {
		return apierr.ErrUsage("--csv carries the summary itself and cannot combine with output formatting flags")
	}

	ctx := cmd.Context()
	first, err := c.readPage(ctx, "")
	if err != nil {
		return err
	}
	collected, err := collectPages(ctx, first, pageRequest{All: c.all, MaxPages: maxTimeTrackPages}, c.readPage)
	if err != nil {
		return err
	}

	matched := filterReportTracks(collected.Items, since, until)
	groups := aggregateTimeTracks(matched, c.groupBy)
	rows := timeTrackReportRows(groups)

	more := collected.Cursor != ""
	notice := timetrackReportNotice(len(matched), collected.Read, more, collected.Truncated)

	if c.csv {
		if notice != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "notice: %s\n", notice)
		}
		return writeTimeTrackReportCSV(cmd.OutOrStdout(), rows)
	}

	if writer.IsStyled() {
		if len(rows) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No time tracks matched.")
		} else {
			printTimeTrackReportTable(cmd.OutOrStdout(), groups)
		}
		if notice != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", notice)
		}
		return nil
	}

	if stderrNotice := paginationNoticeForStderr(writer.EffectiveFormat(), notice); stderrNotice != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), stderrNotice)
	}

	return writeOK(rows,
		output.WithSummary(timeTrackReportSummary(groups)),
		output.WithNotice(notice),
		output.WithMeta("pages_fetched", collected.Read),
		output.WithBreadcrumbs(output.Breadcrumb{
			Action:      "list",
			Command:     "hey timetrack list",
			Description: "List completed time tracks",
		}),
	)
}

func (c *timetrackReportCommand) readPage(ctx context.Context, cursor string) (pageResult[generated.Recording], error) {
	page, err := fetchTimeTrackPage(ctx, c.category, cursor)
	if err != nil {
		return page, timetrackPageReadError(c.category, err)
	}
	return page, nil
}

// timetrackReportStructuredOutputRequested reports whether another output flag was asked
// for alongside --csv, which has nowhere to go: CSV owns stdout.
func timetrackReportStructuredOutputRequested(cmd *cobra.Command) bool {
	return writer.RequestedFormat() != output.FormatAuto || htmlOutput || statsFlag || cmd.Flags().Changed("jq")
}

func validReportGroupBy(value string) bool {
	for _, candidate := range reportGroupBys {
		if value == candidate {
			return true
		}
	}
	return false
}

// parseReportDay reads a YYYY-MM-DD flag as a day on the reader's own clock. parseDateArg
// answers UTC midnight, but the filter compares local days, so only the named y/m/d carry
// over.
func parseReportDay(label, value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	day, err := parseDateArg(strings.TrimPrefix(label, "--")+" date", value)
	if err != nil {
		return time.Time{}, err
	}
	year, month, date := day.Date()
	return time.Date(year, month, date, 0, 0, 0, 0, time.Local), nil
}

// filterReportTracks keeps the tracks whose local start day falls in the window. Both ends
// are whole, inclusive days; a zero bound is open.
func filterReportTracks(tracks []generated.Recording, since, until time.Time) []generated.Recording {
	matched := make([]generated.Recording, 0, len(tracks))
	for _, track := range tracks {
		day := localStartDay(track.StartsAt)
		if !since.IsZero() && day.Before(since) {
			continue
		}
		if !until.IsZero() && day.After(until) {
			continue
		}
		matched = append(matched, track)
	}
	return matched
}

// localStartDay is the midnight, on the reader's own clock, of the day a track began on.
// That day owns the track however far past midnight it ran.
func localStartDay(ts time.Time) time.Time {
	local := ts.Local()
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local)
}

// timeTrackReportGroup is one aggregated bucket, before any view renders it.
type timeTrackReportGroup struct {
	label      string
	bucket     time.Time
	unfiled    bool
	records    int
	duration   time.Duration
	firstStart time.Time
	lastEnd    time.Time
}

// aggregateTimeTracks folds tracks into buckets and hands the buckets back in a stable
// order: chronologically for day, week and month, and by title for category with tracks
// filed under nothing last.
func aggregateTimeTracks(tracks []generated.Recording, groupBy string) []timeTrackReportGroup {
	groups := map[string]*timeTrackReportGroup{}
	order := make([]string, 0)

	for _, track := range tracks {
		label, bucket := reportBucket(groupBy, track)
		group, ok := groups[label]
		if !ok {
			group = &timeTrackReportGroup{
				label:      label,
				bucket:     bucket,
				unfiled:    groupBy == reportGroupByCategory && label == unfiledCategoryGroup,
				firstStart: track.StartsAt,
				lastEnd:    track.EndsAt,
			}
			groups[label] = group
			order = append(order, label)
		}
		group.records++
		group.duration += trackedLength(track)
		if track.StartsAt.Before(group.firstStart) {
			group.firstStart = track.StartsAt
		}
		if track.EndsAt.After(group.lastEnd) {
			group.lastEnd = track.EndsAt
		}
	}

	result := make([]timeTrackReportGroup, 0, len(order))
	for _, label := range order {
		result = append(result, *groups[label])
	}
	sortTimeTrackReportGroups(result, groupBy)
	return result
}

// reportBucket names and dates the bucket a track belongs to on the reader's clock.
func reportBucket(groupBy string, track generated.Recording) (string, time.Time) {
	start := track.StartsAt.Local()
	switch groupBy {
	case reportGroupByDay:
		bucket := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.Local)
		return bucket.Format(dateLayout), bucket
	case reportGroupByWeek:
		bucket := localMonday(start)
		year, week := bucket.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", year, week), bucket
	case reportGroupByMonth:
		bucket := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.Local)
		return bucket.Format("2006-01"), bucket
	default:
		if track.Category == nil || *track.Category == "" {
			return unfiledCategoryGroup, time.Time{}
		}
		return *track.Category, time.Time{}
	}
}

// localMonday is the Monday that begins the week the instant is in, on the reader's clock.
func localMonday(ts time.Time) time.Time {
	day := localStartDay(ts)
	// Sunday is weekday 0; shift it six days back rather than one forward.
	daysSinceMonday := (int(day.Weekday()) + 6) % 7
	return day.AddDate(0, 0, -daysSinceMonday)
}

func sortTimeTrackReportGroups(groups []timeTrackReportGroup, groupBy string) {
	sort.SliceStable(groups, func(i, j int) bool {
		if groupBy == reportGroupByCategory {
			if groups[i].unfiled != groups[j].unfiled {
				return groups[j].unfiled
			}
			return groups[i].label < groups[j].label
		}
		return groups[i].bucket.Before(groups[j].bucket)
	})
}

// timeTrackReportRow is the aggregated bucket in the shape every non-styled view shares,
// with the columns the CSV fixes and a duration already in H:MM:SS.
type timeTrackReportRow struct {
	Group      string `json:"group"`
	Records    int    `json:"records"`
	Duration   string `json:"duration"`
	FirstStart string `json:"first_start"`
	LastEnd    string `json:"last_end"`
}

func timeTrackReportRows(groups []timeTrackReportGroup) []timeTrackReportRow {
	rows := make([]timeTrackReportRow, 0, len(groups))
	for _, group := range groups {
		rows = append(rows, timeTrackReportRow{
			Group:      group.label,
			Records:    group.records,
			Duration:   formatTrackedLength(group.duration),
			FirstStart: formatReportInstant(group.firstStart),
			LastEnd:    formatReportInstant(group.lastEnd),
		})
	}
	return rows
}

// formatReportInstant stamps a boundary as a complete RFC 3339 instant on the reader's
// clock, so a row stays exact in any zone and parses anywhere.
func formatReportInstant(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.Local().Format(time.RFC3339)
}

func printTimeTrackReportTable(w io.Writer, groups []timeTrackReportGroup) {
	table := newTable(w)
	table.addRow([]string{"Group", "Records", "Duration", "First start", "Last end"})
	for _, group := range groups {
		table.addRow([]string{
			group.label,
			strconv.Itoa(group.records),
			formatTrackedLength(group.duration),
			formatReportTableInstant(group.firstStart),
			formatReportTableInstant(group.lastEnd),
		})
	}
	table.print()
}

// formatReportTableInstant is a day and clock for the human table: a group's range can
// span days, so a bare clock would misdate it.
func formatReportTableInstant(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.Local().Format("2006-01-02 15:04")
}

func writeTimeTrackReportCSV(w io.Writer, rows []timeTrackReportRow) error {
	writer := csv.NewWriter(w)
	if err := writer.Write([]string{"group", "records", "duration", "first_start", "last_end"}); err != nil {
		return err
	}
	for _, row := range rows {
		if err := writer.Write([]string{row.Group, strconv.Itoa(row.Records), row.Duration, row.FirstStart, row.LastEnd}); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}

func timeTrackReportSummary(groups []timeTrackReportGroup) string {
	matched, total := 0, time.Duration(0)
	for _, group := range groups {
		matched += group.records
		total += group.duration
	}
	if matched == 0 {
		return "No time tracks matched"
	}
	return fmt.Sprintf("%d %s over %d %s, %s tracked",
		len(groups), reportGroupNoun(len(groups)), matched, timeTrackNoun(matched), formatTrackedLength(total))
}

func reportGroupNoun(count int) string {
	if count == 1 {
		return "group"
	}
	return "groups"
}

// timetrackReportNotice says why a report stopped, the way timetrackListNotice does: a
// report over a first page quietly looks like the whole of somebody's tracked time.
func timetrackReportNotice(matched, pages int, more, truncated bool) string {
	switch {
	case truncated:
		return fmt.Sprintf("Stopped after %d pages, at %d %s. Narrow the report with --category, --since or --until.", pages, matched, timeTrackNoun(matched))
	case more:
		return fmt.Sprintf("Report covers %d %s from the first page. Use --all to read every page.", matched, timeTrackNoun(matched))
	default:
		return ""
	}
}

// fetchTimeTrackPage reads one page of the completed-track index, narrowed server-side by
// category when one was named. Both list and report walk the same index this way.
func fetchTimeTrackPage(ctx context.Context, categoryID int64, cursor string) (pageResult[generated.Recording], error) {
	params := &generated.ListTimeTracksParams{}
	if cursor != "" {
		params.Page = &cursor
	}
	if categoryID > 0 {
		params.CategoryId = &categoryID
	}

	page, err := sdk.TimeTracks().ListPage(ctx, params)
	if err != nil {
		return pageResult[generated.Recording]{}, err
	}
	if page == nil {
		return pageResult[generated.Recording]{}, nil
	}
	return pageResult[generated.Recording]{Items: page.TimeTracks, Cursor: page.NextPage}, nil
}

// timetrackPageReadError names the category HEY could not find. An id no category has
// answers 404 for the whole index, which otherwise reads as tracked time having vanished.
func timetrackPageReadError(categoryID int64, err error) error {
	converted := apierr.FromSDK(err)
	if categoryID > 0 && apierr.AsError(converted).Code == apierr.CodeNotFound {
		return apierr.ErrNotFound("time track category", strconv.FormatInt(categoryID, 10))
	}
	return converted
}
