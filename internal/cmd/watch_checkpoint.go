package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	hey "github.com/basecamp/hey-sdk/go/pkg/hey"

	"github.com/basecamp/hey-cli/internal/config"
)

const (
	watchCheckpointVersion = 1

	// watchSeenEventLimit bounds how many handed-on event keys are remembered, in memory
	// and in the checkpoint. A resumed watch only re-meets changes from the last batch
	// before its cursor, so a few thousand keys overlap that window many times over.
	watchSeenEventLimit = 2048

	// watchCheckpointThreadLimit bounds the thread activity written with a checkpoint.
	// The in-memory record stays whole for the run; only the file is trimmed, and the
	// entries dropped are the least recently active ones — a thread active again has a
	// newer record and is kept.
	watchCheckpointThreadLimit = 4096
)

// watchScope names whose changes a checkpoint holds: the HEY server and the linked
// account selected for the watch ("all" when none was). A checkpoint from another
// scope is ignored, never restored over this watch.
type watchScope struct {
	origin  string
	account string
}

func watchScopeFor(baseURL, account string) (watchScope, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return watchScope{}, fmt.Errorf("could not scope a watch checkpoint to %q: %w", baseURL, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return watchScope{}, fmt.Errorf("could not scope a watch checkpoint to %q: missing scheme or host", baseURL)
	}
	if account == "" {
		account = config.AllAccounts
	}
	return watchScope{origin: parsed.Scheme + "://" + strings.ToLower(parsed.Host), account: account}, nil
}

// token is the checkpoint's file name: unreadable to a person, unlike the scope inside,
// but unique per server and account without putting either in the path.
func (s watchScope) token() string {
	sum := sha256.Sum256([]byte(s.origin + "\x00" + s.account))
	return hex.EncodeToString(sum[:])[:16]
}

// watchCheckpointFile is the checkpoint on disk. Cursors are the pair the feeds speak —
// a millisecond since and the version the server issued — and the rest is the watch's
// own record: each thread's last activity, the floors skip-aheads set, and the keys of
// the changes already handed on.
type watchCheckpointFile struct {
	Version      int                         `json:"version"`
	Scope        watchCheckpointScope        `json:"scope"`
	UpdatedAt    string                      `json:"updated_at"`
	ReadyAt      string                      `json:"ready_at,omitempty"`
	Boxes        map[string]watchSavedCursor `json:"boxes,omitempty"`
	Calendars    map[string]watchSavedCursor `json:"calendars,omitempty"`
	CalendarList *watchSavedCursor           `json:"calendar_list,omitempty"`
	Threads      map[string]string           `json:"threads,omitempty"`
	Floors       map[string]string           `json:"floors,omitempty"`
	SeenEvents   []string                    `json:"seen_events,omitempty"`
}

type watchCheckpointScope struct {
	Origin  string `json:"origin"`
	Account string `json:"account"`
}

type watchSavedCursor struct {
	Since   string `json:"since"`
	Version string `json:"version,omitempty"`
}

// watchCheckpoint is a watch's persisted position. It is restored once, when the file
// belongs to this account, and rewritten atomically after every confirmed batch and
// every ready. A watch started with --since starts with nothing restored — the explicit
// time wins — but still writes as it catches up; --no-checkpoint neither reads nor
// writes.
type watchCheckpoint struct {
	path     string
	scope    watchScope
	persist  bool
	restored *watchCheckpointFile
	readyAt  string
	warned   bool
}

func openWatchCheckpoint(baseURL, account, since string, disabled bool) (*watchCheckpoint, error) {
	scope, err := watchScopeFor(baseURL, account)
	if err != nil {
		return nil, err
	}
	cp := &watchCheckpoint{scope: scope}
	if dir := config.StateDir(); dir != "" {
		cp.path = filepath.Join(dir, "watch", "checkpoint-"+scope.token()+".json")
		cp.persist = !disabled
	}
	if disabled || since != "" {
		return cp, nil
	}
	cp.restored = cp.read()
	if cp.restored != nil {
		cp.readyAt = cp.restored.ReadyAt
	}
	return cp, nil
}

// read loads the checkpoint, or nil when there is none worth restoring: no file, a torn
// write, an unknown format, or a checkpoint from another server or account. None of
// those is the watch's problem — it starts the way it would with no file at all.
func (cp *watchCheckpoint) read() *watchCheckpointFile {
	data, err := os.ReadFile(cp.path) // #nosec G304 -- the path is derived from the effective server and account scope
	if err != nil {
		return nil
	}
	var file watchCheckpointFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil
	}
	if file.Version != watchCheckpointVersion ||
		file.Scope.Origin != cp.scope.origin || file.Scope.Account != cp.scope.account {
		return nil
	}
	if !validSavedCursors(file.Boxes) || !validSavedCursors(file.Calendars) ||
		(file.CalendarList != nil && !validSavedCursor(*file.CalendarList)) {
		return nil
	}
	if !validSavedTimes(file.Threads) || !validSavedTimes(file.Floors) {
		return nil
	}
	return &file
}

func validSavedCursors(cursors map[string]watchSavedCursor) bool {
	for id, cursor := range cursors {
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			return false
		}
		if !validSavedCursor(cursor) {
			return false
		}
	}
	return true
}

func validSavedCursor(cursor watchSavedCursor) bool {
	_, err := time.Parse(watchCursorTimeLayout, cursor.Since)
	return err == nil
}

func validSavedTimes(times map[string]string) bool {
	for id, at := range times {
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			return false
		}
		if _, err := time.Parse(time.RFC3339, at); err != nil {
			return false
		}
	}
	return true
}

// The restore accessors answer empty maps when nothing was restored, so the call sites
// ask one question rather than knowing which runs start fresh.

func (cp *watchCheckpoint) boxCursors() map[int64]hey.PostingChangesCursor {
	restored := map[int64]hey.PostingChangesCursor{}
	if cp == nil || cp.restored == nil {
		return restored
	}
	for id, cursor := range cp.restored.Boxes {
		boxID, _ := strconv.ParseInt(id, 10, 64)
		restored[boxID] = hey.PostingChangesCursor{Since: cursor.Since, Version: cursor.Version}
	}
	return restored
}

func (cp *watchCheckpoint) calendarCursors() map[int64]hey.CalendarChangesCursor {
	restored := map[int64]hey.CalendarChangesCursor{}
	if cp == nil || cp.restored == nil {
		return restored
	}
	for id, cursor := range cp.restored.Calendars {
		calendarID, _ := strconv.ParseInt(id, 10, 64)
		restored[calendarID] = hey.CalendarChangesCursor{Since: cursor.Since, Version: cursor.Version}
	}
	return restored
}

func (cp *watchCheckpoint) calendarListCursor() *hey.CalendarChangesCursor {
	if cp == nil || cp.restored == nil || cp.restored.CalendarList == nil {
		return nil
	}
	return &hey.CalendarChangesCursor{Since: cp.restored.CalendarList.Since, Version: cp.restored.CalendarList.Version}
}

func (cp *watchCheckpoint) threadActivity() (activeAt, floors map[int64]time.Time) {
	activeAt, floors = map[int64]time.Time{}, map[int64]time.Time{}
	if cp == nil || cp.restored == nil {
		return activeAt, floors
	}
	for id, at := range cp.restored.Threads {
		threadID, _ := strconv.ParseInt(id, 10, 64)
		parsed, _ := time.Parse(time.RFC3339, at)
		activeAt[threadID] = parsed
	}
	for id, at := range cp.restored.Floors {
		boxID, _ := strconv.ParseInt(id, 10, 64)
		parsed, _ := time.Parse(time.RFC3339, at)
		floors[boxID] = parsed
	}
	return activeAt, floors
}

func (cp *watchCheckpoint) seenEvents() []string {
	if cp == nil || cp.restored == nil {
		return nil
	}
	return cp.restored.SeenEvents
}

// save snapshots the watch's position and replaces the checkpoint atomically. A failed
// write is warned about once, not per batch: the watch keeps running on its in-memory
// cursors either way, and a disk that stays full must not fill stderr.
func (cp *watchCheckpoint) save(w *postingsWatch, readyAt string) {
	if cp == nil || !cp.persist {
		return
	}
	if readyAt != "" {
		cp.readyAt = readyAt
	}

	file := watchCheckpointFile{
		Version:   watchCheckpointVersion,
		Scope:     watchCheckpointScope{Origin: cp.scope.origin, Account: cp.scope.account},
		UpdatedAt: watchTime(time.Now()),
		ReadyAt:   cp.readyAt,
		Boxes:     map[string]watchSavedCursor{},
		Calendars: map[string]watchSavedCursor{},
		Threads:   newestThreadActivity(w.newMail, watchCheckpointThreadLimit),
		Floors:    map[string]string{},
	}
	for id, box := range w.boxes {
		if box.cursor.Since == "" {
			continue
		}
		file.Boxes[strconv.FormatInt(id, 10)] = watchSavedCursor{Since: box.cursor.Since, Version: box.cursor.Version}
	}
	if w.calendar != nil {
		for id, calendar := range w.calendar.calendars {
			if calendar.cursor.Since == "" {
				continue
			}
			file.Calendars[strconv.FormatInt(id, 10)] = watchSavedCursor{Since: calendar.cursor.Since, Version: calendar.cursor.Version}
		}
		if w.calendar.listCursor.Since != "" {
			file.CalendarList = &watchSavedCursor{Since: w.calendar.listCursor.Since, Version: w.calendar.listCursor.Version}
		}
	}
	for id, at := range w.newMail.floors {
		file.Floors[strconv.FormatInt(id, 10)] = watchTime(at)
	}
	if w.memory != nil {
		file.SeenEvents = w.memory.keys()
	}

	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return
	}
	if err := writeWatchCheckpoint(cp.path, data); err != nil && !cp.warned {
		cp.warned = true
		fmt.Fprintf(w.errOut, "warning: could not save the watch's checkpoint: %v\n", err)
	}
}

// newestThreadActivity renders the thread record with the most recently active threads
// kept, so a watch running for months can't grow the file without bound.
func newestThreadActivity(mail *newMail, limit int) map[string]string {
	if len(mail.activeAt) <= limit {
		times := make(map[string]string, len(mail.activeAt))
		for id, at := range mail.activeAt {
			times[strconv.FormatInt(id, 10)] = watchTime(at)
		}
		return times
	}

	type threadActivity struct {
		id int64
		at time.Time
	}
	all := make([]threadActivity, 0, len(mail.activeAt))
	for id, at := range mail.activeAt {
		all = append(all, threadActivity{id, at})
	}
	slices.SortFunc(all, func(a, b threadActivity) int { return b.at.Compare(a.at) })

	times := make(map[string]string, limit)
	for _, activity := range all[:limit] {
		times[strconv.FormatInt(activity.id, 10)] = watchTime(activity.at)
	}
	return times
}

// writeWatchCheckpoint replaces the file from a sibling temp file, so a reader — another
// watch resuming, or this one after a crash — meets the old checkpoint or the new one,
// never a write half done.
func writeWatchCheckpoint(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	temporary, err := os.CreateTemp(dir, ".checkpoint-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

// watchEventMemory remembers the keys of changes already handed on, so an event re-read
// across a reconnect or a restart is printed and run through a script once at most. The
// key is the event kind, the posting or recording it names, and the activity time: the
// same activity twice is one event; new activity on the same posting is another.
type watchEventMemory struct {
	limit int
	seen  map[string]struct{}
	order []string
}

func newWatchEventMemory(known []string) *watchEventMemory {
	memory := &watchEventMemory{limit: watchSeenEventLimit, seen: map[string]struct{}{}}
	for _, key := range known {
		memory.remember(key)
	}
	return memory
}

// remember records the key and says whether it was new: false for an event the watch has
// already handed on.
func (m *watchEventMemory) remember(key string) bool {
	if _, ok := m.seen[key]; ok {
		return false
	}
	m.seen[key] = struct{}{}
	m.order = append(m.order, key)
	if len(m.order) > m.limit {
		delete(m.seen, m.order[0])
		m.order = m.order[1:]
	}
	return true
}

func (m *watchEventMemory) keys() []string {
	return slices.Clone(m.order)
}

// watchEventKey is a change's identity for dedup. A posting change is its kind, the
// posting, and its active_at — the event time for a deletion, which has none; a
// recording change is the same shape; a calendar-level change names its calendar; a
// box-level one (a resync) its box.
func watchEventKey(event watchEvent) (string, bool) {
	switch {
	case event.PostingID != 0:
		at := event.At
		if event.Posting != nil && !event.Posting.ActiveAt.IsZero() {
			at = watchTime(event.Posting.ActiveAt)
		}
		return fmt.Sprintf("posting:%s:%d:%s", event.Change, event.PostingID, at), true
	case event.RecordingID != 0:
		return fmt.Sprintf("recording:%s:%d:%s", event.Change, event.RecordingID, event.At), true
	case event.Calendar != nil:
		return fmt.Sprintf("calendar:%s:%d:%s", event.Change, event.Calendar.ID, event.At), true
	case event.Box != nil:
		return fmt.Sprintf("box:%s:%d:%s", event.Change, event.Box.ID, event.At), true
	default:
		return "", false
	}
}
