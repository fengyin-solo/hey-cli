package cmd

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basecamp/hey-sdk/go/pkg/generated"
)

// reportTrack builds a completed recording for the aggregation tests. Times are the UTC
// instants HEY would serve.
func reportTrack(id int64, category *string, start, end string) generated.Recording {
	return generated.Recording{
		Id:       id,
		Category: category,
		StartsAt: mustReportTime(start),
		EndsAt:   mustReportTime(end),
	}
}

func mustReportTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}

func reportCategory(title string) *string { return &title }

// reportRows decodes the JSON envelope's data into the shared row shape.
func reportRows(t *testing.T, data any) []timeTrackReportRow {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal report data: %v", err)
	}
	var rows []timeTrackReportRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode report rows: %v", err)
	}
	return rows
}

func TestAggregateTimeTracksByCategory(t *testing.T) {
	inLocalZone(t, 2)
	tracks := []generated.Recording{
		reportTrack(3, reportCategory("Planning"), "2026-08-19T14:00:00Z", "2026-08-19T14:30:00Z"),
		reportTrack(2, reportCategory("Client work"), "2026-08-21T15:00:00Z", "2026-08-21T16:30:00Z"),
		reportTrack(1, nil, "2026-08-20T22:30:00Z", "2026-08-21T00:45:00Z"),
		reportTrack(4, reportCategory("Client work"), "2026-08-21T09:00:00Z", "2026-08-21T10:00:00Z"),
	}

	groups := aggregateTimeTracks(tracks, reportGroupByCategory)

	gotLabels := make([]string, len(groups))
	for i, group := range groups {
		gotLabels[i] = group.label
	}
	wantLabels := []string{"Client work", "Planning", unfiledCategoryGroup}
	if strings.Join(gotLabels, ",") != strings.Join(wantLabels, ",") {
		t.Errorf("group order = %v, want %v (unfiled last)", gotLabels, wantLabels)
	}

	byLabel := map[string]timeTrackReportGroup{}
	for _, group := range groups {
		byLabel[group.label] = group
	}
	client := byLabel["Client work"]
	if client.records != 2 {
		t.Errorf("client records = %d, want 2", client.records)
	}
	if client.duration != 2*time.Hour+30*time.Minute {
		t.Errorf("client duration = %s, want 2:30:00", client.duration)
	}
	wantFirst := mustReportTime("2026-08-21T09:00:00Z")
	wantLast := mustReportTime("2026-08-21T16:30:00Z")
	if !client.firstStart.Equal(wantFirst) || !client.lastEnd.Equal(wantLast) {
		t.Errorf("client range = %s..%s, want %s..%s", client.firstStart, client.lastEnd, wantFirst, wantLast)
	}
	if byLabel[unfiledCategoryGroup].records != 1 {
		t.Errorf("unfiled records = %d, want 1", byLabel[unfiledCategoryGroup].records)
	}
}

// A track that ran past midnight belongs to the day it started on, read on the reader's
// own clock. The same UTC track starts on a different local date east and west of
// Greenwich.
func TestAggregateTimeTracksByDayCrossesMidnight(t *testing.T) {
	track := reportTrack(1, nil, "2026-08-20T22:30:00Z", "2026-08-21T00:45:00Z")

	inLocalZone(t, 2)
	east := aggregateTimeTracks([]generated.Recording{track}, reportGroupByDay)
	if len(east) != 1 || east[0].label != "2026-08-21" {
		t.Errorf("UTC+2 groups = %v, want only 2026-08-21", east)
	}

	local := time.Local
	time.Local = time.FixedZone("UTC-5", -5*60*60)
	t.Cleanup(func() { time.Local = local })
	west := aggregateTimeTracks([]generated.Recording{track}, reportGroupByDay)
	if len(west) != 1 || west[0].label != "2026-08-20" {
		t.Errorf("UTC-5 groups = %v, want only 2026-08-20", west)
	}
}

func TestAggregateTimeTracksByWeek(t *testing.T) {
	inLocalZone(t, 2)
	tracks := []generated.Recording{
		reportTrack(1, reportCategory("Client work"), "2026-08-21T09:00:00Z", "2026-08-21T11:15:00Z"),
		reportTrack(2, nil, "2026-08-20T22:30:00Z", "2026-08-21T00:45:00Z"),
		reportTrack(3, reportCategory("Planning"), "2026-08-19T14:00:00Z", "2026-08-19T14:30:00Z"),
	}

	groups := aggregateTimeTracks(tracks, reportGroupByWeek)
	if len(groups) != 1 {
		t.Fatalf("groups = %v, want one week", groups)
	}
	group := groups[0]
	if group.label != "2026-W34" {
		t.Errorf("week label = %q, want 2026-W34", group.label)
	}
	if group.records != 3 {
		t.Errorf("records = %d, want 3", group.records)
	}
	if group.duration != 5*time.Hour {
		t.Errorf("duration = %s, want 5:00:00", group.duration)
	}
	wantFirst := mustReportTime("2026-08-19T14:00:00Z")
	wantLast := mustReportTime("2026-08-21T11:15:00Z")
	if !group.firstStart.Equal(wantFirst) || !group.lastEnd.Equal(wantLast) {
		t.Errorf("range = %s..%s, want %s..%s", group.firstStart, group.lastEnd, wantFirst, wantLast)
	}
}

func TestAggregateTimeTracksByMonth(t *testing.T) {
	inLocalZone(t, 2)
	tracks := []generated.Recording{
		reportTrack(1, reportCategory("Client work"), "2026-08-21T09:00:00Z", "2026-08-21T11:15:00Z"),
		reportTrack(2, reportCategory("Planning"), "2026-08-19T14:00:00Z", "2026-08-19T14:30:00Z"),
	}

	groups := aggregateTimeTracks(tracks, reportGroupByMonth)
	if len(groups) != 1 || groups[0].label != "2026-08" {
		t.Fatalf("groups = %v, want only 2026-08", groups)
	}
	if groups[0].records != 2 || groups[0].duration != 2*time.Hour+45*time.Minute {
		t.Errorf("group = %d records, %s; want 2 and 2:45:00", groups[0].records, groups[0].duration)
	}
}

// ISO weeks cross calendar years: the first days of January belong to the last week of
// the year before. The groups stay ordered by Monday, not by their label strings.
func TestAggregateTimeTracksWeekYearBoundary(t *testing.T) {
	inLocalZone(t, 2)
	tracks := []generated.Recording{
		reportTrack(2, nil, "2027-01-04T10:00:00Z", "2027-01-04T11:00:00Z"),
		reportTrack(1, nil, "2027-01-01T00:30:00Z", "2027-01-01T01:30:00Z"),
	}

	groups := aggregateTimeTracks(tracks, reportGroupByWeek)
	got := []string{groups[0].label, groups[1].label}
	want := []string{"2026-W53", "2027-W01"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("week order = %v, want %v chronological", got, want)
	}
}

func TestFilterReportTracksBoundaries(t *testing.T) {
	tracks := []generated.Recording{
		// 22:30Z is 00:30 the next local day east of Greenwich, 17:30 the same day west.
		reportTrack(1, nil, "2026-08-20T22:30:00Z", "2026-08-21T00:45:00Z"),
		// Exactly local midnight in UTC+2.
		reportTrack(2, nil, "2026-08-20T22:00:00Z", "2026-08-20T22:15:00Z"),
	}

	day := func(value string) time.Time {
		parsed, err := parseReportDay("--since", value)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}

	inLocalZone(t, 2)
	if matched := filterReportTracks(tracks, day("2026-08-21"), time.Time{}); len(matched) != 2 {
		t.Errorf("UTC+2 since 2026-08-21 matched %d tracks, want both (local start days 08-21)", len(matched))
	}
	if matched := filterReportTracks(tracks, day("2026-08-22"), time.Time{}); len(matched) != 0 {
		t.Errorf("UTC+2 since 2026-08-22 matched %d, want none", len(matched))
	}
	if matched := filterReportTracks(tracks, time.Time{}, day("2026-08-21")); len(matched) != 2 {
		t.Errorf("UTC+2 until 2026-08-21 matched %d, want both (inclusive)", len(matched))
	}
	if matched := filterReportTracks(tracks, day("2026-08-21"), day("2026-08-21")); len(matched) != 2 {
		t.Errorf("UTC+2 single-day window matched %d, want both", len(matched))
	}

	local := time.Local
	time.Local = time.FixedZone("UTC-5", -5*60*60)
	t.Cleanup(func() { time.Local = local })
	if matched := filterReportTracks(tracks[:1], day("2026-08-21"), time.Time{}); len(matched) != 0 {
		t.Errorf("UTC-5 since 2026-08-21 matched %d, want none: 22:30Z is still 08-20 locally", len(matched))
	}
}

func TestParseReportDay(t *testing.T) {
	inLocalZone(t, 2)
	if parsed, err := parseReportDay("--since", ""); err != nil || !parsed.IsZero() {
		t.Errorf("empty date = %v, %v; want the zero time", parsed, err)
	}
	parsed, err := parseReportDay("--until", "2026-08-21")
	if err != nil {
		t.Fatalf("parse date: %v", err)
	}
	want := time.Date(2026, 8, 21, 0, 0, 0, 0, time.Local)
	if !parsed.Equal(want) {
		t.Errorf("parsed = %v, want local midnight %v", parsed, want)
	}
	if _, err := parseReportDay("--since", "21/08/2026"); err == nil || !strings.Contains(err.Error(), "invalid since date") {
		t.Errorf("bad date error = %v, want the flag named", err)
	}
}

func TestTimetrackReportNotice(t *testing.T) {
	tests := []struct {
		name      string
		matched   int
		pages     int
		more      bool
		truncated bool
		want      string
	}{
		{name: "complete", matched: 3, pages: 3},
		{name: "first page", matched: 2, pages: 1, more: true, want: "Report covers 2 time tracks from the first page. Use --all to read every page."},
		{name: "cap", matched: 500, pages: 100, more: true, truncated: true, want: "Stopped after 100 pages, at 500 time tracks. Narrow the report with --category, --since or --until."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := timetrackReportNotice(test.matched, test.pages, test.more, test.truncated); got != test.want {
				t.Errorf("notice = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTimetrackReportGroupsByDayFromTheIndex(t *testing.T) {
	inLocalZone(t, 2)
	var asked []string
	pages := map[string]string{
		"":   timeTrackFirstPage,
		"b2": timeTrackSecondPage,
		"b3": `{"time_tracks":[],"categories":[]}`,
	}
	response, err := runJSONCommand(t, timeTrackIndexHandler(t, pages, &asked), "timetrack", "report", "--all")
	if err != nil {
		t.Fatalf("execute timetrack report: %v", err)
	}
	if strings.Join(asked, ",") != ",b2,b3" {
		t.Errorf("pages asked for = %#v, want all three", asked)
	}
	if response.Summary != "2 groups over 3 time tracks, 5:00:00 tracked" {
		t.Errorf("summary = %q", response.Summary)
	}

	rows := reportRows(t, response.Data)
	if len(rows) != 2 {
		t.Fatalf("rows = %#v, want two day groups", rows)
	}
	if rows[0].Group != "2026-08-19" || rows[1].Group != "2026-08-21" {
		t.Errorf("day order = %q, %q; want chronological", rows[0].Group, rows[1].Group)
	}
	friday := rows[1]
	if friday.Records != 2 || friday.Duration != "4:30:00" {
		t.Errorf("Friday group = %d records over %s, want 2 and 4:30:00", friday.Records, friday.Duration)
	}
	// Both tracks started on the reader's 08-21, including the one HEY stamps 22:30Z.
	if friday.FirstStart != "2026-08-21T00:30:00+02:00" {
		t.Errorf("first_start = %q, want local 00:30", friday.FirstStart)
	}
	if friday.LastEnd != "2026-08-21T13:15:00+02:00" {
		t.Errorf("last_end = %q, want local 13:15", friday.LastEnd)
	}
	if pagesFetched, _ := response.Meta["pages_fetched"].(float64); pagesFetched != 3 {
		t.Errorf("pages_fetched = %v, want 3", response.Meta["pages_fetched"])
	}
}

func TestTimetrackReportGroupsByCategory(t *testing.T) {
	inLocalZone(t, 2)
	pages := map[string]string{"": timeTrackFirstPage, "b2": timeTrackSecondPage}
	response, err := runJSONCommand(t, timeTrackIndexHandler(t, pages, new([]string)), "timetrack", "report", "--group-by", "category", "--all")
	if err != nil {
		t.Fatalf("execute timetrack report --group-by category: %v", err)
	}

	rows := reportRows(t, response.Data)
	var labels []string
	for _, row := range rows {
		labels = append(labels, row.Group)
	}
	if strings.Join(labels, ",") != "Client work,Planning,"+unfiledCategoryGroup {
		t.Errorf("category groups = %v", labels)
	}
	if rows[0].Duration != "2:15:00" || rows[0].Records != 1 {
		t.Errorf("client group = %#v, want 1 record of 2:15:00", rows[0])
	}
}

func TestTimetrackReportSinceUntilFiltersLocally(t *testing.T) {
	inLocalZone(t, 2)
	pages := map[string]string{"": timeTrackFirstPage, "b2": timeTrackSecondPage}

	since, err := runJSONCommand(t, timeTrackIndexHandler(t, pages, new([]string)), "timetrack", "report", "--group-by", "day", "--since", "2026-08-21", "--all")
	if err != nil {
		t.Fatalf("report --since: %v", err)
	}
	rows := reportRows(t, since.Data)
	if len(rows) != 1 || rows[0].Group != "2026-08-21" || rows[0].Records != 2 {
		t.Fatalf("since rows = %#v, want one 2026-08-21 group with 2 tracks", rows)
	}
}

// West of Greenwich the 22:30Z track is still the reader's 08-20, so an --until on that
// day keeps it; only the Friday track drops out.
func TestTimetrackReportUntilFilterWestOfGreenwich(t *testing.T) {
	inLocalZone(t, -5)
	pages := map[string]string{"": timeTrackFirstPage, "b2": timeTrackSecondPage}
	until, err := runJSONCommand(t, timeTrackIndexHandler(t, pages, new([]string)), "timetrack", "report", "--group-by", "day", "--until", "2026-08-20", "--all")
	if err != nil {
		t.Fatalf("report --until: %v", err)
	}
	var days []string
	for _, row := range reportRows(t, until.Data) {
		days = append(days, row.Group)
	}
	if strings.Join(days, ",") != "2026-08-19,2026-08-20" {
		t.Fatalf("until days = %v, want 2026-08-19 and 2026-08-20, the Friday track excluded", days)
	}
}

func TestTimetrackReportEmptyResult(t *testing.T) {
	pages := map[string]string{"": timeTrackFirstPage, "b2": timeTrackSecondPage}
	response, err := runJSONCommand(t, timeTrackIndexHandler(t, pages, new([]string)), "timetrack", "report", "--since", "2030-01-01")
	if err != nil {
		t.Fatalf("execute empty report: %v", err)
	}
	if response.Summary != "No time tracks matched" {
		t.Errorf("summary = %q", response.Summary)
	}
	rows := reportRows(t, response.Data)
	if len(rows) != 0 {
		t.Errorf("rows = %#v, want none", rows)
	}
}

func TestTimetrackReportReadsOnePageByDefault(t *testing.T) {
	var asked []string
	pages := map[string]string{"": timeTrackFirstPage, "b2": timeTrackSecondPage}
	response, err := runJSONCommand(t, timeTrackIndexHandler(t, pages, &asked), "timetrack", "report")
	if err != nil {
		t.Fatalf("execute timetrack report: %v", err)
	}
	if len(asked) != 1 || asked[0] != "" {
		t.Errorf("pages asked for = %#v, want the first page only", asked)
	}
	if !strings.Contains(response.Notice, "Use --all to read every page") {
		t.Errorf("notice = %q", response.Notice)
	}
}

func TestTimetrackReportTruncatedAtHundredPages(t *testing.T) {
	var requests atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		page := r.URL.Query().Get("page")
		dayNumber := 1
		if page != "" {
			if _, err := fmt.Sscanf(page, "p%d", &dayNumber); err != nil {
				t.Errorf("bad cursor %q", page)
			}
		}
		// A distinct day per page keeps the groups apart, and every page — the hundredth
		// included — promises another one, so the walk reaches the cap mid-list.
		day := time.Date(2026, 8, dayNumber, 9, 0, 0, 0, time.UTC)
		body := fmt.Sprintf(`{"time_tracks":[{"id":1,"type":"Calendar::TimeTrack","starts_at":%q,"ends_at":%q}],"categories":[]}`,
			day.Format(time.RFC3339), day.Add(time.Hour).Format(time.RFC3339))
		w.Header().Set("Link", `<`+r.URL.Path+`?page=p`+fmt.Sprintf("%d", dayNumber+1)+`>; rel="next"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})

	response, err := runJSONCommand(t, handler, "timetrack", "report", "--all", "--group-by", "day")
	if err != nil {
		t.Fatalf("execute truncated report: %v", err)
	}
	if requests.Load() != 100 {
		t.Errorf("requests = %d, want the 100-page cap", requests.Load())
	}
	if !strings.Contains(response.Notice, "Stopped after 100 pages") {
		t.Errorf("notice = %q", response.Notice)
	}

	// CSV keeps the truncation notice on stderr so stdout stays a clean CSV.
	stdout, stderr, err := runFormattedCommandWithStderr(t, handler, nil, "timetrack", "report", "--all", "--csv")
	if err != nil {
		t.Fatalf("execute truncated csv report: %v", err)
	}
	parsed, parseErr := csv.NewReader(strings.NewReader(stdout)).ReadAll()
	if parseErr != nil {
		t.Fatalf("parse csv: %v", parseErr)
	}
	if len(parsed) != 101 {
		t.Errorf("csv rows = %d, want header plus 100 groups", len(parsed))
	}
	if !strings.Contains(stderr, "Stopped after 100 pages") {
		t.Errorf("stderr = %q, want the truncation notice", stderr)
	}
	if strings.Contains(stdout, "Stopped after") {
		t.Errorf("stdout carries the notice:\n%s", stdout)
	}
}

func TestTimetrackReportSendsCategoryID(t *testing.T) {
	var requests atomic.Int32
	response, err := runJSONCommand(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.URL.Query().Get("category_id"); got != "42" {
			t.Errorf("category_id = %q, want 42", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, timeTrackSecondPage)
	}), "timetrack", "report", "--group-by", "category", "--category", "42")
	if err != nil {
		t.Fatalf("execute report --category: %v", err)
	}
	if requests.Load() != 1 {
		t.Errorf("requests = %d, want 1", requests.Load())
	}
	rows := reportRows(t, response.Data)
	if len(rows) != 1 || rows[0].Group != "Planning" {
		t.Errorf("rows = %#v, want the Planning group only", rows)
	}
}

func TestTimetrackReportUnknownCategory(t *testing.T) {
	_, err := runJSONCommand(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}), "timetrack", "report", "--category", "999")
	if err == nil || !strings.Contains(err.Error(), `time track category "999" not found`) {
		t.Fatalf("error = %v, want the category reported missing", err)
	}
}

func TestTimetrackReportRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "group by", args: []string{"timetrack", "report", "--group-by", "fortnight"}, want: "--group-by takes one of"},
		{name: "category zero", args: []string{"timetrack", "report", "--category", "0"}, want: "--category takes a category ID"},
		{name: "bad since", args: []string{"timetrack", "report", "--since", "last week"}, want: "invalid since date"},
		{name: "bad until", args: []string{"timetrack", "report", "--until", "nope"}, want: "invalid until date"},
		{name: "order", args: []string{"timetrack", "report", "--since", "2026-09-01", "--until", "2026-08-01"}, want: "--until 2026-08-01 is before --since 2026-09-01"},
		{name: "csv and json", args: []string{"timetrack", "report", "--csv", "--json"}, want: "cannot combine with output formatting flags"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			_, err := runJSONCommand(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
			}), test.args...)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if requests.Load() != 0 {
				t.Errorf("requests = %d, want none", requests.Load())
			}
		})
	}
}

func TestTimetrackReportCSV(t *testing.T) {
	inLocalZone(t, 2)
	pages := map[string]string{
		"":   timeTrackFirstPage,
		"b2": timeTrackSecondPage,
		"b3": `{"time_tracks":[],"categories":[]}`,
	}
	stdout, err := runFormattedCommand(t, timeTrackIndexHandler(t, pages, new([]string)), nil, "timetrack", "report", "--group-by", "category", "--all", "--csv")
	if err != nil {
		t.Fatalf("execute report --csv: %v", err)
	}
	rows, parseErr := csv.NewReader(strings.NewReader(stdout)).ReadAll()
	if parseErr != nil {
		t.Fatalf("parse csv: %v", parseErr)
	}
	if strings.Join(rows[0], ",") != "group,records,duration,first_start,last_end" {
		t.Errorf("header = %v", rows[0])
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %v, want header plus three groups", rows)
	}
	client := rows[1]
	if client[0] != "Client work" || client[1] != "1" || client[2] != "2:15:00" {
		t.Errorf("client row = %v", client)
	}
	if client[3] != "2026-08-21T11:00:00+02:00" || client[4] != "2026-08-21T13:15:00+02:00" {
		t.Errorf("client range = %q..%q", client[3], client[4])
	}
}

func TestTimetrackReportCSVEmpty(t *testing.T) {
	pages := map[string]string{"": `{"time_tracks":[],"categories":[]}`}
	stdout, err := runFormattedCommand(t, timeTrackIndexHandler(t, pages, new([]string)), nil, "timetrack", "report", "--csv")
	if err != nil {
		t.Fatalf("execute empty report --csv: %v", err)
	}
	rows, parseErr := csv.NewReader(strings.NewReader(strings.TrimSpace(stdout))).ReadAll()
	if parseErr != nil {
		t.Fatalf("parse csv: %v", parseErr)
	}
	if len(rows) != 1 || strings.Join(rows[0], ",") != "group,records,duration,first_start,last_end" {
		t.Errorf("empty csv = %v, want the header only", rows)
	}
}

func TestTimetrackReportMarkdown(t *testing.T) {
	pages := map[string]string{"": timeTrackFirstPage, "b2": timeTrackSecondPage}
	stdout, err := runFormattedCommand(t, timeTrackIndexHandler(t, pages, new([]string)), []string{"--markdown"}, "timetrack", "report", "--group-by", "category", "--all")
	if err != nil {
		t.Fatalf("execute report --markdown: %v", err)
	}
	// The generic Markdown table names the JSON keys, alphabetically ordered.
	if !strings.Contains(stdout, "| duration | first_start | group | last_end | records |") {
		t.Errorf("markdown missing its header:\n%s", stdout)
	}
	if !strings.Contains(stdout, "| Client work |") {
		t.Errorf("markdown missing the group row:\n%s", stdout)
	}
}

func TestTimetrackReportStyled(t *testing.T) {
	inLocalZone(t, 2)
	pages := map[string]string{"": timeTrackFirstPage, "b2": timeTrackSecondPage}
	stdout, err := runStyledCommand(t, timeTrackIndexHandler(t, pages, new([]string)), "timetrack", "report", "--group-by", "week", "--all")
	if err != nil {
		t.Fatalf("execute styled report: %v", err)
	}
	for _, want := range []string{"Group", "Records", "Duration", "First start", "Last end", "2026-W34", "3", "5:00:00"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("styled table missing %q:\n%s", want, stdout)
		}
	}
}
