package integlife

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const aiWorklogTag = "#ai-worklog"

type NoteCommandResult struct {
	Note       NoteRecord
	Synced     bool
	SyncDetail string
}

func createNoteSchema(tx *sql.Tx) error {
	var tableExists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'notes'`).Scan(&tableExists); err != nil {
		return err
	}
	if tableExists > 0 {
		found, err := tableHasColumn(tx, "notes", "remote_path")
		if err != nil {
			return err
		}
		if !found {
			return migrateNotePaths(tx)
		}
	}
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS notes (
			uuid TEXT PRIMARY KEY,
			path TEXT NOT NULL,
			content TEXT NOT NULL,
			deleted_at TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			synced_at TEXT,
			synced_content_hash TEXT NOT NULL DEFAULT '',
			sync_etag TEXT NOT NULL DEFAULT '',
			sync_base_path TEXT NOT NULL DEFAULT '',
			remote_content TEXT NOT NULL DEFAULT '',
			remote_path TEXT NOT NULL DEFAULT '',
			remote_sync_etag TEXT NOT NULL DEFAULT '',
			remote_sync_base_path TEXT NOT NULL DEFAULT '',
			conflict_copy_uuid TEXT NOT NULL DEFAULT '',
			conflict_copy_path TEXT NOT NULL DEFAULT '',
			conflict_attempt_json TEXT NOT NULL DEFAULT '',
			remote_updated_at TEXT,
			remote_deleted_at TEXT,
			last_sync_error TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_notes_pending ON notes(synced_at, updated_at);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_notes_active_path ON notes(path) WHERE deleted_at IS NULL
	`); err != nil {
		return fmt.Errorf("create notes schema: %w", err)
	}
	return nil
}

func migrateNotePaths(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE notes RENAME TO notes_v5`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_notes_pending`); err != nil {
		return err
	}
	if err := createNoteSchema(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO notes (uuid,path,content,deleted_at,created_at,updated_at,synced_at,
		synced_content_hash,remote_content,remote_updated_at,remote_deleted_at,last_sync_error)
		SELECT uuid,path,content,deleted_at,created_at,updated_at,synced_at,
		synced_content_hash,remote_content,remote_updated_at,remote_deleted_at,last_sync_error FROM notes_v5`); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT uuid,path,synced_content_hash FROM notes WHERE synced_content_hash <> ''`)
	if err != nil {
		return err
	}
	type baseline struct{ uuid, path, digest string }
	var bases []baseline
	for rows.Next() {
		var base baseline
		if err := rows.Scan(&base.uuid, &base.path, &base.digest); err != nil {
			rows.Close()
			return err
		}
		bases = append(bases, base)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, base := range bases {
		if _, err := tx.Exec(`UPDATE notes SET synced_content_hash = ? WHERE uuid = ?`, notePathHash(base.path, base.digest), base.uuid); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`DROP TABLE notes_v5`)
	return err
}

func (s *Store) NotesDir() string {
	return s.notesDir
}

type noteExecutor interface {
	Exec(string, ...any) (sql.Result, error)
}

func (s *Store) SaveNote(note NoteRecord) error {
	return saveNote(s.db, note)
}

func saveNote(executor noteExecutor, note NoteRecord) error {
	attemptJSON := ""
	if note.ConflictAttempt != nil {
		encoded, err := json.Marshal(noteToSync(*note.ConflictAttempt))
		if err != nil {
			return err
		}
		attemptJSON = string(encoded)
	}
	_, err := executor.Exec(`
		INSERT INTO notes (
			uuid, path, content, deleted_at, created_at, updated_at, synced_at,
			synced_content_hash, sync_etag, sync_base_path, remote_content, remote_path, remote_sync_etag,
			remote_sync_base_path, conflict_copy_uuid, conflict_copy_path, conflict_attempt_json, remote_updated_at, remote_deleted_at, last_sync_error
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(uuid) DO UPDATE SET
			path = excluded.path,
			content = excluded.content,
			deleted_at = excluded.deleted_at,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at,
			synced_at = excluded.synced_at,
			synced_content_hash = excluded.synced_content_hash,
			sync_etag = excluded.sync_etag,
			sync_base_path = excluded.sync_base_path,
			remote_content = excluded.remote_content,
			remote_path = excluded.remote_path,
			remote_sync_etag = excluded.remote_sync_etag,
			remote_sync_base_path = excluded.remote_sync_base_path,
			conflict_copy_uuid = excluded.conflict_copy_uuid,
			conflict_copy_path = excluded.conflict_copy_path,
			conflict_attempt_json = excluded.conflict_attempt_json,
			remote_updated_at = excluded.remote_updated_at,
			remote_deleted_at = excluded.remote_deleted_at,
			last_sync_error = excluded.last_sync_error
	`, note.UUID, note.Path, note.Content, formatNullTime(note.DeletedAt), formatTime(note.CreatedAt),
		formatTime(note.UpdatedAt), formatNullTime(note.SyncedAt), note.SyncedContentHash,
		note.SyncETag, note.SyncBasePath, note.RemoteContent, note.RemotePath, note.RemoteSyncETag,
		note.RemoteSyncBasePath, note.ConflictCopyUUID, note.ConflictCopyPath, attemptJSON, formatNullTime(note.RemoteUpdatedAt), formatNullTime(note.RemoteDeletedAt),
		note.LastSyncError)
	if err != nil {
		return fmt.Errorf("save note: %w", err)
	}
	return nil
}

func (s *Store) Note(ref string) (NoteRecord, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return NoteRecord{}, fmt.Errorf("note reference must be non-empty")
	}
	rows, err := s.db.Query(noteSelectSQL()+`
		WHERE uuid = ? OR uuid LIKE ? OR ((path = ? OR path = ?) AND deleted_at IS NULL)
		ORDER BY updated_at DESC
		LIMIT 2
	`, ref, ref+"%", ref, filepath.Base(ref))
	if err != nil {
		return NoteRecord{}, fmt.Errorf("query note: %w", err)
	}
	defer rows.Close()
	var matches []NoteRecord
	for rows.Next() {
		note, err := scanNote(rows)
		if err != nil {
			return NoteRecord{}, err
		}
		matches = append(matches, note)
	}
	if len(matches) == 0 {
		return NoteRecord{}, fmt.Errorf("note not found: %s", ref)
	}
	if len(matches) > 1 {
		return NoteRecord{}, fmt.Errorf("note reference is ambiguous: %s", ref)
	}
	return matches[0], nil
}

func (s *Store) ListNotes(includeDeleted, conflictsOnly bool) ([]NoteRecord, error) {
	query := noteSelectSQL() + ` WHERE 1 = 1`
	if !includeDeleted {
		query += ` AND deleted_at IS NULL`
	}
	if conflictsOnly {
		query += ` AND last_sync_error LIKE 'conflict:%'`
	}
	query += ` ORDER BY updated_at DESC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query notes: %w", err)
	}
	defer rows.Close()
	var notes []NoteRecord
	for rows.Next() {
		note, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}

func (s *Store) PendingNotes() ([]NoteRecord, error) {
	rows, err := s.db.Query(noteSelectSQL() + `
		WHERE synced_at IS NULL AND last_sync_error NOT LIKE 'conflict:%'
		ORDER BY updated_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query pending notes: %w", err)
	}
	defer rows.Close()
	var notes []NoteRecord
	for rows.Next() {
		note, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}

func (s *Store) PendingNoteCount() (int, error) {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM notes WHERE synced_at IS NULL`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count pending notes: %w", err)
	}
	return count, nil
}

func (s *Store) MarkNoteSynced(uuid string, syncedAt time.Time, accepted ...NoteRecord) error {
	note, err := s.noteByUUID(uuid)
	if err != nil {
		return err
	}
	confirmed := note
	if len(accepted) > 0 {
		confirmed.SyncETag = accepted[0].SyncETag
		confirmed.SyncBasePath = accepted[0].SyncBasePath
	}
	return s.MarkNoteAcknowledged(note, confirmed, syncedAt)
}

// MarkNoteAcknowledged binds the server response to the snapshot sent in that request.
// Later edits stay pending; an acknowledgement cannot replace a newer remote baseline.
func (s *Store) MarkNoteAcknowledged(attempt, accepted NoteRecord, syncedAt time.Time) error {
	acceptedHash := noteStateHash(accepted.Content, accepted.DeletedAt, accepted.Path)
	if attempt.UUID == "" || accepted.UUID != attempt.UUID || acceptedHash != noteStateHash(attempt.Content, attempt.DeletedAt, attempt.Path) {
		return fmt.Errorf("note acknowledgement does not match the sent snapshot: %s", attempt.UUID)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := scanNote(tx.QueryRow(noteSelectSQL()+` WHERE uuid = ?`, attempt.UUID))
	if err != nil {
		return err
	}
	if current.SyncETag != attempt.SyncETag || current.SyncBasePath != attempt.SyncBasePath || current.SyncedContentHash != attempt.SyncedContentHash || strings.HasPrefix(current.LastSyncError, "conflict:") {
		return tx.Commit()
	}
	current.SyncETag, current.SyncBasePath = accepted.SyncETag, accepted.SyncBasePath
	if current.SyncETag != "" && current.SyncBasePath == "" {
		current.SyncBasePath = accepted.Path
	}
	current.SyncedContentHash = acceptedHash
	if noteStateHash(current.Content, current.DeletedAt, current.Path) == acceptedHash {
		current.SyncedAt = &syncedAt
	} else {
		current.SyncedAt = nil
	}
	clearNoteConflict(&current)
	if err := saveNote(tx, current); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkNoteSyncError(uuid, detail string) error {
	if _, err := s.db.Exec(`UPDATE notes SET last_sync_error = ? WHERE uuid = ?`, detail, uuid); err != nil {
		return fmt.Errorf("mark note sync error: %w", err)
	}
	return nil
}

func (s *Store) RefreshNoteFiles(now time.Time) error {
	notes, err := s.ListNotes(true, false)
	if err != nil {
		return err
	}
	for _, note := range notes {
		if note.DeletedAt != nil {
			// A recreated path carries no proof that it belongs to the deleted UUID.
			continue
		}
		path, err := s.noteFilePath(note.Path)
		if err != nil {
			return err
		}
		data, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) {
			if note.DeletedAt == nil && !strings.HasPrefix(note.LastSyncError, "conflict:") {
				deletedAt := now.UTC()
				note.DeletedAt = &deletedAt
				note.UpdatedAt = deletedAt
				note.SyncedAt = nil
				note.LastSyncError = ""
				if err := s.SaveNote(note); err != nil {
					return err
				}
			}
			continue
		}
		if readErr != nil {
			return fmt.Errorf("read note %s: %w", path, readErr)
		}
		content := string(data)
		if note.DeletedAt == nil && content == note.Content {
			continue
		}
		note.Content = content
		note.DeletedAt = nil
		note.UpdatedAt = now.UTC()
		note.SyncedAt = nil
		if !strings.HasPrefix(note.LastSyncError, "conflict:") {
			note.LastSyncError = ""
		}
		if err := s.SaveNote(note); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ApplyRemoteNote(remote NoteRecord, syncedAt time.Time) (bool, error) {
	local, err := s.noteByUUID(remote.UUID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		if remote.DeletedAt != nil || !isAIWorklog(remote.Content) {
			return false, nil
		}
		if remote.Path == "" {
			remote.Path = noteFilename(remote)
		}
		remote.SyncedAt = &syncedAt
		remote.SyncedContentHash = noteStateHash(remote.Content, remote.DeletedAt, remote.Path)
		return false, s.persistRemoteNote(remote, nil)
	}
	if remote.Path == "" {
		remote.Path = local.Path
	}

	localChanged := noteStateHash(local.Content, local.DeletedAt, local.Path) != local.SyncedContentHash
	remoteChanged := noteStateHash(remote.Content, remote.DeletedAt, remote.Path) != local.SyncedContentHash
	if local.SyncedContentHash == "" {
		remoteChanged = noteStateHash(remote.Content, remote.DeletedAt, remote.Path) != noteStateHash(local.Content, local.DeletedAt, local.Path)
	}
	if localChanged {
		if remoteChanged {
			return true, s.ApplyNoteConflict(local.UUID, remote)
		}
		return false, nil
	}

	remote.SyncedAt = &syncedAt
	remote.SyncedContentHash = noteStateHash(remote.Content, remote.DeletedAt, remote.Path)
	if err := s.persistRemoteNote(remote, &local); err != nil {
		return false, err
	}
	_ = s.removeRemoteNoteShadow(local)
	return false, nil
}

func (s *Store) ApplyNoteConflict(uuid string, remote NoteRecord, copies ...NoteRecord) error {
	local, err := s.noteByUUID(uuid)
	if err != nil {
		return err
	}
	previous := local
	attempt := noteFromSync(noteToSync(local))
	if len(copies) > 1 && copies[1].UUID == uuid {
		attempt = copies[1]
	} else if previous.ConflictAttempt != nil {
		attempt = *previous.ConflictAttempt
	}
	local.ConflictAttempt = &attempt
	local.RemoteContent = remote.Content
	local.RemotePath = remote.Path
	if local.RemotePath == "" {
		local.RemotePath = local.Path
	}
	local.RemoteSyncETag = remote.SyncETag
	local.RemoteSyncBasePath = remote.SyncBasePath
	if local.RemoteSyncETag != "" && local.RemoteSyncBasePath == "" {
		local.RemoteSyncBasePath = local.RemotePath
	}
	local.RemoteUpdatedAt = &remote.UpdatedAt
	local.RemoteDeletedAt = remote.DeletedAt
	if len(copies) > 0 && copies[0].UUID != "" {
		local.ConflictCopyUUID, local.ConflictCopyPath = copies[0].UUID, copies[0].Path
	}
	local.SyncedAt = nil
	local.LastSyncError = "conflict: local and server changed; merge the local and .remote.md files, then run life note resolve " + shortUUID(uuid) + " --local"
	shadowPath := strings.TrimSuffix(local.Path, filepath.Ext(local.Path)) + ".remote.md"
	ownedShadow, expectedBody := "", ""
	if strings.HasPrefix(previous.LastSyncError, "conflict:") {
		ownedShadow, expectedBody = shadowPath, remoteNoteBody(previous.RemoteContent, previous.RemoteDeletedAt)
	}
	overwrite, shadowErr := s.validateNoteTarget(uuid, shadowPath, ownedShadow, expectedBody)
	if shadowErr != nil {
		local.LastSyncError += "; remote shadow file preserved: " + shadowErr.Error()
	}
	if err := s.SaveNote(local); err != nil {
		return err
	}
	if shadowErr == nil {
		body := []byte(remoteNoteBody(remote.Content, remote.DeletedAt))
		if overwrite {
			shadowErr = writeFileAtomic(s.remoteNotePath(local.Path), body)
		} else {
			shadowErr = writeFileExclusive(s.remoteNotePath(local.Path), body)
		}
		if shadowErr != nil {
			local.LastSyncError += "; remote shadow unavailable: " + shadowErr.Error()
			return s.SaveNote(local)
		}
	}
	return nil
}

func (s *Store) ResolveNote(ref string, preferRemote bool, now time.Time) (NoteRecord, error) {
	note, err := s.Note(ref)
	if err != nil {
		return NoteRecord{}, err
	}
	if !strings.HasPrefix(note.LastSyncError, "conflict:") {
		return NoteRecord{}, fmt.Errorf("note has no unresolved conflict: %s", ref)
	}
	previous := note
	remotePath := note.RemotePath
	if remotePath == "" {
		remotePath = note.Path
	}
	note.SyncETag = note.RemoteSyncETag
	note.SyncBasePath = note.RemoteSyncBasePath
	if note.SyncETag != "" && note.SyncBasePath == "" {
		note.SyncBasePath = remotePath
	}
	if preferRemote {
		note.Content = note.RemoteContent
		note.Path = remotePath
		note.UpdatedAt = valueOrTime(note.RemoteUpdatedAt, now.UTC())
		note.DeletedAt = note.RemoteDeletedAt
		note.SyncedContentHash = noteStateHash(note.Content, note.DeletedAt, note.Path)
		syncedAt := now.UTC()
		note.SyncedAt = &syncedAt
	} else {
		path := s.mustNoteFilePath(note.Path)
		data, err := os.ReadFile(path)
		if err != nil {
			return NoteRecord{}, fmt.Errorf("read merged local note: %w", err)
		}
		note.Content = string(data)
		note.DeletedAt = nil
		note.SyncedContentHash = noteStateHash(note.RemoteContent, note.RemoteDeletedAt, remotePath)
		note.UpdatedAt = afterRemote(now.UTC(), note.RemoteUpdatedAt)
		note.SyncedAt = nil
	}
	clearNoteConflict(&note)
	if preferRemote {
		if err := s.persistRemoteNote(note, &previous); err != nil {
			return NoteRecord{}, err
		}
	} else if err := s.SaveNote(note); err != nil {
		return NoteRecord{}, err
	}
	_ = s.removeRemoteNoteShadow(previous)
	return note, nil
}

func clearNoteConflict(note *NoteRecord) {
	note.RemoteContent, note.RemotePath, note.RemoteSyncETag, note.RemoteSyncBasePath = "", "", "", ""
	note.ConflictCopyUUID, note.ConflictCopyPath = "", ""
	note.ConflictAttempt = nil
	note.RemoteUpdatedAt, note.RemoteDeletedAt = nil, nil
	note.LastSyncError = ""
}

func (s *Service) NewAIWorklog(title string) (NoteCommandResult, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return NoteCommandResult{}, fmt.Errorf("title must be non-empty")
	}
	now := s.now().UTC()
	note := NoteRecord{
		UUID:      newUUID(),
		Content:   "# " + title + "\n\n" + aiWorklogTag + "\n",
		CreatedAt: now,
		UpdatedAt: now,
	}
	note.Path = noteFilename(note)
	if err := s.store.persistRemoteNote(note, nil); err != nil {
		return NoteCommandResult{}, err
	}
	updated, err := s.store.Note(note.UUID)
	if err != nil {
		return NoteCommandResult{}, err
	}
	return NoteCommandResult{Note: updated, SyncDetail: "edit the Markdown file, then run life note sync"}, nil
}

func (s *Service) ListNotes(includeDeleted, conflictsOnly bool) ([]NoteRecord, error) {
	if err := s.store.RefreshNoteFiles(s.now()); err != nil {
		return nil, err
	}
	return s.store.ListNotes(includeDeleted, conflictsOnly)
}

func (s *Service) Note(ref string) (NoteRecord, error) {
	return s.store.Note(ref)
}

func (s *Service) NotesDir() string {
	return s.store.NotesDir()
}

func (s *Service) ResolveNote(ref string, preferRemote bool) (NoteRecord, error) {
	return s.store.ResolveNote(ref, preferRemote, s.now())
}

func (s *Store) noteByUUID(uuid string) (NoteRecord, error) {
	return scanNote(s.db.QueryRow(noteSelectSQL()+` WHERE uuid = ?`, uuid))
}

func (s *Store) noteFilePath(name string) (string, error) {
	name = strings.TrimSpace(name)
	rel := filepath.Clean(filepath.FromSlash(name))
	if name == "" || strings.Contains(name, "\\") || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid cached note path: %q", name)
	}
	if !strings.EqualFold(filepath.Ext(rel), ".md") {
		return "", fmt.Errorf("cached note path must end in .md: %q", name)
	}
	return filepath.Join(s.notesDir, rel), nil
}

func (s *Store) mustNoteFilePath(name string) string {
	path, _ := s.noteFilePath(name)
	return path
}

func (s *Store) remoteNotePath(name string) string {
	path := s.mustNoteFilePath(name)
	return strings.TrimSuffix(path, filepath.Ext(path)) + ".remote.md"
}

func (s *Store) writeNoteFile(name, content string) error {
	path, err := s.noteFilePath(name)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, []byte(content))
}

func writeFileAtomic(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create notes directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".note-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary note: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(content); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace cached note: %w", err)
	}
	return nil
}

func noteFilename(note NoteRecord) string {
	title := "ai-worklog"
	for _, line := range strings.Split(note.Content, "\n") {
		if value := strings.TrimSpace(strings.TrimPrefix(line, "# ")); strings.HasPrefix(line, "# ") && value != "" {
			title = value
			break
		}
	}
	createdAt := note.CreatedAt.UTC()
	stamp := createdAt.Format("20060102-1504")
	return filepath.ToSlash(filepath.Join(
		"ai-tasks",
		createdAt.Format("2006"),
		createdAt.Format("01"),
		fmt.Sprintf("%s-%s--%s.md", stamp, filenameSlug(title), filenameSlug(note.UUID)),
	))
}

func filenameSlug(value string) string {
	var out []rune
	dash := false
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out = append(out, r)
			dash = false
			continue
		}
		if !dash && len(out) > 0 {
			out = append(out, '-')
			dash = true
		}
	}
	slug := strings.Trim(string(out), "-")
	if slug == "" {
		return "ai-worklog"
	}
	return slug
}

func isAIWorklog(content string) bool {
	return strings.Contains(strings.ToLower(content), aiWorklogTag)
}

func noteStateHash(content string, deletedAt *time.Time, paths ...string) string {
	state := "active\x00"
	if deletedAt != nil {
		state = "deleted\x00"
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(state+content)))
	if len(paths) > 0 {
		return notePathHash(paths[0], digest)
	}
	return digest
}

func notePathHash(path, legacyDigest string) string {
	return fmt.Sprintf("v2:%x", sha256.Sum256([]byte(path+"\x00"+legacyDigest)))
}

func afterRemote(now time.Time, remote *time.Time) time.Time {
	if remote != nil && !now.After(*remote) {
		return remote.Add(time.Nanosecond)
	}
	return now
}

func valueOrTime(value *time.Time, fallback time.Time) time.Time {
	if value != nil {
		return value.UTC()
	}
	return fallback
}

func shortUUID(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}

func noteSelectSQL() string {
	return `SELECT uuid, path, content, deleted_at, created_at, updated_at, synced_at,
		synced_content_hash, sync_etag, sync_base_path, remote_content, remote_path, remote_sync_etag,
		remote_sync_base_path, conflict_copy_uuid, conflict_copy_path, conflict_attempt_json, remote_updated_at, remote_deleted_at, last_sync_error
		FROM notes`
}

func scanNote(scanner interface{ Scan(dest ...any) error }) (NoteRecord, error) {
	var note NoteRecord
	var attemptJSON string
	var deletedAt, syncedAt, remoteUpdatedAt, remoteDeletedAt sql.NullString
	var createdAt, updatedAt string
	if err := scanner.Scan(
		&note.UUID, &note.Path, &note.Content, &deletedAt, &createdAt, &updatedAt, &syncedAt,
		&note.SyncedContentHash, &note.SyncETag, &note.SyncBasePath, &note.RemoteContent, &note.RemotePath,
		&note.RemoteSyncETag, &note.RemoteSyncBasePath, &note.ConflictCopyUUID, &note.ConflictCopyPath, &attemptJSON, &remoteUpdatedAt, &remoteDeletedAt,
		&note.LastSyncError,
	); err != nil {
		return NoteRecord{}, err
	}
	if attemptJSON != "" {
		var attempt syncNote
		if err := json.Unmarshal([]byte(attemptJSON), &attempt); err != nil {
			return NoteRecord{}, fmt.Errorf("invalid cached conflict attempt: %w", err)
		}
		restored := noteFromSync(attempt)
		note.ConflictAttempt = &restored
	}
	var err error
	if note.DeletedAt, err = parseNullTime(deletedAt); err != nil {
		return NoteRecord{}, err
	}
	if note.SyncedAt, err = parseNullTime(syncedAt); err != nil {
		return NoteRecord{}, err
	}
	if note.RemoteUpdatedAt, err = parseNullTime(remoteUpdatedAt); err != nil {
		return NoteRecord{}, err
	}
	if note.RemoteDeletedAt, err = parseNullTime(remoteDeletedAt); err != nil {
		return NoteRecord{}, err
	}
	if note.CreatedAt, err = parseTime(createdAt); err != nil {
		return NoteRecord{}, err
	}
	if note.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return NoteRecord{}, err
	}
	return note, nil
}
