package integlife

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func managedNotePath(path string) bool {
	parts := strings.SplitN(path, "/", 3)
	if len(parts) != 3 || parts[0] != "vaults" || parts[2] == "" || len(parts[1]) != 36 {
		return false
	}
	for i, char := range parts[1] {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if char != '-' {
				return false
			}
		} else if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

func remoteNoteBody(content string, deletedAt *time.Time) string {
	if deletedAt != nil {
		return "<!-- The server deleted this note. -->\n"
	}
	return content
}

func (s *Store) removeRemoteNoteShadow(previous NoteRecord) error {
	if !strings.HasPrefix(previous.LastSyncError, "conflict:") {
		return nil
	}
	shadowPath := strings.TrimSuffix(previous.Path, filepath.Ext(previous.Path)) + ".remote.md"
	owner, err := s.activeNotePathOwner(shadowPath)
	if err != nil || owner != "" {
		return err
	}
	target := s.remoteNotePath(previous.Path)
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	if string(data) != remoteNoteBody(previous.RemoteContent, previous.RemoteDeletedAt) {
		return nil
	}
	return os.Remove(target)
}

func (s *Store) activeNotePathOwner(path string) (string, error) {
	rows, err := s.db.Query(`SELECT uuid,path FROM notes WHERE deleted_at IS NULL`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var uuid, candidate string
		if err := rows.Scan(&uuid, &candidate); err != nil {
			return "", err
		}
		// Cache portability requires rejecting names that collide on macOS/Windows.
		if strings.EqualFold(filepath.Clean(candidate), filepath.Clean(path)) {
			return uuid, nil
		}
	}
	return "", rows.Err()
}

func (s *Store) validateNoteTarget(uuid, path, ownedPath, expectedContent string) (bool, error) {
	target, err := s.noteFilePath(path)
	if err != nil {
		return false, err
	}
	owner, err := s.activeNotePathOwner(path)
	if err != nil {
		return false, err
	}
	if owner != "" && owner != uuid {
		return false, fmt.Errorf("cached note path belongs to another UUID: %s", path)
	}
	rel, _ := filepath.Rel(s.notesDir, target)
	current := s.notesDir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("cached note path contains a symlink: %s", path)
		}
	}
	info, err := os.Stat(target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	owned := false
	if ownedPath != "" {
		oldPath, err := s.noteFilePath(ownedPath)
		if err != nil {
			return false, err
		}
		oldInfo, err := os.Stat(oldPath)
		owned = err == nil && os.SameFile(info, oldInfo)
	}
	if !owned {
		return false, fmt.Errorf("cached note target already contains an unrelated file: %s", path)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return false, err
	}
	if string(data) != expectedContent {
		return false, fmt.Errorf("cached note file changed during sync; retry after refreshing: %s", path)
	}
	return true, nil
}

func (s *Store) persistRemoteNote(remote NoteRecord, previous *NoteRecord) error {
	ownedPath, expectedContent := "", ""
	if previous != nil && previous.DeletedAt == nil {
		ownedPath, expectedContent = previous.Path, previous.Content
	}
	var overwrite bool
	if remote.DeletedAt == nil {
		var err error
		overwrite, err = s.validateNoteTarget(remote.UUID, remote.Path, ownedPath, expectedContent)
		if err != nil {
			return err
		}
	}
	if remote.SyncETag != "" && remote.SyncBasePath == "" {
		remote.SyncBasePath = remote.Path
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Reserve the active namespace in SQLite before any file mutation.
	if err := saveNote(tx, remote); err != nil {
		return err
	}
	if remote.DeletedAt == nil {
		target := s.mustNoteFilePath(remote.Path)
		if overwrite {
			err = writeFileAtomic(target, []byte(remote.Content))
		} else {
			err = writeFileExclusive(target, []byte(remote.Content))
		}
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if previous != nil && previous.DeletedAt == nil && (remote.DeletedAt != nil || previous.Path != remote.Path) {
		return s.removePreviousNoteFile(*previous, remote.Path, remote.DeletedAt == nil)
	}
	return nil
}

func (s *Store) removePreviousNoteFile(previous NoteRecord, replacementPath string, hasReplacement bool) error {
	owner, err := s.activeNotePathOwner(previous.Path)
	if err != nil || (owner != "" && owner != previous.UUID) {
		return err
	}
	oldPath := s.mustNoteFilePath(previous.Path)
	if hasReplacement {
		oldInfo, oldErr := os.Stat(oldPath)
		newInfo, newErr := os.Stat(s.mustNoteFilePath(replacementPath))
		if oldErr == nil && newErr == nil && os.SameFile(oldInfo, newInfo) {
			return nil
		}
	}
	data, err := os.ReadFile(oldPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if string(data) != previous.Content {
		return nil // Preserve an edit that arrived after the server request started.
	}
	return os.Remove(oldPath)
}

func writeFileExclusive(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".note-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(content); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	// Linking publishes the complete file atomically and refuses an existing target.
	return os.Link(temp.Name(), path)
}
