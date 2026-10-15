package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basecamp/hey-sdk/go/pkg/generated"

	"github.com/basecamp/hey-cli/internal/auth"
	"github.com/basecamp/hey-cli/internal/config"
)

// feedBody is one staged answer of a box's changes feed: the JSON body and the
// since cursor the Link header moves the reader to.
type feedBody struct {
	body string
	next string
}

// stagedFeed answers the changes feed per since cursor, handing out staged bodies in
// order and repeating the last one — the shape a catch-up, a reconnect and a restart
// read the same box in.
type stagedFeed struct {
	mu        sync.Mutex
	stages    map[string][]feedBody
	reads     map[string]int
	requested []string
}

func newStagedFeed() *stagedFeed {
	return &stagedFeed{stages: map[string][]feedBody{}, reads: map[string]int{}}
}

func (f *stagedFeed) stage(since string, bodies ...feedBody) {
	f.stages[since] = bodies
}

func (f *stagedFeed) answer(w http.ResponseWriter, r *http.Request) {
	since := r.URL.Query().Get("since")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requested = append(f.requested, since)
	bodies, staged := f.stages[since]
	if !staged {
		http.Error(w, "no staged answer for since "+since, http.StatusInternalServerError)
		return
	}
	body := bodies[min(f.reads[since], len(bodies)-1)]
	f.reads[since]++

	w.Header().Set("Content-Type", "application/json")
	if body.next != "" {
		w.Header().Set("Link", `<`+r.URL.Path+`?since=`+strings.ReplaceAll(body.next, ":", "%3A")+`&v=2>; rel="next"`)
	}
	_, _ = w.Write([]byte(body.body))
}

// checkpointBoxesJSON is a box list whose Imbox cursor is the watch start, so a cold
// watch reads the feed at 09:00 and a resumed one answers against its own stages.
const checkpointBoxesJSON = `[{"id":24088,"kind":"imbox","name":"Imbox","posting_changes_url":"/boxes/24088/postings/changes.json?since=2026-08-21T09%3A00%3A00.000Z&v=2"}]`

func checkpointBoxesServer(t *testing.T, feed *stagedFeed) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/boxes.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(checkpointBoxesJSON))
		default:
			feed.answer(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// checkpointStateHomes gives a test's first and second watch one state directory: a
// second setupCheckpointWatch is the restart the test is about, and finds the first
// watch's checkpoint.
var checkpointStateHomes sync.Map

func setupCheckpointWatch(t *testing.T, server *httptest.Server, command *watchCommand, events ...string) (*postingsWatch, *bytes.Buffer, *watchCheckpoint) {
	t.Helper()
	t.Setenv("HEY_TOKEN", "test-token")
	stateHome, _ := checkpointStateHomes.LoadOrStore(t.Name(), t.TempDir())
	t.Setenv("XDG_STATE_HOME", stateHome.(string))
	initSDK(auth.NewManager(server.URL, server.Client(), t.TempDir()), server.URL)

	previous := cfg
	cfg = &config.Config{BaseURL: server.URL, AccountID: config.AllAccounts}
	t.Cleanup(func() { cfg = previous })

	if command == nil {
		command = newWatchCommand()
	}
	if len(events) > 0 {
		command.events = events
	}
	changes, err := command.watchedChanges()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cp, err := openWatchCheckpoint(cfg.BaseURL, cfg.AccountID, command.since, command.noCheckpoint)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	started := watchStarted
	newMail := trackNewMail(started)
	activeAt, floors := cp.threadActivity()
	newMail.restore(activeAt, floors)

	boxes, err := command.watchedBoxes(context.Background(), started, cp.boxCursors())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := &bytes.Buffer{}
	watch := &postingsWatch{
		boxes:       boxes,
		changes:     changes,
		asyncScript: command.asyncScript,
		syncScript:  command.syncScript,
		exitOnFirst: command.exitOnFirst,
		newMail:     newMail,
		checkpoint:  cp,
		memory:      newWatchEventMemory(cp.seenEvents()),
		out:         out,
		errOut:      &bytes.Buffer{},
		connection:  make(chan struct{}, 1),
		unread:      map[int64]bool{},
		running:     make(chan struct{}, asyncScriptLimit),
	}
	return watch, out, cp
}

func readCheckpointFile(t *testing.T, cp *watchCheckpoint) watchCheckpointFile {
	t.Helper()
	data, err := os.ReadFile(cp.path)
	if err != nil {
		t.Fatalf("no checkpoint at %s: %v", cp.path, err)
	}
	var file watchCheckpointFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("the checkpoint is not JSON: %v\n%s", err, data)
	}
	return file
}

// The cold start: no file, the first catch-up checkpoints its cursor, the thread's
// activity and the event it handed on, with the ready moment.
func TestWatchCheckpointColdStartSavesPositionAndReady(t *testing.T) {
	feed := newStagedFeed()
	feed.stage("2026-08-21T09:00:00.000Z", feedBody{
		body: `{"added":[{"id":9002,"kind":"topic","box_id":24088,"app_url":"https://app.hey.com/topics/5512","active_at":"2026-08-21T09:14:22Z","creator":{"name":"Northwind Invoicing"}}]}`,
		next: "2026-08-21T09:14:22.000Z",
	})
	feed.stage("2026-08-21T09:14:22.000Z", feedBody{body: `{}`, next: "2026-08-21T09:14:22.000Z"})
	server := checkpointBoxesServer(t, feed)

	watch, out, cp := setupCheckpointWatch(t, server, nil, "added")
	if err := watch.catchUp(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	lines := watchLines(t, out)
	if len(lines) != 2 || lines[0]["posting_id"] != float64(9002) || lines[0]["new"] != true || lines[1]["change"] != "ready" {
		t.Fatalf("wrote %v, want the arrival, new, then ready", lines)
	}

	file := readCheckpointFile(t, cp)
	if file.Version != watchCheckpointVersion {
		t.Errorf("version = %d, want %d", file.Version, watchCheckpointVersion)
	}
	if file.Scope.Account != config.AllAccounts || file.Scope.Origin != server.URL {
		t.Errorf("scope = %+v, want this server and the all-accounts scope", file.Scope)
	}
	if got := file.Boxes["24088"].Since; got != "2026-08-21T09:14:22.000Z" {
		t.Errorf("box cursor = %q, want where the feed left off", got)
	}
	if got := file.Threads["9002"]; got != "2026-08-21T09:14:22.000Z" {
		t.Errorf("thread activity = %q, want the arrival remembered", got)
	}
	if file.ReadyAt == "" || file.UpdatedAt == "" {
		t.Errorf("ready_at = %q, updated_at = %q; want both written at ready", file.ReadyAt, file.UpdatedAt)
	}
	wantKey := "posting:added:9002:2026-08-21T09:14:22.000Z"
	if !slices.Contains(file.SeenEvents, wantKey) {
		t.Errorf("seen_events = %v, want the handed-on event keyed %q", file.SeenEvents, wantKey)
	}
}

// The restart: the second watch resumes at the checkpoint cursor, and the same event
// the feed carries again is handed on once. The thread record comes back too — the same
// activity is not new, a later reply on the thread is.
func TestWatchCheckpointRestartsWhereItStoppedAndDedupes(t *testing.T) {
	feed := newStagedFeed()
	feed.stage("2026-08-21T09:00:00.000Z", feedBody{
		body: `{"added":[{"id":9002,"kind":"topic","box_id":24088,"app_url":"https://app.hey.com/topics/5512","active_at":"2026-08-21T09:14:22Z","creator":{"name":"Northwind Invoicing"}}]}`,
		next: "2026-08-21T09:14:22.000Z",
	})
	feed.stage("2026-08-21T09:14:22.000Z", feedBody{body: `{}`, next: "2026-08-21T09:20:00.000Z"})
	feed.stage("2026-08-21T09:20:00.000Z", feedBody{ // the overlap a restart re-reads
		body: `{"added":[{"id":9002,"kind":"topic","box_id":24088,"app_url":"https://app.hey.com/topics/5512","active_at":"2026-08-21T09:14:22Z","creator":{"name":"Northwind Invoicing"}}]}`,
		next: "2026-08-21T09:25:00.000Z",
	})
	feed.stage("2026-08-21T09:25:00.000Z", feedBody{ // the same thread, the same activity — not new
		body: `{"updated":[{"id":9002,"kind":"topic","box_id":24088,"app_url":"https://app.hey.com/topics/5512","active_at":"2026-08-21T09:14:22Z","creator":{"name":"Northwind Invoicing"}}]}`,
		next: "2026-08-21T09:26:00.000Z",
	})
	feed.stage("2026-08-21T09:26:00.000Z", feedBody{ // a reply: new activity on a thread the record knows
		body: `{"updated":[{"id":9002,"kind":"topic","box_id":24088,"app_url":"https://app.hey.com/topics/5512","active_at":"2026-08-21T09:30:00Z","creator":{"name":"Northwind Invoicing"}}]}`,
		next: "2026-08-21T09:30:00.000Z",
	})
	server := checkpointBoxesServer(t, feed)

	first, _, _ := setupCheckpointWatch(t, server, nil, "added", "updated")
	if err := first.catchUp(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The restart: same state directory, a fresh watch built the way run() builds one.
	second, out, cp := setupCheckpointWatch(t, server, nil, "added", "updated")
	if got := second.boxes[24088].cursor.Since; got != "2026-08-21T09:14:22.000Z" {
		t.Fatalf("resumed cursor = %q, want the checkpoint's", got)
	}
	if err := second.catchUp(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The catch-up re-met the arrival across the cursor overlap; it must not be replayed.
	if lines := watchLines(t, out); len(lines) != 1 || lines[0]["change"] != "ready" {
		t.Fatalf("wrote %v, want only ready, the repeated event suppressed", lines)
	}
	out.Reset()

	// Another read carries the same arrival again: still the same event, suppressed.
	if err := second.readBox(context.Background(), second.boxes[24088]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("wrote %q, want the repeated arrival suppressed", out.String())
	}

	// Then the same thread with the same activity — an update, but not new.
	if err := second.readBox(context.Background(), second.boxes[24088]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := watchLines(t, out)
	if len(lines) != 1 || lines[0]["change"] != "updated" || lines[0]["new"] != false {
		t.Errorf("wrote %v, want one update of the known activity, not new", lines)
	}

	out.Reset()
	if err := second.readBox(context.Background(), second.boxes[24088]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines = watchLines(t, out)
	if len(lines) != 1 || lines[0]["new"] != true {
		t.Errorf("wrote %v, want the later reply new against the restored record", lines)
	}
	if got := readCheckpointFile(t, cp).Boxes["24088"].Since; got != "2026-08-21T09:30:00.000Z" {
		t.Errorf("cursor = %q, want it advanced through the resumed reads", got)
	}
	if got := readCheckpointFile(t, cp).ReadyAt; got == "" {
		t.Error("ready_at = empty, want the ready moment carried across the later batch saves")
	}
}

// A resync skips ahead once; the skip's cursor and new-mail floor are checkpointed, so a
// restart reads forward from after the gap and never sees the resync again.
func TestWatchCheckpointResumesAfterAResyncWithoutRepeatingIt(t *testing.T) {
	feed := newStagedFeed()
	feed.stage("2026-08-21T09:00:00.000Z", feedBody{}) // answered 409 below
	feed.stage("2026-08-21T12:00:00.000Z", feedBody{
		body: `{"added":[{"id":9010,"kind":"topic","box_id":24088,"app_url":"https://app.hey.com/topics/5520","active_at":"2026-08-21T12:03:00Z","creator":{"name":"Sam Whitfield"}}]}`,
		next: "2026-08-21T12:05:00.000Z",
	})
	feed.stage("2026-08-21T12:05:00.000Z", feedBody{body: `{}`, next: "2026-08-21T12:05:00.000Z"})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/boxes.json":
			// The fresh cursor the skip-ahead finds in the box list.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":24088,"kind":"imbox","name":"Imbox","posting_changes_url":"/boxes/24088/postings/changes.json?since=2026-08-21T12%3A00%3A00.000Z&v=2"}]`))
		default:
			if r.URL.Query().Get("since") == "2026-08-21T09:00:00.000Z" {
				w.WriteHeader(http.StatusConflict) // too far behind: full sync
				return
			}
			feed.answer(w, r)
		}
	}))
	t.Cleanup(server.Close)

	first, out, cp := setupCheckpointWatch(t, server, nil, "added", "resync")
	if err := first.readBox(context.Background(), first.boxes[24088]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := watchLines(t, out)
	if len(lines) != 1 || lines[0]["change"] != "resync" {
		t.Fatalf("wrote %v, want one resync", lines)
	}
	file := readCheckpointFile(t, cp)
	if got := file.Boxes["24088"].Since; got != "2026-08-21T12:00:00.000Z" {
		t.Fatalf("cursor = %q, want the skip-ahead cursor checkpointed", got)
	}
	if got := file.Floors["24088"]; got != "2026-08-21T12:00:00.000Z" {
		t.Errorf("floor = %q, want the skip's floor checkpointed", got)
	}

	// The restart resumes at the fresh cursor: no 409, no second resync, and the mail
	// after the gap is new against the restored floor.
	second, secondOut, _ := setupCheckpointWatch(t, server, nil, "added", "resync")
	if got := second.boxes[24088].cursor.Since; got != "2026-08-21T12:00:00.000Z" {
		t.Fatalf("resumed cursor = %q, want the skip-ahead one", got)
	}
	if err := second.catchUp(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines = watchLines(t, secondOut)
	if len(lines) != 2 || lines[0]["change"] != "added" || lines[0]["new"] != true || lines[1]["change"] != "ready" {
		t.Errorf("wrote %v, want the post-gap mail, new, and ready — no repeated resync", lines)
	}
}

// A reconnect catches up from the in-memory cursors and checkpoints that catch-up, so a
// restart after it resumes past both batches.
func TestWatchCheckpointReconnectCatchUpCheckpointsAndRestartDedupes(t *testing.T) {
	feed := newStagedFeed()
	feed.stage("2026-08-21T09:00:00.000Z", feedBody{
		body: `{"added":[{"id":9001,"kind":"topic","box_id":24088,"app_url":"https://app.hey.com/topics/5511","active_at":"2026-08-21T09:14:22Z","creator":{"name":"Maria Delgado"}}]}`,
		next: "2026-08-21T09:14:22.000Z",
	})
	feed.stage("2026-08-21T09:14:22.000Z", feedBody{
		body: `{"added":[{"id":9002,"kind":"topic","box_id":24088,"app_url":"https://app.hey.com/topics/5512","active_at":"2026-08-21T09:20:00Z","creator":{"name":"Northwind Invoicing"}}]}`,
		next: "2026-08-21T09:20:00.000Z",
	})
	feed.stage("2026-08-21T09:20:00.000Z", feedBody{body: `{}`, next: "2026-08-21T09:21:00.000Z"})
	feed.stage("2026-08-21T09:21:00.000Z", feedBody{ // the batch before the reconnect, met again across the restart
		body: `{"added":[{"id":9001,"kind":"topic","box_id":24088,"app_url":"https://app.hey.com/topics/5511","active_at":"2026-08-21T09:14:22Z","creator":{"name":"Maria Delgado"}}]}`,
		next: "2026-08-21T09:25:00.000Z",
	})
	feed.stage("2026-08-21T09:25:00.000Z", feedBody{ // new activity on the same posting
		body: `{"updated":[{"id":9001,"kind":"topic","box_id":24088,"app_url":"https://app.hey.com/topics/5511","active_at":"2026-08-21T09:30:00Z","creator":{"name":"Maria Delgado"}}]}`,
		next: "2026-08-21T09:30:00.000Z",
	})
	server := checkpointBoxesServer(t, feed)

	first, out, cp := setupCheckpointWatch(t, server, nil, "added", "updated")
	if err := first.catchUp(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	first.noteConnection(false)
	first.noteConnection(true)
	if err := first.followConnection(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := watchLines(t, out)
	if len(lines) != 5 ||
		lines[0]["posting_id"] != float64(9001) || lines[1]["change"] != "ready" ||
		lines[2]["change"] != "disconnected" ||
		lines[3]["posting_id"] != float64(9002) || lines[4]["change"] != "ready" {
		t.Fatalf("wrote %v, want the first catch-up and ready, then drop, the reconnect's catch-up, ready", lines)
	}
	if got := readCheckpointFile(t, cp).Boxes["24088"].Since; got != "2026-08-21T09:20:00.000Z" {
		t.Fatalf("cursor = %q, want the reconnect's catch-up checkpointed", got)
	}

	second, secondOut, _ := setupCheckpointWatch(t, server, nil, "added", "updated")
	if err := second.catchUp(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lines := watchLines(t, secondOut); len(lines) != 1 || lines[0]["change"] != "ready" {
		t.Fatalf("wrote %v, want only ready on the resumed catch-up", lines)
	}
	secondOut.Reset()
	// The overlap replays the first posting's old activity: suppressed.
	if err := second.readBox(context.Background(), second.boxes[24088]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if secondOut.Len() != 0 {
		t.Errorf("wrote %q, want the reconnected batch's old event suppressed", secondOut.String())
	}
	// New activity on the posting is another event.
	if err := second.readBox(context.Background(), second.boxes[24088]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines = watchLines(t, secondOut)
	if len(lines) != 1 || lines[0]["posting_id"] != float64(9001) || lines[0]["change"] != "updated" {
		t.Errorf("wrote %v, want only the later reply", lines)
	}
}

// A script already run for an event does not run again when the event is re-read, and it
// runs for genuinely new activity.
func TestWatchCheckpointDoesNotRerunAConfirmedScript(t *testing.T) {
	feed := newStagedFeed()
	feed.stage("2026-08-21T09:00:00.000Z", feedBody{
		body: `{"added":[{"id":9001,"kind":"topic","box_id":24088,"app_url":"https://app.hey.com/topics/5511","active_at":"2026-08-21T09:14:22Z","creator":{"name":"Maria Delgado"}}]}`,
		next: "2026-08-21T09:14:22.000Z",
	})
	feed.stage("2026-08-21T09:14:22.000Z", feedBody{ // the catch-up's overlap: the event the first watch already ran
		body: `{"added":[{"id":9001,"kind":"topic","box_id":24088,"app_url":"https://app.hey.com/topics/5511","active_at":"2026-08-21T09:14:22Z","creator":{"name":"Maria Delgado"}}]}`,
		next: "2026-08-21T09:20:00.000Z",
	})
	feed.stage("2026-08-21T09:20:00.000Z", feedBody{ // the same event, re-read again
		body: `{"added":[{"id":9001,"kind":"topic","box_id":24088,"app_url":"https://app.hey.com/topics/5511","active_at":"2026-08-21T09:14:22Z","creator":{"name":"Maria Delgado"}}]}`,
		next: "2026-08-21T09:25:00.000Z",
	})
	feed.stage("2026-08-21T09:25:00.000Z", feedBody{ // genuinely new activity on the posting
		body: `{"updated":[{"id":9001,"kind":"topic","box_id":24088,"app_url":"https://app.hey.com/topics/5511","active_at":"2026-08-21T09:30:00Z","creator":{"name":"Maria Delgado"}}]}`,
		next: "2026-08-21T09:30:00.000Z",
	})
	server := checkpointBoxesServer(t, feed)
	marker := filepath.Join(t.TempDir(), "ran")
	script := `printf '%s\n' "$HEY_POSTING_ID" >> ` + marker

	first := newWatchCommand()
	first.syncScript = script
	watch, _, _ := setupCheckpointWatch(t, server, first, "added", "updated")
	if err := watch.catchUp(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runs := strings.TrimSpace(string(mustRead(t, marker))); runs != "9001" {
		t.Fatalf("script ran for %q, want once for 9001", runs)
	}

	second := newWatchCommand()
	second.syncScript = script
	resumed, _, _ := setupCheckpointWatch(t, server, second, "added", "updated")
	// The resumed catch-up meets the old event across the overlap: it must not run.
	if err := resumed.catchUp(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := resumed.readBox(context.Background(), resumed.boxes[24088]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runs := strings.TrimSpace(string(mustRead(t, marker))); runs != "9001" {
		t.Errorf("script ran again for the same event: %q", runs)
	}
	if err := resumed.readBox(context.Background(), resumed.boxes[24088]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runs := strings.TrimSpace(string(mustRead(t, marker))); runs != "9001\n9001" {
		t.Errorf("script ran for %q, want the new activity to run it once more", runs)
	}
}

// --since wins over the checkpoint outright: nothing is restored, the feed starts at the
// given time, and an event the checkpoint had already handed on is reported again.
func TestWatchSinceTakesPrecedenceOverTheCheckpoint(t *testing.T) {
	feed := newStagedFeed()
	feed.stage("2026-08-21T09:00:00.000Z", feedBody{
		body: `{"added":[{"id":9001,"kind":"topic","box_id":24088,"app_url":"https://app.hey.com/topics/5511","active_at":"2026-08-21T09:14:22Z","creator":{"name":"Maria Delgado"}}]}`,
		next: "2026-08-21T09:14:22.000Z",
	})
	feed.stage("2026-08-21T08:00:00.000Z", feedBody{
		body: `{"added":[{"id":9001,"kind":"topic","box_id":24088,"app_url":"https://app.hey.com/topics/5511","active_at":"2026-08-21T09:14:22Z","creator":{"name":"Maria Delgado"}}]}`,
		next: "2026-08-21T09:40:00.000Z",
	})
	server := checkpointBoxesServer(t, feed)

	first, _, _ := setupCheckpointWatch(t, server, nil, "added")
	if err := first.catchUp(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	withSince := newWatchCommand()
	withSince.since = "2026-08-21T08:00:00Z"
	second, out, cp := setupCheckpointWatch(t, server, withSince, "added")
	if restored := cp.boxCursors(); len(restored) != 0 {
		t.Errorf("restored %v, want --since to restore nothing", restored)
	}
	if len(cp.seenEvents()) != 0 {
		t.Error("seen events = restored, want --since to start with a clean memory")
	}
	if got := second.boxes[24088].cursor.Since; got != "2026-08-21T08:00:00.000Z" {
		t.Fatalf("cursor = %q, want --since", got)
	}
	if err := second.catchUp(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := watchLines(t, out)
	if len(lines) != 2 || lines[0]["posting_id"] != float64(9001) || lines[0]["new"] != true {
		t.Errorf("wrote %v, want the explicit catch-up to replay the event", lines)
	}
	// --since still writes: the next plain watch resumes where this catch-up ended.
	if got := readCheckpointFile(t, cp).Boxes["24088"].Since; got != "2026-08-21T09:40:00.000Z" {
		t.Errorf("cursor = %q, want the --since run's new position saved", got)
	}
}

// --no-checkpoint neither restores nor writes, however much the watch reads.
func TestWatchNoCheckpointLeavesNoState(t *testing.T) {
	feed := newStagedFeed()
	feed.stage("2026-08-21T09:00:00.000Z", feedBody{
		body: `{"added":[{"id":9001,"kind":"topic","box_id":24088,"app_url":"https://app.hey.com/topics/5511","active_at":"2026-08-21T09:14:22Z","creator":{"name":"Maria Delgado"}}]}`,
		next: "2026-08-21T09:14:22.000Z",
	})
	server := checkpointBoxesServer(t, feed)

	command := newWatchCommand()
	command.noCheckpoint = true
	watch, out, cp := setupCheckpointWatch(t, server, command, "added")
	if cp.persist {
		t.Error("--no-checkpoint must disable writes")
	}
	if err := watch.catchUp(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), `"posting_id":9001`) {
		t.Errorf("wrote %q, want the change reported as usual", out.String())
	}
	stateRoot := filepath.Dir(filepath.Dir(cp.path)) // .../hey-cli
	if left := filesUnder(t, stateRoot); len(left) != 0 {
		t.Errorf("state files = %v, want none with --no-checkpoint", left)
	}
}

// The calendar cursors come back just like the boxes', and a replayed recording is
// handed on once.
func TestWatchCheckpointRestoresCalendarCursorsAndDedupesRecordings(t *testing.T) {
	t.Setenv("HEY_TOKEN", "test-token")
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	requested := map[string][]string{}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/calendars.json":
			_, _ = w.Write([]byte(`{
				"calendars": [{"calendar": {"id": 512, "name": "Household"},
				               "recording_changes_url": "/calendars/512/recording/changes.json?since=2026-08-21T09%3A00%3A00.000Z&v=1",
				               "signed_stream_name": "signed-household"}],
				"calendar_changes_url": "/calendar/changes.json?since=2026-08-21T09%3A00%3A00.000Z"
			}`))
		case strings.HasPrefix(r.URL.Path, "/calendars/512/recording/changes"):
			since := r.URL.Query().Get("since")
			mu.Lock()
			requested[since] = append(requested[since], since)
			count := len(requested[since])
			mu.Unlock()
			if count == 1 {
				w.Header().Set("Link", `<`+r.URL.Path+`?since=2026-08-21T11%3A05%3A00.000Z&v=1>; rel="next"`)
				_, _ = w.Write([]byte(`{"added": {"Calendar::Event": [{"id": 88001, "title": "Dentist appointment", "created_at": "2026-08-21T11:02:00.000Z"}]}}`))
				return
			}
			w.Header().Set("Link", `<`+r.URL.Path+`?since=2026-08-21T11%3A10%3A00.000Z&v=1>; rel="next"`)
			// The same recording, re-read across the restart overlap.
			_, _ = w.Write([]byte(`{"added": {"Calendar::Event": [{"id": 88001, "title": "Dentist appointment", "created_at": "2026-08-21T11:02:00.000Z"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	initSDK(auth.NewManager(server.URL, server.Client(), t.TempDir()), server.URL)
	previous := cfg
	cfg = &config.Config{BaseURL: server.URL, AccountID: config.AllAccounts}
	t.Cleanup(func() { cfg = previous })

	// Seed a checkpoint as a previous watch that already read to 11:00 / 10:00.
	cp, err := openWatchCheckpoint(cfg.BaseURL, cfg.AccountID, "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	seed := watchCheckpointFile{
		Version:      watchCheckpointVersion,
		Scope:        watchCheckpointScope{Origin: cp.scope.origin, Account: cp.scope.account},
		UpdatedAt:    "2026-08-21T11:00:00.000Z",
		Calendars:    map[string]watchSavedCursor{"512": {Since: "2026-08-21T11:00:00.000Z", Version: "1"}},
		CalendarList: &watchSavedCursor{Since: "2026-08-21T10:00:00.000Z"},
	}
	writeSeedCheckpoint(t, cp.path, seed)
	cp.restored = cp.read() // the seed landed after the initial open

	command := newWatchCommand()
	calendars, err := command.watchedCalendars(context.Background(), watchStarted, cp.calendarCursors(), cp.calendarListCursor())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := calendars.calendars[512].cursor; got.Since != "2026-08-21T11:00:00.000Z" || got.Version != "1" {
		t.Errorf("recording cursor = %+v, want the checkpoint's", got)
	}
	if got := calendars.listCursor.Since; got != "2026-08-21T10:00:00.000Z" {
		t.Errorf("calendar list cursor = %q, want the checkpoint's", got)
	}

	watch, out := newTestWatch("recording_added")
	watch.checkpoint = cp
	watch.memory = newWatchEventMemory(cp.seenEvents())
	watch.calendar = calendars
	if err := watch.readCalendar(context.Background(), calendars.calendars[512]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lines := watchLines(t, out); len(lines) != 1 || lines[0]["recording_id"] != float64(88001) {
		t.Fatalf("wrote %v, want the recording once", lines)
	}
	file := readCheckpointFile(t, cp)
	if got := file.Calendars["512"].Since; got != "2026-08-21T11:05:00.000Z" {
		t.Errorf("recording cursor = %q, want it checkpointed", got)
	}
	wantKey := "recording:recording_added:88001:2026-08-21T11:02:00.000Z"
	if !slices.Contains(file.SeenEvents, wantKey) {
		t.Errorf("seen_events = %v, want %q", file.SeenEvents, wantKey)
	}

	// Restart: the feed carries the same recording again and it is suppressed.
	resumed := &watchCheckpoint{path: cp.path, scope: cp.scope, persist: true, restored: cp.read()}
	watchTwo, outTwo := newTestWatch("recording_added")
	watchTwo.checkpoint = resumed
	watchTwo.memory = newWatchEventMemory(resumed.seenEvents())
	watchTwo.calendar = newTestCalendarsWatch(t, &watchedCalendar{
		id: 512, name: "Household", cursor: resumed.calendarCursors()[512],
	})
	if err := watchTwo.readCalendar(context.Background(), watchTwo.calendar.calendars[512]); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outTwo.Len() != 0 {
		t.Errorf("wrote %q, want the replayed recording suppressed", outTwo.String())
	}
}

// --- unit tests for the store, the scope, the memory and the keys ---

func TestWatchCheckpointScope(t *testing.T) {
	first, err := watchScopeFor("http://127.0.0.1:3939", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first.account != config.AllAccounts {
		t.Errorf("account = %q, want the all-accounts default", first.account)
	}
	named, err := watchScopeFor("http://127.0.0.1:3939", "42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	otherAccount, err := watchScopeFor("http://127.0.0.1:3939", "43")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	otherServer, err := watchScopeFor("http://127.0.0.1:4040", "42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first.token() == named.token() || named.token() == otherAccount.token() || named.token() == otherServer.token() {
		t.Error("different servers or accounts must have different checkpoint files")
	}
	if again, _ := watchScopeFor("HTTP://127.0.0.1:3939/", "42"); again.token() != named.token() {
		t.Error("the same server, only cased or trailed differently, must share a file")
	}
	if _, err := watchScopeFor("not a url", "42"); err == nil {
		t.Error("expected an unusable base URL to fail")
	}
}

func TestWatchEventKey(t *testing.T) {
	isNew := true
	added := watchEvent{
		Change: "added", At: "2026-08-21T09:14:00.000Z", Box: &watchEventBox{ID: 24088},
		PostingID: 9001, New: &isNew,
		Posting: &generated.Posting{ActiveAt: time.Date(2026, 8, 21, 9, 14, 22, 0, time.UTC)},
	}
	key, ok := watchEventKey(added)
	if !ok || key != "posting:added:9001:2026-08-21T09:14:22.000Z" {
		t.Errorf("key = %q, want kind, posting and active_at", key)
	}

	deleted := watchEvent{Change: "deleted", At: "2026-08-21T09:15:00.000Z", PostingID: 9003}
	if key, _ = watchEventKey(deleted); key != "posting:deleted:9003:2026-08-21T09:15:00.000Z" {
		t.Errorf("key = %q, want a deletion keyed on its event time", key)
	}

	recording := watchEvent{
		Change: "recording_added", At: "2026-08-21T09:14:22.031Z",
		Calendar: &watchEventCalendar{ID: 512, Name: "Household"}, RecordingID: 88001, RecordingType: "Calendar::Event",
	}
	if key, _ = watchEventKey(recording); key != "recording:recording_added:88001:2026-08-21T09:14:22.031Z" {
		t.Errorf("key = %q, want kind, recording and event time", key)
	}

	calendarAdded := watchEvent{Change: "calendar_added", At: "2026-08-21T09:14:22.031Z", Calendar: &watchEventCalendar{ID: 514}}
	if key, _ = watchEventKey(calendarAdded); key != "calendar:calendar_added:514:2026-08-21T09:14:22.031Z" {
		t.Errorf("key = %q, want the calendar-level change keyed on its calendar", key)
	}

	resync := watchEvent{Change: "resync", At: "2026-08-21T09:14:22.031Z", Box: &watchEventBox{ID: 24088}}
	if key, _ = watchEventKey(resync); key != "box:resync:24088:2026-08-21T09:14:22.031Z" {
		t.Errorf("key = %q, want the resync keyed on its box and moment", key)
	}

	if _, ok := watchEventKey(watchEvent{Change: "ready"}); ok {
		t.Error("the watch's own news has no dedup key")
	}
}

func TestWatchEventMemoryRemembersOnceAndBoundsItself(t *testing.T) {
	memory := newWatchEventMemory([]string{"posting:added:1:x"})
	memory.limit = 3
	if memory.remember("posting:added:1:x") {
		t.Error("a restored key is already remembered")
	}
	if !memory.remember("posting:added:2:x") || !memory.remember("posting:added:3:x") {
		t.Error("new keys should be remembered")
	}
	// A fourth evicts the oldest — the restored seed — so the rest stay and the
	// forgotten oldest can be remembered again as new.
	memory.remember("posting:added:4:x")
	if !memory.remember("posting:added:1:x") {
		t.Error("the evicted oldest key should be new again")
	}
	keys := memory.keys()
	if len(keys) != 3 {
		t.Errorf("keys = %v, want the memory capped at 3", keys)
	}
}

func TestNewestThreadActivityKeepsTheMostRecentlyActive(t *testing.T) {
	mail := trackNewMail(watchStarted)
	mail.restore(map[int64]time.Time{
		1: watchStarted.Add(time.Minute),
		2: watchStarted.Add(3 * time.Minute),
		3: watchStarted.Add(2 * time.Minute),
	}, nil)

	got := newestThreadActivity(mail, 2)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want the limit", len(got))
	}
	if _, has := got["2"]; !has {
		t.Error("the newest activity should be kept")
	}
	if _, has := got["3"]; !has {
		t.Error("the second-newest should be kept")
	}
	if _, has := got["1"]; has {
		t.Error("the oldest should be trimmed")
	}
}

func TestWriteWatchCheckpointIsAtomicAndPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "watch")
	path := filepath.Join(dir, "checkpoint-test.json")

	if err := writeWatchCheckpoint(path, []byte(`{"version":1}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", info.Mode().Perm())
	}
	if dirInfo, err := os.Stat(dir); err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", dirInfo.Mode().Perm())
	}
	if leftovers := filesUnder(t, dir); len(leftovers) != 1 || leftovers[0] != filepath.Base(path) {
		t.Errorf("files = %v, want the checkpoint alone, no temp files", leftovers)
	}

	// A second write replaces the file in place; a reader never meets a partial write.
	if err := writeWatchCheckpoint(path, []byte(`{"version":1,"ready_at":"2026-08-21T09:30:00.000Z"}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if leftovers := filesUnder(t, dir); len(leftovers) != 1 {
		t.Errorf("files = %v, want one file after the replace", leftovers)
	}
	if data := mustRead(t, path); !strings.Contains(string(data), "09:30:00") {
		t.Errorf("content = %q, want the replacement", data)
	}
}

func TestWatchCheckpointIgnoresATornOrForeignFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	scope := watchScope{origin: "http://127.0.0.1:3939", account: config.AllAccounts}
	valid := func() watchCheckpointFile {
		return watchCheckpointFile{
			Version:   watchCheckpointVersion,
			Scope:     watchCheckpointScope{Origin: scope.origin, Account: scope.account},
			UpdatedAt: "2026-08-21T09:30:00.000Z",
			Boxes:     map[string]watchSavedCursor{"24088": {Since: "2026-08-21T09:14:22.000Z", Version: "2"}},
		}
	}

	cases := map[string]any{
		"torn write":    `{"version":1,"scope":{"ori`,
		"not json":      `everything changed, nothing stayed`,
		"empty file":    ``,
		"wrong version": func() watchCheckpointFile { f := valid(); f.Version = 99; return f }(),
		"other account": func() watchCheckpointFile { f := valid(); f.Scope.Account = "42"; return f }(),
		"other server":  func() watchCheckpointFile { f := valid(); f.Scope.Origin = "http://example.test"; return f }(),
		"bad box id": func() watchCheckpointFile {
			f := valid()
			f.Boxes = map[string]watchSavedCursor{"imbox": {Since: "2026-08-21T09:14:22.000Z"}}
			return f
		}(),
		"bad cursor since": func() watchCheckpointFile {
			f := valid()
			f.Boxes = map[string]watchSavedCursor{"24088": {Since: "later"}}
			return f
		}(),
		"bad thread time": func() watchCheckpointFile { f := valid(); f.Threads = map[string]string{"9001": "yesterday"}; return f }(),
		"bad calendar list": func() watchCheckpointFile {
			f := valid()
			f.CalendarList = &watchSavedCursor{Since: "whenever"}
			return f
		}(),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			cp := &watchCheckpoint{path: filepath.Join(t.TempDir(), "checkpoint.json"), scope: scope}
			switch data := content.(type) {
			case string:
				mustWrite(t, cp.path, []byte(data))
			case watchCheckpointFile:
				writeSeedCheckpoint(t, cp.path, data)
			}
			if restored := cp.read(); restored != nil {
				t.Errorf("read() = %+v, want a %s checkpoint ignored", restored, name)
			}
		})
	}

	t.Run("valid file restores", func(t *testing.T) {
		cp := &watchCheckpoint{path: filepath.Join(t.TempDir(), "checkpoint.json"), scope: scope}
		writeSeedCheckpoint(t, cp.path, valid())
		restored := cp.read()
		if restored == nil {
			t.Fatal("a valid checkpoint for this scope should restore")
		}
		if got := restored.Boxes["24088"].Since; got != "2026-08-21T09:14:22.000Z" {
			t.Errorf("cursor = %q, want it restored", got)
		}
	})

	t.Run("missing file is a fresh start", func(t *testing.T) {
		cp := &watchCheckpoint{path: filepath.Join(t.TempDir(), "missing.json"), scope: scope}
		if restored := cp.read(); restored != nil {
			t.Errorf("read() = %+v, want nil with no file", restored)
		}
	})
}

func TestWatchCheckpointFilesAreIsolatedPerAccount(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	personal, err := openWatchCheckpoint("http://127.0.0.1:3939", "42", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	work, err := openWatchCheckpoint("http://127.0.0.1:3939", "43", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if personal.path == work.path {
		t.Fatal("two accounts must not share a checkpoint file")
	}

	first := watchCheckpointFile{
		Version: watchCheckpointVersion,
		Scope:   watchCheckpointScope{Origin: personal.scope.origin, Account: "42"},
		Boxes:   map[string]watchSavedCursor{"1": {Since: "2026-08-21T09:00:00.000Z"}},
	}
	writeSeedCheckpoint(t, personal.path, first)
	second := watchCheckpointFile{
		Version: watchCheckpointVersion,
		Scope:   watchCheckpointScope{Origin: work.scope.origin, Account: "43"},
		Boxes:   map[string]watchSavedCursor{"2": {Since: "2026-08-21T10:00:00.000Z"}},
	}
	writeSeedCheckpoint(t, work.path, second)

	if restored := personal.read(); restored == nil || restored.Boxes["1"] == (watchSavedCursor{}) {
		t.Error("the personal account should read its own checkpoint")
	}
	if restored := work.read(); restored == nil || restored.Boxes["2"] == (watchSavedCursor{}) {
		t.Error("the work account should read its own checkpoint")
	}

	// A file copied in from another account lands under a name that does not match its
	// contents: the scope check ignores it rather than restoring another account's boxes.
	writeSeedCheckpoint(t, personal.path, second)
	if restored := personal.read(); restored != nil {
		t.Errorf("restored %+v, want the foreign-account contents ignored", restored)
	}
}

// --- small file helpers ---

func writeSeedCheckpoint(t *testing.T, path string, file watchCheckpointFile) {
	t.Helper()
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := writeWatchCheckpoint(path, data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		files = append(files, path[len(root)+1:])
		return nil
	})
	return files
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("unexpected error reading %s: %v", path, err)
	}
	return data
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
