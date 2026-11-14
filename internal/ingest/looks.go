package ingest

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bilanc/posthook/internal/gitx"
	"github.com/bilanc/posthook/internal/lineranges"
	"github.com/bilanc/posthook/internal/logx"
	"github.com/bilanc/posthook/internal/notes"
	"github.com/bilanc/posthook/internal/spool"
	"github.com/bilanc/posthook/internal/store"
)

// Looking at the working tree.
//
// An agent that changes files with a script (`python3 - <<EOF`), a formatter
// or `sed -i` sends a hook payload that names no file and carries no text.
// Nothing in it says what changed. So after every shell tool call posthook
// looks at the repository: it asks git which files are modified or new,
// compares each with its authorship map, and credits the lines that are new
// to that shell call. The same look, when a prompt is submitted, credits
// nobody: whatever changed between two turns was changed by a person.
//
// What a look credits, and what it does not:
//
//   - A line is new when it is in neither the file's map nor the base
//     version of the file in git. The base is HEAD. So a file that a git
//     command left equal to HEAD credits nothing (checkout, pull, merge,
//     rebase, reset, restore), and a map left over from another branch
//     cannot turn the branch's differences into generated lines.
//   - When HEAD has moved since the last look, the reflog says how. If it
//     shows only `git commit` entries (an amend among them), and each can be
//     put down to an agent's shell command (byAgentCommand: made while the
//     command ran, with the commit message the command names), the base is
//     where HEAD was at the last look: the lines those commits added were
//     written during the calls (`python gen.py && git commit`), and their
//     files are looked at even though they are clean. Commits somebody else
//     made (the engineer, in another terminal) leave the base at HEAD, so
//     what they committed is not new. When HEAD moved any other way, or the
//     command itself runs a git command that brings existing content in
//     (movesContent), the look records what it finds and credits nothing.
//   - A new file whose content is, byte for byte, a file of the base commit
//     was copied or moved there (`cp`, `mv`): nobody wrote it.
//   - The first look at a repository credits nothing: there is no earlier
//     look to say what was already there. Nor does the first look after one
//     that could not be made (git did not answer), or after a long gap. And
//     after a look that could not read every changed file (more than
//     maxLookFiles), a file with no map is recorded and not credited.
//   - Whenever git cannot say what the base version of a file is, the file
//     is recorded and not credited. A base version too large to keep a map
//     for (store.MaxSnapshotBytes) is not read, and counts as unknown.
//
// A look runs in the background worker, after the fact, like every event.
// It is held back until the worker has caught up with the events that had
// already fired, so an edit made right after a shell command has claimed its
// own lines by the time the look reads the disk. Lines that such an edit
// finds in a file and cannot explain are held for the look
// (store.LineAuthor.Held), which confirms or drops them by the same rules.
//
// git is asked with --no-optional-locks, so a look never takes index.lock
// and cannot make the engineer's or the agent's own git command fail, and
// with a timeout: on a repository where git does not answer in time the
// event credits nothing, and the repository is left alone for a while.
//
// What a look costs: one `git status` when nothing changed, which is most
// looks. When files changed it also reads them, asks git for their base
// versions (blobsAt: one `git cat-file`), lists the base tree when one of
// them is a new file (blobsInTree), and attributes again the recent commits
// that hold lines it has just given an author.

const (
	// lookSettle is how long after its hook fired a look waits before it
	// may run.
	lookSettle = 500 * time.Millisecond
	// slowRepoBackoff is how long a repository is left alone after git did
	// not answer in time.
	slowRepoBackoff = 10 * time.Minute
	// maxLookAge is how long after its hook fired a look may still credit
	// its shell call. A look that runs later than that (the worker was not
	// running) cannot tell what the command wrote from everything that has
	// happened since, and only records.
	maxLookAge = 10 * time.Minute
	// LookBacklog is how long a look may wait for the queue to empty before
	// the worker runs it regardless.
	LookBacklog = 10 * time.Second
	// maxLookGap is how long after the last look at a repository the next
	// one may still credit. The maps of files that have not changed since
	// are dropped after a month untouched; past this gap a changed file
	// with no map is not known to have been unchanged then.
	maxLookGap = 14 * 24 * time.Hour
	// mapRefresh is how often the map of a changed file that is still as
	// the map has it is marked as seen, so it does not expire while the
	// file stays that way.
	mapRefresh = 24 * time.Hour
	// maxLookFiles bounds the changed files one look reads, and
	// maxLookDirs the untracked directories it lists the files of.
	maxLookFiles = 200
	maxLookDirs  = 50
	// reflogDepth bounds how far back a look reads HEAD's reflog.
	reflogDepth = 200
)

// lookTimeout bounds each git call a look makes. A variable so a test can
// make git "not answer in time".
var lookTimeout = 2 * time.Second

// queueLooks queues the looks an event calls for: one per repository after a
// shell tool call, one for the session's repository when a prompt is
// submitted.
func queueLooks(db *store.DB, eventType, agentSlug string, payload map[string]any, author store.LineAuthor, roots []string, firedAt time.Time) {
	// A tool call ran some time between the session's previous event and
	// this one.
	started := previousEventFired(author.SessionID, firedAt)
	kind, command, movesContent, commits := "", "", false, false
	switch {
	case eventType == "PostToolUse" && isShellTool(payload):
		kind = store.LookShell
		command = shellCommand(payload)
		movesContent = commandMovesContent(command)
		commits = commandCommits(command)
	case eventType == "UserPromptSubmit" || eventType == "beforeSubmitPrompt":
		kind = store.LookPrompt
	default:
		return
	}
	seen := map[string]bool{}
	for _, root := range roots {
		if root == "" {
			continue
		}
		// Maps are kept under the canonical path of each file.
		if root = gitx.Canonicalize(root); seen[root] {
			continue
		}
		seen[root] = true
		if err := db.AddPendingLook(store.PendingLook{
			Kind: kind, EventID: author.EventID, SessionID: author.SessionID, Agent: agentSlug, EventTS: author.TS,
			Root: root, FiredAt: firedAt, StartedAt: started, Command: command,
			MovesContent: movesContent, Commits: commits,
		}); err != nil {
			logx.Warnf("queueing a look at %s failed: %v", root, err)
		}
	}
}

// lastFired is when the last event of each session fired, as far as this
// process has seen. The worker is one long-lived process, so in practice
// that is every event since it started.
var (
	lastFiredMu sync.Mutex
	lastFired   = map[string]time.Time{}
)

// previousEventFired returns when the session's previous event fired (zero
// when this process has seen none) and records firedAt as its latest.
func previousEventFired(sessionID string, firedAt time.Time) time.Time {
	if sessionID == "" {
		return time.Time{}
	}
	lastFiredMu.Lock()
	defer lastFiredMu.Unlock()
	previous := lastFired[sessionID]
	if len(lastFired) > 4096 {
		lastFired = map[string]time.Time{}
	}
	if firedAt.After(previous) {
		lastFired[sessionID] = firedAt
	} else {
		lastFired[sessionID] = previous
	}
	return previous
}

func isShellTool(payload map[string]any) bool {
	toolName, _ := payload["tool_name"].(string)
	return shellToolNames[toolName]
}

// shellCommand is the command text of a shell tool call. Codex passes it as
// an argv array (["bash", "-lc", "<script>"]); the script is the last
// element.
func shellCommand(payload map[string]any) string {
	ti, ok := payload["tool_input"].(map[string]any)
	if !ok {
		return ""
	}
	switch v := ti["command"].(type) {
	case string:
		return v
	case []any:
		if len(v) > 0 {
			s, _ := v[len(v)-1].(string)
			return s
		}
	}
	return ""
}

// gitInvocation matches how a shell command calls git, up to the subcommand:
// `git` as a word of its own, by its path (/usr/bin/git) or past an alias
// (\git), quoted or not, then the options that may stand before the
// subcommand (-C dir, -c k=v, --no-pager).
const gitInvocation = `(^|[\s;&|(){}'"` + "`" + `])([^\s;&|(){}'"` + "`" + `]*[/\\])?git(\.exe)?['"]?` +
	`(\s+(-[cC]\s+\S+|--?[A-Za-z][\w-]*(=\S+)?))*\s+`

// movesContentRE matches a git invocation whose subcommand brings existing
// content into the working tree or moves it there: text nobody wrote during
// this call.
var movesContentRE = regexp.MustCompile(gitInvocation +
	`(stash\s+(pop|apply)|apply|am|mv|checkout|switch|restore|reset|pull|merge|rebase|cherry-pick|revert)\b`)

// commandMovesContent reports whether a shell command runs a git command
// that brings existing content into the working tree. A look after such a
// command credits nothing: what changed on disk may be a teammate's pulled
// commits, another branch, or a stash of hand-written work. `git stash pop`
// in particular leaves no trace in HEAD's reflog and is only recognisable
// from the command.
func commandMovesContent(command string) bool {
	return movesContentRE.MatchString(command)
}

// commitsRE matches a `git commit` invocation.
var commitsRE = regexp.MustCompile(gitInvocation + `commit\b`)

// commandCommits reports whether a shell command runs `git commit`.
func commandCommits(command string) bool {
	return commitsRE.MatchString(command)
}

// PendingLooks is how many looks are queued, for the worker's idle check.
func PendingLooks() int {
	db, err := store.Open()
	if err != nil {
		return 0
	}
	return db.CountPendingLooks()
}

// RunLooks runs the looks whose hook fired at least lookSettle before asOf,
// oldest first, and returns how many it ran. The worker calls it with the
// current time when the spool is empty.
func RunLooks(asOf time.Time) (int, error) {
	db, err := store.Open()
	if err != nil {
		return 0, err
	}
	looks, err := db.PendingLooks(asOf.Add(-lookSettle))
	if err != nil {
		return 0, err
	}
	ran := 0
	for _, look := range looks {
		// An event that arrived while the looks before this one ran may be
		// an edit of a file this look is about to read. Unless the look has
		// waited too long already, the worker handles the queue first.
		if queued, _ := spool.Pending(); queued > 0 && time.Since(look.FiredAt) < LookBacklog {
			break
		}
		runLook(db, look, time.Now())
		if err := db.DeletePendingLook(look.ID); err != nil {
			return ran, err
		}
		ran++
	}
	return ran, nil
}

// lookFile is one file a look has read.
type lookFile struct {
	rel, path string
	content   []byte
	readAt    time.Time
	prev      store.FileMap
	found     bool
	blob      string

	// What the look credited its event in this file.
	credited []int    // 1-based line numbers
	added    []uint64 // their exact fingerprints
	reworked []uint64 // fingerprints of the session's lines the call removed
	changed  bool     // authorship in the file changed
}

func runLook(db *store.DB, look store.PendingLook, now time.Time) {
	root := look.Root
	prev, looked := db.RepoLook(root)
	visited := map[string]bool{}
	defer func() {
		// A file that holds lines for this look and was not reached (git
		// did not answer, the file is gone, too many files) gives them up.
		for _, path := range look.Files {
			if !visited[path] {
				releaseHeld(db, path, look.EventID)
			}
		}
	}()

	// markBlind records that a look that was due could not be made: the
	// next one that can will only record.
	markBlind := func() {
		prev.Blind = true
		if !looked {
			prev.LookedAt = now
		}
		_ = db.SaveRepoLook(root, prev)
	}
	if looked && now.Before(prev.SlowUntil) {
		if !prev.Blind {
			markBlind()
		}
		return
	}
	status, err := gitStatus(root)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			logx.Warnf("git status did not answer within %s in %s; not looking there for %s", lookTimeout, root, slowRepoBackoff)
			prev.SlowUntil = now.Add(slowRepoBackoff)
		} else {
			logx.Debugf("look: git status failed in %s: %v", root, err)
		}
		markBlind()
		return
	}

	credit := look.Kind == store.LookShell && looked && !prev.Blind && !look.MovesContent && look.SessionID != "" &&
		now.Sub(look.FiredAt) <= maxLookAge && now.Sub(prev.LookedAt) <= maxLookGap
	base := status.head
	var rels []string
	rels = append(rels, status.files...)
	if looked && prev.Head != status.head {
		made, plain := commitsSince(root, prev.Head, status.head)
		if !plain {
			credit = false
		} else if before, any := beforeAgentCommits(db, look, made, prev.Head, prev.LookedAt); any {
			// The newest commits were made by an agent's shell command:
			// what they added was written during those calls. The base is
			// HEAD as it was before the first of them.
			base = before
			rels = append(rels, changedBetween(root, before, status.head)...)
		}
	}
	for _, path := range look.Files {
		if rel := gitx.RelPathInRepo(root, path); rel != "" {
			rels = append(rels, rel)
		}
	}

	// Read what changed.
	partial := status.partial
	var files []*lookFile
	seen := map[string]bool{}
	for _, rel := range rels {
		if seen[rel] || rel == "" {
			continue
		}
		seen[rel] = true
		if len(files) == maxLookFiles {
			logx.Debugf("look: more than %d changed files in %s, the rest are not read", maxLookFiles, root)
			partial = true
			break
		}
		lf := readLookFile(db, root, rel, look)
		if lf == nil {
			continue
		}
		visited[lf.path] = true
		files = append(files, lf)
	}

	if len(files) > 0 {
		names := make([]string, len(files))
		for i, lf := range files {
			names[i] = lf.rel
		}
		baseContent, answered := blobsAt(root, base, names)
		// New files that are in git nowhere at their path: copied or moved
		// from a file of the base commit, or written just now.
		var copied map[string]bool
		if credit {
			var fresh []string
			for _, lf := range files {
				if _, inGit := baseContent[lf.rel]; answered[lf.rel] && !inGit && !lf.found {
					fresh = append(fresh, lf.blob)
				}
			}
			if len(fresh) > 0 {
				copied = blobsInTree(root, base, fresh)
			}
		}
		for _, lf := range files {
			baseLines, inGit := baseContent[lf.rel]
			fileCredit := credit && answered[lf.rel]
			// A file with no map was unchanged at the last look, unless that
			// look could not read everything that had changed. A map made
			// since this look's command ran (by an edit handled just before)
			// says nothing about the time before it, unless it holds lines
			// for this look: the edit held them against the map the file had
			// before (updateAuthorship holds nothing in a file that had none
			// after such a look).
			if fileCredit && prev.Partial && !(lf.found && (!lf.prev.AsOf.After(look.FiredAt) || holdsFor(lf.prev, look.EventID))) {
				fileCredit = false
			}
			if fileCredit && !inGit && !lf.found && (copied == nil || copied[lf.blob]) {
				// The whole file is content the base commit already holds
				// (or git could not say): nobody wrote it.
				fileCredit = false
			}
			lookAtFile(db, look, lf, baseLines, inGit, fileCredit)
		}
		if look.Kind == store.LookShell {
			creditLook(db, look, files)
		}
	}

	if err := db.SaveRepoLook(root, store.RepoLook{Head: status.head, LookedAt: now, Partial: partial}); err != nil {
		logx.Warnf("look: recording the look at %s failed: %v", root, err)
	}

	// A commit made since the last look can hold lines this look has only
	// now given an author (a script and `git commit` in one command).
	changed := map[string]bool{}
	for _, lf := range files {
		if lf.changed {
			changed[lf.rel] = true
		}
	}
	since := look.FiredAt.Add(-10 * time.Minute)
	if looked {
		since = prev.LookedAt
	}
	reattributeRecentCommits(db, root, changed, since)
}

// readLookFile reads one changed file and its map. nil when there is
// nothing to do for it: the file is gone, is not text, is too large to keep
// a map for, or is exactly as its map has it.
func readLookFile(db *store.DB, root, rel string, look store.PendingLook) *lookFile {
	path := filepath.Join(root, filepath.FromSlash(rel))
	// Lstat: a symlink is not a file of lines, whatever it points at.
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > store.MaxSnapshotBytes {
		return nil
	}
	if look.Kind == store.LookPrompt && info.ModTime().After(look.FiredAt) {
		// Changed after the prompt was submitted, so not by a person between
		// turns: the worker got to the prompt late. The next look takes it.
		return nil
	}
	content, err := os.ReadFile(path)
	if err != nil || bytes.IndexByte(content[:min(len(content), 8000)], 0) >= 0 {
		return nil
	}
	lf := &lookFile{rel: rel, path: path, content: content, readAt: time.Now(), blob: gitBlobID(string(content))}
	lf.prev, lf.found = db.FileMap(path)
	if lf.found && lf.prev.HasAuthors() && lf.prev.Blob == lf.blob && !holdsFor(lf.prev, look.EventID) {
		// Exactly as the map has it. A file that stays changed for weeks
		// (a local config, a scratch file) must not lose its map to the
		// expiry of untouched rows: without it the file would look new.
		if lf.readAt.Sub(lf.prev.AsOf) > mapRefresh {
			lf.prev.AsOf = lf.readAt
			_ = db.SaveFileMap(path, lf.prev)
		}
		return nil
	}
	if lf.found && !lf.prev.HasAuthors() {
		lf.prev = seedMapAuthors(db, root, rel, lf.prev, "")
	}
	return lf
}

func holdsFor(m store.FileMap, eventID string) bool {
	for i := range m.Hashes {
		if a, held := m.HeldFor(i); held && a.EventID == eventID {
			return true
		}
	}
	return false
}

// lookAtFile lines the file's map up with what is on disk and settles the
// lines that are new: credited to the look's event, or to nobody.
func lookAtFile(db *store.DB, look store.PendingLook, lf *lookFile, baseContent string, inGit bool, credit bool) {
	lines := lineranges.Lines(string(lf.content))
	next, from := lf.prev.Carry(lines)

	// inBase marks the lines that line up with the base version in git.
	var inBase []bool
	basedOn := func(j int) bool {
		if !inGit {
			return false
		}
		if inBase == nil {
			inBase = make([]bool, len(lines))
			baseLines := lineranges.Lines(baseContent)
			norms := make([]uint64, len(baseLines))
			for i, l := range baseLines {
				norms[i] = store.NormHash(l)
			}
			for _, k := range lineranges.AlignHashes(norms, next.Norms) {
				if k >= 0 {
					inBase[k] = true
				}
			}
		}
		return inBase[j]
	}

	author := look.Author(false)
	for j := range lines {
		held, isHeld := next.HeldFor(j)
		if isHeld && held.EventID != look.EventID {
			continue // another look's to settle
		}
		if from[j] >= 0 && !isHeld {
			continue // a line the map already knew
		}
		if credit && !basedOn(j) {
			next.SetAuthor(j, author)
			lf.credited = append(lf.credited, j+1)
			lf.added = append(lf.added, next.Hashes[j])
			lf.changed = true
		} else if isHeld {
			next.ClearAuthor(j)
			lf.changed = true
		}
	}
	if credit {
		// The session's own earlier lines that this call removed.
		kept := make([]bool, len(lf.prev.Hashes))
		for _, i := range from {
			if i >= 0 {
				kept[i] = true
			}
		}
		for i := range lf.prev.Hashes {
			if a, ok := lf.prev.AuthorOf(i); ok && !kept[i] && a.SessionID == look.SessionID {
				lf.reworked = append(lf.reworked, lf.prev.Hashes[i])
			}
		}
	}
	next.AsOf = lf.readAt
	next.Blob = lf.blob
	if err := db.SaveFileMap(lf.path, next); err != nil {
		logx.Warnf("look: saving the map of %s failed: %v", lf.path, err)
	}
}

// releaseHeld drops the lines a file holds for a look that could not settle
// them.
func releaseHeld(db *store.DB, path, eventID string) {
	m, ok := db.FileMap(path)
	if !ok || !m.HasAuthors() {
		return
	}
	changed := false
	for i := range m.Hashes {
		if a, held := m.HeldFor(i); held && a.EventID == eventID {
			m.ClearAuthor(i)
			changed = true
		}
	}
	if changed {
		_ = db.SaveFileMap(path, m)
	}
}

// creditLook adds what a look found to its shell event: the lines it
// generated and reworked, the per-file split, the line ranges, and the
// session's ledger of written lines. An event the look credits nothing is
// left as it was.
func creditLook(db *store.DB, look store.PendingLook, files []*lookFile) {
	generated, reworked := 0, 0
	for _, lf := range files {
		generated += len(lf.credited)
		reworked += len(lf.reworked)
	}
	if generated == 0 && reworked == 0 {
		return
	}
	var gen0, rew0 sql.NullInt64
	var split0, ownPath, ownRel, eventRepo sql.NullString
	if err := db.QueryRow(`SELECT lines_generated, lines_reworked, file_lines, file_path, rel_file_path, repo_id FROM events WHERE id = ?`,
		look.EventID).Scan(&gen0, &rew0, &split0, &ownPath, &ownRel, &eventRepo); err != nil {
		return
	}
	// The row is there in all but the first event of a repository; asking
	// git for the remote again would cost a process per look.
	var repoID string
	if err := db.QueryRow(`SELECT id FROM repositories WHERE root_path = ?`, look.Root).Scan(&repoID); err != nil {
		if repoID, err = upsertRepositoryByCwd(db, look.Root); err != nil {
			logx.Warnf("look: no repository row for %s: %v", look.Root, err)
			return
		}
	}

	// The split as it stands: what the event's own capture measured.
	var split []fileLines
	if split0.Valid {
		_ = json.Unmarshal([]byte(split0.String), &split)
	} else if ownPath.Valid && (gen0.Valid || rew0.Valid) {
		own := fileLines{FilePath: ownPath.String, LinesGenerated: int(gen0.Int64), LinesReworked: int(rew0.Int64)}
		if eventRepo.Valid {
			id := eventRepo.String
			own.RepoID = &id
		}
		if ownRel.Valid {
			rel := ownRel.String
			own.RelFilePath = &rel
		}
		split = append(split, own)
	}
	at := map[string]int{}
	for i, f := range split {
		at[f.FilePath] = i
	}
	for _, lf := range files {
		if len(lf.credited) == 0 && len(lf.reworked) == 0 {
			continue
		}
		i, known := at[lf.path]
		if !known {
			rel, id := lf.rel, repoID
			split = append(split, fileLines{RepoID: &id, RelFilePath: &rel, FilePath: lf.path})
			i = len(split) - 1
			at[lf.path] = i
		}
		split[i].LinesGenerated += len(lf.credited)
		split[i].LinesReworked += len(lf.reworked)
	}

	// An event that named no file takes the first one the look credited.
	path, rel := ownPath, ownRel
	if !path.Valid && len(split) > 0 {
		path = sql.NullString{String: split[0].FilePath, Valid: true}
		if eventRepo.Valid && eventRepo.String == repoID && split[0].RelFilePath != nil {
			rel = sql.NullString{String: *split[0].RelFilePath, Valid: true}
		}
	}
	var encoded sql.NullString
	if !(len(split) == 1 && split[0].FilePath == path.String) {
		if raw, err := json.Marshal(split); err == nil {
			encoded = sql.NullString{String: string(raw), Valid: true}
		}
	}
	if _, err := db.Exec(`
		UPDATE events SET lines_generated = ?, lines_reworked = ?, file_lines = ?,
		       file_path = ?, rel_file_path = ?, synced_at = NULL
		WHERE id = ?`,
		int(gen0.Int64)+generated, int(rew0.Int64)+reworked, encoded, path, rel, look.EventID); err != nil {
		logx.Warnf("look: crediting event %s failed: %v", look.EventID, err)
		return
	}

	for _, lf := range files {
		if len(lf.credited) > 0 {
			relPath := sql.NullString{String: lf.rel, Valid: true}
			if err := insertLineRanges(db, look.EventID, lf.path, relPath, lf.content, "shell",
				lineranges.Extracted{Ranges: lineranges.RangesOf(lf.credited)}); err != nil {
				logx.Warnf("look: recording line ranges failed: %v", err)
			}
		}
		if look.SessionID == "" || (len(lf.added) == 0 && len(lf.reworked) == 0) {
			continue
		}
		// The session's ledger of the lines it has standing in the file.
		ledger := db.SessionFileLines(look.SessionID, lf.path)
		drop := counts(lf.reworked)
		kept := ledger[:0:0]
		for _, h := range ledger {
			if drop[h] > 0 {
				drop[h]--
				continue
			}
			kept = append(kept, h)
		}
		if err := db.SaveSessionFileLines(look.SessionID, lf.path, append(kept, lf.added...)); err != nil {
			logx.Warnf("look: updating the session's lines failed: %v", err)
		}
	}
	logx.Debugf("look: credited %d line(s), %d reworked, to shell event %s", generated, reworked, look.EventID)
}

// reattributeRecentCommits attributes again the commits of root, made at or
// after since, that touch one of the named files. A commit is attributed the
// moment it is made, by the git shadow; the worker handles the edits behind
// it a moment later. Without this the commit keeps what it was credited
// before those edits were known.
func reattributeRecentCommits(db *store.DB, root string, rels map[string]bool, since time.Time) {
	if len(rels) == 0 || root == "" {
		return
	}
	rows, err := db.Query(`
		SELECT c.id, c.sha, c.committed_at, c.repo_id
		FROM commits c JOIN repositories r ON r.id = c.repo_id
		WHERE r.root_path = ?
		ORDER BY c.rowid DESC LIMIT 20`, root)
	if err != nil {
		return
	}
	type recent struct{ id, sha, repoID string }
	var candidates []recent
	// A commit's time has one-second resolution.
	floor := since.Add(-time.Second)
	for rows.Next() {
		var c recent
		var at string
		if rows.Scan(&c.id, &c.sha, &at, &c.repoID) != nil {
			continue
		}
		if made, err := time.Parse(time.RFC3339, at); err == nil && !made.Before(floor) {
			candidates = append(candidates, c)
		}
	}
	rows.Close()
	var redo []recent
	for _, c := range candidates {
		files, err := db.Query(`SELECT file_path FROM commit_files WHERE commit_id = ?`, c.id)
		if err != nil {
			continue
		}
		touches := false
		for files.Next() {
			var file string
			if files.Scan(&file) == nil && rels[file] {
				touches = true
			}
		}
		files.Close()
		if touches {
			redo = append(redo, c)
		}
	}
	if len(redo) == 0 {
		return
	}
	noted := notedCommits(root)
	notesBefore := notesTip(root)
	remotes := map[string]bool{}
	// Oldest first, as they were made.
	for i := len(redo) - 1; i >= 0; i-- {
		c := redo[i]
		// Only a commit in the history of what is checked out. One that a
		// rewrite has replaced gave its lines up to its replacement, and
		// would take them back.
		if onBranch, err := gitx.IsAncestor(root, c.sha, "HEAD"); err != nil || !onBranch {
			continue
		}
		attributeAndNote(db, root, c.repoID, c.sha, c.id, noted[c.sha])
		if len(c.sha) >= 7 {
			logx.Debugf("attributed commit %s again: edits behind it were handled after it was made", c.sha[:7])
		}
		if remote := remoteHolding(root, c.sha); remote != "" {
			remotes[remote] = true
		}
	}
	// A commit that was pushed in the meantime went up without the note it
	// has only now been given (`gen.py && git commit && git push` in one
	// command). Best effort, like all notes transport.
	if len(remotes) > 0 && notes.Enabled() {
		if after := notesTip(root); after != "" && after != notesBefore {
			for remote := range remotes {
				if err := notes.Push(root, remote); err != nil {
					logx.Debugf("sending notes to %s failed: %v", remote, err)
				}
			}
		}
	}
}

// remoteHolding returns a remote that has the commit on one of its branches,
// as far as this checkout knows, or "".
func remoteHolding(root, sha string) string {
	out := gitx.Run(root, "for-each-ref", "--contains", sha, "--count=1", "--format=%(refname)", "refs/remotes")
	rest, ok := strings.CutPrefix(strings.TrimSpace(out), "refs/remotes/")
	if !ok {
		return ""
	}
	remote, _, _ := strings.Cut(rest, "/")
	return remote
}

// ── git, read-only and time-boxed ─────────────────────────────────────────

// lookGit runs git for a look: no optional locks, the real git, a timeout.
// A variable so a test can make one kind of call fail.
var lookGit = runLookGit

func runLookGit(root, stdin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lookTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks"}, args...)...)
	cmd.Dir = root
	cmd.Env = gitx.BypassEnv()
	// Killing git does not close a pipe a child of its own still holds (an
	// fsmonitor hook); without this the worker would wait on it for good.
	cmd.WaitDelay = 500 * time.Millisecond
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return out, err
}

// repoStatus is what `git status` says of a working tree.
type repoStatus struct {
	// head is the commit HEAD points at, empty before the first commit.
	head string
	// files are the paths, relative to the root, that differ from HEAD or
	// are untracked and not ignored.
	files []string
	// partial: there were more than are listed.
	partial bool
}

// gitStatus asks git which files differ from HEAD. Untracked directories
// come back as one entry (--untracked-files=normal), so git does not walk
// them; the files inside one are then listed for that directory only.
func gitStatus(root string) (repoStatus, error) {
	out, err := lookGit(root, "", "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=normal", "--no-renames")
	if err != nil {
		return repoStatus{}, err
	}
	status, dirs := parseStatus(string(out))
	if len(dirs) > maxLookDirs {
		dirs, status.partial = dirs[:maxLookDirs], true
	}
	for _, dir := range dirs {
		if len(status.files) >= maxLookFiles {
			status.partial = true
			break
		}
		listed, err := lookGit(root, "", "ls-files", "--others", "--exclude-standard", "-z", "--", dir)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return repoStatus{}, err
			}
			continue
		}
		for _, rel := range strings.Split(string(listed), "\x00") {
			if rel == "" || strings.HasSuffix(rel, "/") {
				continue
			}
			if len(status.files) >= maxLookFiles {
				status.partial = true
				break
			}
			status.files = append(status.files, rel)
		}
	}
	return status, nil
}

// parseStatus reads `git status --porcelain=v2 --branch -z`. dirs are the
// untracked directories, still to be listed.
func parseStatus(out string) (status repoStatus, dirs []string) {
	records := strings.Split(out, "\x00")
	for i := 0; i < len(records); i++ {
		rec := records[i]
		switch {
		case strings.HasPrefix(rec, "# branch.oid "):
			if oid := strings.TrimPrefix(rec, "# branch.oid "); oid != "(initial)" {
				status.head = oid
			}
		case strings.HasPrefix(rec, "1 "):
			if f := strings.SplitN(rec, " ", 9); len(f) == 9 && !deletedInTree(f[1]) {
				status.files = append(status.files, f[8])
			}
		case strings.HasPrefix(rec, "2 "):
			// A rename or copy: the path, then the path it came from as a
			// record of its own.
			if f := strings.SplitN(rec, " ", 10); len(f) == 10 && !deletedInTree(f[1]) {
				status.files = append(status.files, f[9])
			}
			i++
		case strings.HasPrefix(rec, "u "):
			if f := strings.SplitN(rec, " ", 11); len(f) == 11 {
				status.files = append(status.files, f[10])
			}
		case strings.HasPrefix(rec, "? "):
			if path := rec[2:]; strings.HasSuffix(path, "/") {
				dirs = append(dirs, path)
			} else {
				status.files = append(status.files, path)
			}
		}
	}
	return status, dirs
}

// deletedInTree reports whether a status XY pair says the file is gone from
// the working tree.
func deletedInTree(xy string) bool {
	return len(xy) == 2 && (xy[1] == 'D' || (xy[0] == 'D' && xy[1] == '.'))
}

// reflogCommit is one `git commit` in HEAD's reflog.
type reflogCommit struct {
	sha     string // what HEAD pointed at afterwards
	at      int64  // unix seconds
	subject string
	amend   bool
}

// commitsSince reads this checkout's reflog for how HEAD got from the commit
// `from` to the commit `to`. plain is true when it got there by `git commit`
// alone (a new commit, the first commit of a repository, or an amend), and
// made then lists those commits, newest first. Every other way HEAD moves (pull, merge,
// rebase, cherry-pick, revert, checkout, reset) brings in or rearranges
// content that was not written just now. With the reflog switched off, or
// `from` not in it, plain is false.
func commitsSince(root, from, to string) (made []reflogCommit, plain bool) {
	if from == "" || to == "" {
		return nil, false
	}
	out, err := lookGit(root, "", "log", "-g", "--date=unix", "--format=%H%x00%gd%x00%gs", "-n", strconv.Itoa(reflogDepth), "HEAD")
	if err != nil {
		return nil, false
	}
	for i, entry := range nonEmptyLines(string(out)) {
		fields := strings.SplitN(entry, "\x00", 3)
		if len(fields) != 3 {
			return nil, false
		}
		sha, selector, subject := fields[0], fields[1], fields[2]
		if i == 0 && sha != to {
			return nil, false
		}
		if sha == from {
			return made, i > 0
		}
		commit := reflogCommit{sha: sha}
		switch {
		case strings.HasPrefix(subject, "commit: "):
			commit.subject = strings.TrimPrefix(subject, "commit: ")
		case strings.HasPrefix(subject, "commit (initial): "):
			commit.subject = strings.TrimPrefix(subject, "commit (initial): ")
		case strings.HasPrefix(subject, "commit (amend): "):
			commit.subject, commit.amend = strings.TrimPrefix(subject, "commit (amend): "), true
		default:
			return nil, false
		}
		// The selector reads HEAD@{1763045000}.
		open, shut := strings.Index(selector, "{"), strings.LastIndex(selector, "}")
		if open < 0 || shut < open {
			return nil, false
		}
		if commit.at, err = strconv.ParseInt(selector[open+1:shut], 10, 64); err != nil {
			return nil, false
		}
		made = append(made, commit)
	}
	return nil, false
}

// beforeAgentCommits looks at the commits made since the last look, newest
// first, for those that can be put down to a shell command that runs
// `git commit`: this look's own command, or one whose look is still queued
// behind it. It returns the commit HEAD pointed at before the newest
// unbroken run of them (`from` when all of them are), and whether there is
// such a run at all. For a commit to count there has to be a command that
//
//   - had not ended before the commit was made, and whose session's
//     previous event, when that is known, had fired by then: the commit was
//     made while the command ran;
//   - names the commit: its text contains the commit's subject line, or,
//     for an amend, `--amend` (an amend usually keeps the message).
//
// A commit no command accounts for was made by the engineer, in another
// terminal, and what it added is theirs: the run stops there, and that
// commit is part of the base. Agents pass the message on the command line;
// one that comes from a file or an editor is not recognised, and the lines
// of that commit are not credited by the look.
//
// A commit's time has one-second resolution, hence the second of slack.
func beforeAgentCommits(db *store.DB, look store.PendingLook, made []reflogCommit, from string, lastLook time.Time) (before string, any bool) {
	var commands []store.PendingLook
	if look.Commits {
		commands = append(commands, look)
	}
	for _, queued := range db.CommittingLooks(look.Root) {
		if queued.ID != look.ID {
			commands = append(commands, queued)
		}
	}
	byCommand := func(commit reflogCommit) bool {
		if commit.at+1 < lastLook.Unix() {
			return false
		}
		for _, command := range commands {
			if commit.at > command.FiredAt.Unix()+1 {
				continue
			}
			if !command.StartedAt.IsZero() && commit.at+1 < command.StartedAt.Unix() {
				continue
			}
			if commit.amend && strings.Contains(command.Command, "--amend") {
				return true
			}
			if !commit.amend && commit.subject != "" && strings.Contains(command.Command, commit.subject) {
				return true
			}
		}
		return false
	}
	run := 0
	for run < len(made) && byCommand(made[run]) {
		run++
	}
	switch {
	case run == 0:
		return "", false
	case run == len(made):
		return from, true
	default:
		return made[run].sha, true
	}
}

// changedBetween lists the files that differ between two commits.
func changedBetween(root, from, to string) []string {
	out, err := lookGit(root, "", "diff", "--name-only", "-z", "--no-renames", from, to)
	if err != nil {
		return nil
	}
	var rels []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel != "" {
			rels = append(rels, rel)
		}
	}
	return rels
}

// blobsAt returns the content of the named files at a commit, for those
// that exist there. answered says for which files git gave an answer at all,
// content or "not there": a file it is missing for has an unknown base, and
// must not be taken for a file that is not in git. That is so when git
// failed or did not answer in time, and for a version larger than
// store.MaxSnapshotBytes, which is never read: a command that shrinks a huge
// generated file must not make the worker hold the old one in memory.
//
// One `git cat-file --batch` in all but that last case: git is stopped at a
// version that is too large, and asked again for the files after it.
func blobsAt(root, rev string, rels []string) (found map[string]string, answered map[string]bool) {
	found, answered = map[string]string{}, map[string]bool{}
	if len(rels) == 0 {
		return found, answered
	}
	if rev == "" {
		// A repository with no commit: nothing is in git yet.
		for _, rel := range rels {
			answered[rel] = true
		}
		return found, answered
	}
	var asked, names []string
	for _, rel := range rels {
		if strings.ContainsAny(rel, "\n\x00") {
			continue
		}
		asked = append(asked, rel)
		names = append(names, rev+":"+rel)
	}
	for calls := 0; len(asked) > 0 && calls <= maxLargeBases; calls++ {
		answers, err := lookBlobs(root, names, store.MaxSnapshotBytes)
		if err != nil {
			break
		}
		for i, a := range answers {
			switch {
			case a.tooLarge:
				// Not read: an unknown base.
			case a.isBlob:
				answered[asked[i]] = true
				found[asked[i]] = string(a.content)
			default:
				// Not there, or a directory or a submodule: no file of lines.
				answered[asked[i]] = true
			}
		}
		if len(answers) == 0 || !answers[len(answers)-1].tooLarge {
			// Every name is answered, or git stopped answering.
			break
		}
		asked, names = asked[len(answers):], names[len(answers):]
	}
	return found, answered
}

// maxLargeBases bounds how often one look asks git again after a version
// that was too large to read. The files after that stay unanswered.
const maxLargeBases = 4

// blobAnswer is what git says of one object asked for by name.
type blobAnswer struct {
	// isBlob: the name is a file; content is its text unless tooLarge.
	// Neither set: there is nothing under the name, or no file.
	isBlob   bool
	tooLarge bool
	content  []byte
}

// lookBlobs asks git for objects by name. A variable so a test can make git
// fail to answer, or watch what it is asked.
var lookBlobs = runLookBlobs

// runLookBlobs runs one `git cat-file --batch` for the named objects and
// returns git's answers, in order. Its output is read as it comes: at the
// first file larger than limit it stops, with that answer (tooLarge, no
// content) as the last, and git is stopped without its content having been
// read. Fewer answers than names otherwise means git stopped answering.
func runLookBlobs(root string, names []string, limit int64) ([]blobAnswer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lookTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "--no-optional-locks", "cat-file", "--batch")
	cmd.Dir = root
	cmd.Env = gitx.BypassEnv()
	cmd.WaitDelay = 500 * time.Millisecond
	cmd.Stdin = strings.NewReader(strings.Join(names, "\n") + "\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(stdout)
	var answers []blobAnswer
	for range names {
		// "<id> <type> <size>", then the content and a newline; or
		// "<name> missing".
		header, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		fields := strings.Fields(header)
		if len(fields) >= 2 && fields[len(fields)-1] == "missing" {
			answers = append(answers, blobAnswer{})
			continue
		}
		if len(fields) != 3 {
			break
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			break
		}
		if fields[1] == "blob" && size > limit {
			answers = append(answers, blobAnswer{isBlob: true, tooLarge: true})
			break
		}
		content := make([]byte, size+1)
		if _, err := readFull(reader, content); err != nil {
			break
		}
		answers = append(answers, blobAnswer{isBlob: fields[1] == "blob", content: content[:size]})
	}
	timedOut := ctx.Err() != nil
	// Stops git if it is still writing a file that will not be read.
	cancel()
	_ = cmd.Wait()
	if timedOut && len(answers) < len(names) {
		return answers, context.DeadlineExceeded
	}
	return answers, nil
}

// blobsInTree reports which of the given blob ids are the content of some
// file of the commit rev. nil when git could not say.
func blobsInTree(root, rev string, blobs []string) map[string]bool {
	in := map[string]bool{}
	if rev == "" {
		return in
	}
	want := map[string]bool{}
	for _, b := range blobs {
		want[b] = true
	}
	out, err := lookGit(root, "", "ls-tree", "-r", "-z", rev)
	if err != nil {
		return nil
	}
	// Each entry reads "<mode> <type> <id>\t<path>".
	for _, entry := range strings.Split(string(out), "\x00") {
		meta, _, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		if fields := strings.Fields(meta); len(fields) == 3 && fields[1] == "blob" && want[fields[2]] {
			in[fields[2]] = true
		}
	}
	return in
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
