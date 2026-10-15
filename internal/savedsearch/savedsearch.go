// Package savedsearch stores named search condition sets in the hey-cli state
// directory. One JSON file per account scope keeps a user's saved queries for
// different linked accounts apart; every write is atomic and fields this
// version does not know about are carried through, so a query a newer hey-cli
// saved stays readable and migratable.
package savedsearch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

const (
	dirName     = "saved-searches"
	fileVersion = 1
)

var (
	// ErrNotFound means no query with that name exists for the account scope.
	ErrNotFound = errors.New("saved search not found")
	// ErrExists means a query with that name already exists and force was not set.
	ErrExists = errors.New("saved search already exists")

	scopePattern = regexp.MustCompile(`^[a-z0-9_-]+$`)
)

// Conditions is the saved subset of hey.SearchParams — the criteria, without
// the execution options (page, all).
type Conditions struct {
	Query       string `json:"query,omitempty"`
	Required    string `json:"required,omitempty"`
	Any         string `json:"any,omitempty"`
	None        string `json:"none,omitempty"`
	ExactPhrase string `json:"exact_phrase,omitempty"`
	From        string `json:"from,omitempty"`
	To          string `json:"to,omitempty"`
	Subject     string `json:"subject,omitempty"`
	Date        string `json:"date,omitempty"`
	In          string `json:"in,omitempty"`
	Label       string `json:"label,omitempty"`
	Attachment  string `json:"attachment,omitempty"`
}

// Query is one named, saved condition set. Sources records where each
// parameter came from when it was saved (the positional argument or the flag
// that supplied it), keyed by the parameter's JSON name.
type Query struct {
	Name      string            `json:"name"`
	CreatedAt time.Time         `json:"created_at"`
	Sources   map[string]string `json:"sources,omitempty"`
	Conditions
}

// Store reads and writes saved searches under one state directory.
type Store struct {
	stateDir string
}

func New(stateDir string) *Store {
	return &Store{stateDir: stateDir}
}

// Save stores query under its name for the account scope. An existing name is
// refused with ErrExists unless force is true; overwriting keeps the previous
// record's unknown fields rather than dropping state this version does not
// understand.
func (s *Store) Save(account string, query Query, force bool) error {
	if s.stateDir == "" {
		return errors.New("could not determine hey-cli state directory")
	}
	path, err := s.scopePath(account)
	if err != nil {
		return err
	}

	document, err := readDocument(path)
	if err != nil {
		return err
	}
	if document == nil {
		document = map[string]json.RawMessage{}
	}
	queries, err := queriesMap(document)
	if err != nil {
		return err
	}
	var previous json.RawMessage
	if existing, exists := queries[query.Name]; exists {
		if !force {
			return fmt.Errorf("%w: %q", ErrExists, query.Name)
		}
		previous = existing
	}

	encoded, err := marshalQuery(query, previous)
	if err != nil {
		return fmt.Errorf("could not marshal saved search %q: %w", query.Name, err)
	}
	queries[query.Name] = encoded
	document["version"], _ = json.Marshal(fileVersion)
	document["queries"], err = json.Marshal(queries)
	if err != nil {
		return fmt.Errorf("could not encode saved searches: %w", err)
	}

	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("could not marshal saved searches: %w", err)
	}
	if err := writeFileAtomic(path, append(data, '\n')); err != nil {
		return err
	}
	return nil
}

// Get loads one named query. A missing scope file or name answers ErrNotFound;
// a file that cannot be parsed answers an error naming the file.
func (s *Store) Get(account, name string) (Query, error) {
	var query Query
	path, err := s.scopePath(account)
	if err != nil {
		return query, err
	}
	document, err := readDocument(path)
	if err != nil {
		return query, err
	}
	if document == nil {
		return query, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	queries, err := queriesMap(document)
	if err != nil {
		return query, err
	}
	raw, ok := queries[name]
	if !ok {
		return query, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if err := json.Unmarshal(raw, &query); err != nil {
		return query, fmt.Errorf("saved search %q in %s is unreadable: %w", name, path, err)
	}
	return query, nil
}

// List returns every saved query for the account scope sorted by name. A
// missing scope file answers an empty list.
func (s *Store) List(account string) ([]Query, error) {
	path, err := s.scopePath(account)
	if err != nil {
		return nil, err
	}
	document, err := readDocument(path)
	if err != nil {
		return nil, err
	}
	if document == nil {
		return []Query{}, nil
	}
	queries, err := queriesMap(document)
	if err != nil {
		return nil, err
	}
	list := make([]Query, 0, len(queries))
	for name, raw := range queries {
		var query Query
		if err := json.Unmarshal(raw, &query); err != nil {
			return nil, fmt.Errorf("saved search %q in %s is unreadable: %w", name, path, err)
		}
		list = append(list, query)
	}
	slices.SortFunc(list, func(a, b Query) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return list, nil
}

// Delete removes a named query. A missing scope file or name answers
// ErrNotFound.
func (s *Store) Delete(account, name string) error {
	path, err := s.scopePath(account)
	if err != nil {
		return err
	}
	document, err := readDocument(path)
	if err != nil {
		return err
	}
	if document == nil {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	queries, err := queriesMap(document)
	if err != nil {
		return err
	}
	if _, ok := queries[name]; !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	delete(queries, name)
	if len(queries) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("could not remove %s: %w", path, err)
		}
		return nil
	}
	document["queries"], err = json.Marshal(queries)
	if err != nil {
		return fmt.Errorf("could not encode saved searches: %w", err)
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("could not marshal saved searches: %w", err)
	}
	return writeFileAtomic(path, append(data, '\n'))
}

// Path reports the scope file, exposed so callers can name it in errors.
func (s *Store) Path(account string) (string, error) {
	return s.scopePath(account)
}

func (s *Store) scopePath(account string) (string, error) {
	if account == "" {
		return "", errors.New("no account scope for saved searches")
	}
	if !scopePattern.MatchString(account) {
		return "", fmt.Errorf("unsafe account scope for saved searches: %q", account)
	}
	return filepath.Join(s.stateDir, dirName, account+".json"), nil
}

// readDocument returns the raw top-level JSON object so keys this version does
// not know survive a rewrite. A missing file is not an error.
func readDocument(path string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is built inside the hey-cli state directory
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("could not read saved searches %s: %w", path, err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("saved searches file %s is corrupted: %w", path, err)
	}
	if document == nil {
		return nil, fmt.Errorf("saved searches file %s is corrupted: expected a JSON object", path)
	}
	return document, nil
}

func queriesMap(document map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	raw, ok := document["queries"]
	if !ok {
		return map[string]json.RawMessage{}, nil
	}
	var queries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &queries); err != nil {
		return nil, fmt.Errorf("saved searches file has an unreadable queries object: %w", err)
	}
	return queries, nil
}

// marshalQuery encodes query on top of the previous raw record, so keys a
// newer hey-cli wrote that this version does not model survive an overwrite by
// this version. Current keys always win; only unknown keys are carried over.
func marshalQuery(query Query, previous json.RawMessage) (json.RawMessage, error) {
	merged := map[string]json.RawMessage{}
	if len(previous) > 0 {
		_ = json.Unmarshal(previous, &merged)
	}
	known, err := json.Marshal(query)
	if err != nil {
		return nil, err
	}
	var knownMap map[string]json.RawMessage
	if err := json.Unmarshal(known, &knownMap); err != nil {
		return nil, err
	}
	for key, value := range knownMap {
		merged[key] = value
	}
	return json.Marshal(merged)
}

// writeFileAtomic writes to a temp file in the destination directory, syncs it,
// and renames it into place, so a crash can never leave a half-written saved
// searches file.
func writeFileAtomic(path string, data []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("could not create saved searches directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil { //nolint:gosec // G302: tightening the state directory to 0700
		return fmt.Errorf("could not secure saved searches directory: %w", err)
	}

	temporary, err := os.CreateTemp(directory, ".saved-searches-*")
	if err != nil {
		return fmt.Errorf("could not create temporary saved searches file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("could not secure temporary saved searches file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("could not write temporary saved searches file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("could not sync temporary saved searches file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("could not close temporary saved searches file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil { //nolint:gosec // G703: both paths are inside the state directory
		return fmt.Errorf("could not replace saved searches file: %w", err)
	}
	return nil
}
