package cmd

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	hey "github.com/basecamp/hey-sdk/go/pkg/hey"

	"github.com/basecamp/hey-cli/internal/apierr"
	"github.com/basecamp/hey-cli/internal/config"
	"github.com/basecamp/hey-cli/internal/output"
	"github.com/basecamp/hey-cli/internal/savedsearch"
)

func savedSearchStore() *savedsearch.Store {
	return savedsearch.New(config.StateDir())
}

func savedConditionsFromParams(params hey.SearchParams) savedsearch.Conditions {
	return savedsearch.Conditions{
		Query:       params.Query,
		Required:    params.Required,
		Any:         params.Any,
		None:        params.None,
		ExactPhrase: params.ExactPhrase,
		From:        params.From,
		To:          params.To,
		Subject:     params.Subject,
		Date:        params.Date,
		In:          params.In,
		Label:       params.Label,
		Attachment:  params.Attachment,
	}
}

func searchParamsFromSaved(conditions savedsearch.Conditions, page int) hey.SearchParams {
	return hey.SearchParams{
		Query:       conditions.Query,
		Page:        page,
		Required:    conditions.Required,
		Any:         conditions.Any,
		None:        conditions.None,
		ExactPhrase: conditions.ExactPhrase,
		From:        conditions.From,
		To:          conditions.To,
		Subject:     conditions.Subject,
		Date:        conditions.Date,
		In:          conditions.In,
		Label:       conditions.Label,
		Attachment:  conditions.Attachment,
	}
}

// --- save -----------------------------------------------------------------

type searchSaveCommand struct {
	cmd *cobra.Command

	searchConditions
	name  string
	force bool
}

func newSearchSaveCommand() *searchSaveCommand {
	c := &searchSaveCommand{}
	c.cmd = &cobra.Command{
		Use:   "save [query]",
		Short: "Save a named set of search conditions",
		Long: "Save the free-text query and search refinements given on this command under a name, " +
			"so `hey search run <name>` can replay them without retyping. Accepts the same conditions " +
			"as `hey search`. A saved name is refused unless --force overwrites it. Saved searches " +
			"live in the hey-cli state directory and are scoped to the active mail account.",
		Annotations: map[string]string{
			"agent_notes": "Stores the conditions locally per active mail account; no request is sent and no login is needed. The saved record carries the parameter sources and its creation time. Overwrite an existing name with --force. Replay with `hey search run <name>` and inspect with `hey search list`.",
		},
		Example: `  hey search save --name invoices --subject invoice --attachment pdfs
  hey search save "quarterly planning" --from jane@example.com --name planning
  hey search save --name invoices --subject bill --force`,
		RunE: c.run,
		Args: cobra.MaximumNArgs(1),
	}
	flags := c.cmd.Flags()
	addSearchConditionFlags(flags, &c.searchConditions)
	flags.StringVar(&c.name, "name", "", "Name to save the search under (required)")
	flags.BoolVar(&c.force, "force", false, "Overwrite a saved search with the same name")
	return c
}

func (c *searchSaveCommand) run(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(c.name)
	if name == "" {
		return apierr.ErrUsage("--name is required to save a search")
	}

	query := ""
	if len(args) == 1 {
		query = args[0]
	}
	params := c.searchConditions.params(query, 1)
	if err := validateSearchRequest(params); err != nil {
		return err
	}

	sources := map[string]string{}
	if params.Query != "" {
		sources["query"] = "argument"
	}
	for _, pair := range searchConditionFlagNames {
		if cmd.Flags().Changed(pair.flag) {
			sources[pair.key] = "--" + pair.flag
		}
	}

	record := savedsearch.Query{
		Name:       name,
		CreatedAt:  time.Now().UTC(),
		Sources:    sources,
		Conditions: savedConditionsFromParams(params),
	}
	if err := savedSearchStore().Save(cfg.AccountID, record, c.force); err != nil {
		if errors.Is(err, savedsearch.ErrExists) {
			return apierr.ErrUsageHint(err.Error(), "Re-run with --force to overwrite the saved search.")
		}
		return err
	}

	return writeMutation(cmd, fmt.Sprintf("Saved search %q", name), record,
		output.WithBreadcrumbs(
			output.Breadcrumb{Action: "run", Command: "hey search run " + name, Description: "Run the saved search"},
			output.Breadcrumb{Action: "list", Command: "hey search list", Description: "List saved searches"},
		),
	)
}

// --- list -----------------------------------------------------------------

type searchListCommand struct {
	cmd *cobra.Command
}

type savedSearchListItem struct {
	Name string `json:"name"`
	savedsearch.Conditions
	CreatedAt time.Time         `json:"created_at"`
	Sources   map[string]string `json:"sources,omitempty"`
}

func newSearchListCommand() *searchListCommand {
	c := &searchListCommand{}
	c.cmd = &cobra.Command{
		Use:   "list",
		Short: "List saved searches",
		Long:  "List the named searches saved for the active mail account, alphabetically by name.",
		Annotations: map[string]string{
			"agent_notes": "Returns the saved searches for the active mail account, each with its saved conditions, creation time, and parameter sources. No request is sent and no login is needed. Run one with `hey search run <name>`.",
		},
		Example: `  hey search list
  hey search list --json`,
		RunE: c.run,
		Args: cobra.NoArgs,
	}
	return c
}

func (c *searchListCommand) run(cmd *cobra.Command, _ []string) error {
	queries, err := savedSearchStore().List(cfg.AccountID)
	if err != nil {
		return err
	}

	items := make([]savedSearchListItem, 0, len(queries))
	for _, query := range queries {
		items = append(items, savedSearchListItem{
			Name:       query.Name,
			Conditions: query.Conditions,
			CreatedAt:  query.CreatedAt,
			Sources:    query.Sources,
		})
	}

	if writer.IsStyled() {
		if len(items) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No saved searches")
			return nil
		}
		table := newTable(cmd.OutOrStdout())
		table.addRow([]string{"Name", "Conditions", "Saved"})
		for _, item := range items {
			table.addRow([]string{item.Name, truncate(savedConditionsSummary(item.Conditions), 60), formatDate(item.CreatedAt)})
		}
		table.print()
		return nil
	}

	return writeOK(items,
		output.WithSummary(fmt.Sprintf("%d saved %s", len(items), savedSearchNoun(len(items)))),
		output.WithBreadcrumbs(
			output.Breadcrumb{Action: "run", Command: "hey search run <name>", Description: "Run a saved search"},
		),
	)
}

func savedSearchNoun(count int) string {
	if count == 1 {
		return "search"
	}
	return "searches"
}

func savedConditionsSummary(c savedsearch.Conditions) string {
	var parts []string
	add := func(key, value string) {
		if value != "" {
			parts = append(parts, key+"="+value)
		}
	}
	add("query", c.Query)
	add("required", c.Required)
	add("any", c.Any)
	add("none", c.None)
	add("exact", c.ExactPhrase)
	add("from", c.From)
	add("to", c.To)
	add("subject", c.Subject)
	add("date", c.Date)
	add("in", c.In)
	add("label", c.Label)
	add("attachment", c.Attachment)
	return strings.Join(parts, "; ")
}

// --- run ------------------------------------------------------------------

type searchRunCommand struct {
	cmd *cobra.Command

	searchConditions
	page int
	all  bool
}

func newSearchRunCommand() *searchRunCommand {
	c := &searchRunCommand{}
	c.cmd = &cobra.Command{
		Use:   "run <name> [query]",
		Short: "Run a saved search",
		Long: "Run the conditions saved under <name>. Any condition supplied on the command line " +
			"overrides the saved condition of the same name; an optional free-text [query] overrides " +
			"the saved free text. Conditions not given here stay as they were saved.",
		Annotations: map[string]string{
			"agent_notes": "Loads the named saved search for the active mail account and sends one search request, so login is required. Command-line conditions (including a positional query) override same-named saved conditions; omitted ones come from the saved record. Supports --page and --all like `hey search`. A missing name or a corrupted saved-searches file fails with a clear error.",
		},
		Example: `  hey search run invoices
  hey search run invoices --date last_7_days
  hey search run invoices "overdue" --from mike@example.org --all`,
		RunE: c.run,
		Args: cobra.RangeArgs(1, 2),
	}
	flags := c.cmd.Flags()
	addSearchConditionFlags(flags, &c.searchConditions)
	flags.IntVar(&c.page, "page", 1, "Results page")
	flags.BoolVar(&c.all, "all", false, "Fetch up to 100 results pages from --page onward")
	return c
}

func (c *searchRunCommand) run(cmd *cobra.Command, args []string) error {
	if err := requireAuth(); err != nil {
		return err
	}

	name := args[0]
	query, err := savedSearchStore().Get(cfg.AccountID, name)
	if err != nil {
		if errors.Is(err, savedsearch.ErrNotFound) {
			return apierr.ErrNotFoundHint("Saved search", name, "List saved searches with `hey search list`.")
		}
		return err
	}

	conditions := query.Conditions
	if len(args) == 2 {
		conditions.Query = strings.TrimSpace(args[1])
	}
	applySavedSearchOverrides(cmd, &c.searchConditions, &conditions)

	params := searchParamsFromSaved(conditions, c.page)
	if err := validateSearchRequest(params); err != nil {
		return err
	}
	return executeSearch(cmd, params, c.all)
}

// applySavedSearchOverrides copies each refinement the command line explicitly
// changed over the saved conditions. Unchanged flags leave the saved value in
// place, so `run x --date last_7_days` narrows without clearing what was saved.
func applySavedSearchOverrides(cmd *cobra.Command, flags *searchConditions, conditions *savedsearch.Conditions) {
	changed := cmd.Flags()
	if changed.Changed("required") {
		conditions.Required = strings.TrimSpace(flags.required)
	}
	if changed.Changed("any") {
		conditions.Any = strings.TrimSpace(flags.any)
	}
	if changed.Changed("none") {
		conditions.None = strings.TrimSpace(flags.none)
	}
	if changed.Changed("exact") {
		conditions.ExactPhrase = strings.TrimSpace(flags.exact)
	}
	if changed.Changed("from") {
		conditions.From = strings.TrimSpace(flags.from)
	}
	if changed.Changed("to") {
		conditions.To = strings.TrimSpace(flags.to)
	}
	if changed.Changed("subject") {
		conditions.Subject = strings.TrimSpace(flags.subject)
	}
	if changed.Changed("date") {
		conditions.Date = strings.TrimSpace(flags.date)
	}
	if changed.Changed("in") {
		conditions.In = strings.TrimSpace(flags.in)
	}
	if changed.Changed("label") {
		conditions.Label = strings.TrimSpace(flags.label)
	}
	if changed.Changed("attachment") {
		conditions.Attachment = strings.TrimSpace(flags.attachment)
	}
}

// --- delete ---------------------------------------------------------------

type searchDeleteCommand struct {
	cmd *cobra.Command
}

func newSearchDeleteCommand() *searchDeleteCommand {
	c := &searchDeleteCommand{}
	c.cmd = &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a saved search",
		Long:  "Delete the named saved search from the active mail account's saved searches.",
		Annotations: map[string]string{
			"agent_notes": "Removes the saved search locally for the active mail account without sending any request; no login is needed. A missing name fails as not_found.",
		},
		Example: `  hey search delete invoices`,
		RunE:    c.run,
		Args:    cobra.ExactArgs(1),
	}
	return c
}

func (c *searchDeleteCommand) run(cmd *cobra.Command, args []string) error {
	name := args[0]
	if err := savedSearchStore().Delete(cfg.AccountID, name); err != nil {
		if errors.Is(err, savedsearch.ErrNotFound) {
			return apierr.ErrNotFoundHint("Saved search", name, "List saved searches with `hey search list`.")
		}
		return err
	}
	return writeMutation(cmd, fmt.Sprintf("Deleted saved search %q", name),
		map[string]string{"name": name})
}
