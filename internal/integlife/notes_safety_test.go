package integlife

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var notesSafetyBase = time.Date(2026, 10, 11, 4, 0, 0, 0, time.UTC)

const notesSafetyManagedPrefix = "vaults/11111111-1111-1111-1111-111111111111/"

func notesSafetyOpenStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "integlife.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func notesSafetyRemote(uuid, path, content, etag string) NoteRecord {
	return NoteRecord{UUID: uuid, Path: path, Content: "# Worklog\n\n#ai-worklog\n" + content, CreatedAt: notesSafetyBase, UpdatedAt: notesSafetyBase, SyncETag: etag}
}

func notesSafetySeed(t *testing.T, store *Store, note NoteRecord) NoteRecord {
	t.Helper()
	conflict, err := store.ApplyRemoteNote(note, notesSafetyBase)
	if err != nil || conflict {
		t.Fatalf("seed remote note: conflict=%v err=%v", conflict, err)
	}
	note, err = store.noteByUUID(note.UUID)
	if err != nil {
		t.Fatal(err)
	}
	return note
}

func notesSafetyWrite(t *testing.T, store *Store, path, content string) {
	t.Helper()
	fullPath := filepath.Join(store.NotesDir(), path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func notesSafetyAssertFile(t *testing.T, store *Store, path, content string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(store.NotesDir(), path))
	if err != nil || string(data) != content {
		t.Fatalf("note file %q changed: got=%q err=%v want=%q", path, data, err, content)
	}
}

func notesSafetyRead(t *testing.T, store *Store, uuid string) NoteRecord {
	t.Helper()
	note, err := store.noteByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	return note
}

func notesSafetyLegacyDigest(content string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte("active\x00"+content)))
}

func TestNotesSafetyV5MigrationPreservesCleanAndDirtyBaselines(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "integlife.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE notes (
			uuid TEXT PRIMARY KEY, path TEXT NOT NULL UNIQUE, content TEXT NOT NULL,
			deleted_at TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
			synced_at TEXT, synced_content_hash TEXT NOT NULL DEFAULT '',
			remote_content TEXT NOT NULL DEFAULT '', remote_updated_at TEXT,
			remote_deleted_at TEXT, last_sync_error TEXT NOT NULL DEFAULT ''
		);
		PRAGMA user_version = 5
	`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	clean := notesSafetyRemote("migration-clean", "migration/clean.md", "Original clean content\n", "")
	dirty := notesSafetyRemote("migration-dirty", "migration/dirty.md", "Original dirty content\n", "")
	dirtyOriginal := dirty.Content
	dirty.Content += "Local edits before upgrade\n"
	stamp := notesSafetyBase.Format(time.RFC3339Nano)
	for _, note := range []NoteRecord{clean, dirty} {
		baseline := note.Content
		var synced any = stamp
		if note.UUID == dirty.UUID {
			baseline = dirtyOriginal
			synced = nil
		}
		if _, err := db.Exec(`INSERT INTO notes(uuid,path,content,created_at,updated_at,synced_at,synced_content_hash) VALUES(?,?,?,?,?,?,?)`, note.UUID, note.Path, note.Content, stamp, stamp, synced, notesSafetyLegacyDigest(baseline)); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		fullPath := filepath.Join(dir, "notes", note.Path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(note.Content), 0o600); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 6 {
		t.Fatalf("v5 schema did not migrate to v6: version=%d err=%v", version, err)
	}
	migratedClean := notesSafetyRead(t, store, clean.UUID)
	migratedDirty := notesSafetyRead(t, store, dirty.UUID)
	if migratedClean.SyncedAt == nil || migratedDirty.SyncedAt != nil || migratedClean.SyncedContentHash == notesSafetyLegacyDigest(clean.Content) || migratedDirty.SyncedContentHash == notesSafetyLegacyDigest(dirtyOriginal) {
		t.Fatalf("legacy baseline was not migrated while retaining dirty state: clean=%+v dirty=%+v", migratedClean, migratedDirty)
	}
	clean.Content += "Server update after upgrade\n"
	clean.UpdatedAt = notesSafetyBase.Add(time.Minute)
	if conflict, err := store.ApplyRemoteNote(clean, notesSafetyBase.Add(2*time.Minute)); err != nil || conflict {
		t.Fatalf("clean legacy baseline became a conflict: conflict=%v err=%v", conflict, err)
	}
	notesSafetyAssertFile(t, store, clean.Path, clean.Content)
	unchangedServer := dirty
	unchangedServer.Content = dirtyOriginal
	if conflict, err := store.ApplyRemoteNote(unchangedServer, notesSafetyBase.Add(2*time.Minute)); err != nil || conflict {
		t.Fatalf("unchanged server baseline produced conflict: conflict=%v err=%v", conflict, err)
	}
	notesSafetyAssertFile(t, store, dirty.Path, dirty.Content)
	if got := notesSafetyRead(t, store, dirty.UUID); got.Content != dirty.Content || got.SyncedAt != nil {
		t.Fatalf("migration incorrectly marked dirty local content clean: %+v", got)
	}
	unchangedServer.Content += "Concurrent server edit\n"
	if conflict, err := store.ApplyRemoteNote(unchangedServer, notesSafetyBase.Add(3*time.Minute)); err != nil || !conflict {
		t.Fatalf("dirty legacy content lost its original baseline: conflict=%v err=%v", conflict, err)
	}
	notesSafetyAssertFile(t, store, dirty.Path, dirty.Content)
	// The rebuilt schema allows a deleted UUID and a new active UUID to share
	// the historical path, while active UUIDs remain unique.
	deletedAt := notesSafetyBase.Add(4 * time.Minute)
	tombstone := migratedClean
	tombstone.DeletedAt = &deletedAt
	if err := store.SaveNote(tombstone); err != nil {
		t.Fatal(err)
	}
	replacement := notesSafetyRemote("migration-replacement", clean.Path, "Replacement\n", "")
	if err := store.SaveNote(replacement); err != nil {
		t.Fatalf("v6 active-path index still forbids tombstone reuse: %v", err)
	}
	duplicate := replacement
	duplicate.UUID = "migration-active-collision"
	if err := store.SaveNote(duplicate); err == nil {
		t.Fatal("v6 path uniqueness allowed two active UUIDs")
	}
}

func TestNotesSafetyRemoteRenameConflictResolution(t *testing.T) {
	for _, preferRemote := range []bool{true, false} {
		name := "local"
		if preferRemote {
			name = "remote"
		}
		t.Run(name, func(t *testing.T) {
			store := notesSafetyOpenStore(t)
			local := notesSafetySeed(t, store, notesSafetyRemote("rename-conflict", "work/original.md", "Baseline\n", "etag-1"))
			localContent := local.Content + "Local edit\n"
			notesSafetyWrite(t, store, local.Path, localContent)
			if err := store.RefreshNoteFiles(notesSafetyBase.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			remote := local
			remote.Path = "archive/renamed.md"
			remote.SyncETag = "etag-2"
			remote.UpdatedAt = notesSafetyBase.Add(2 * time.Minute)
			if conflict, err := store.ApplyRemoteNote(remote, notesSafetyBase.Add(3*time.Minute)); err != nil || !conflict {
				t.Fatalf("rename and local edit did not conflict: conflict=%v err=%v", conflict, err)
			}
			conflicted := notesSafetyRead(t, store, local.UUID)
			if conflicted.RemotePath != remote.Path || conflicted.RemoteSyncETag != remote.SyncETag || conflicted.SyncETag != "etag-1" {
				t.Fatalf("conflict lost remote path or ETag baseline: %+v", conflicted)
			}
			notesSafetyAssertFile(t, store, local.Path, localContent)
			resolved, err := store.ResolveNote(local.UUID, preferRemote, notesSafetyBase.Add(4*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if resolved.SyncETag != "etag-2" || resolved.RemotePath != "" || resolved.RemoteSyncETag != "" || resolved.LastSyncError != "" {
				t.Fatalf("resolution lost accepted server baseline or conflict cleanup: %+v", resolved)
			}
			if _, err := os.Stat(store.remoteNotePath(local.Path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("old conflict sidecar remains: %v", err)
			}
			if preferRemote {
				if resolved.Path != remote.Path || resolved.Content != remote.Content || resolved.SyncedAt == nil {
					t.Fatalf("remote resolution did not adopt path/content: %+v", resolved)
				}
				notesSafetyAssertFile(t, store, remote.Path, remote.Content)
				if _, err := os.Stat(filepath.Join(store.NotesDir(), local.Path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("remote resolution left the original file: %v", err)
				}
			} else {
				if resolved.Path != local.Path || resolved.Content != localContent || resolved.SyncedAt != nil {
					t.Fatalf("local resolution did not preserve local path/content: %+v", resolved)
				}
				notesSafetyAssertFile(t, store, local.Path, localContent)
				if conflict, err := store.ApplyRemoteNote(remote, notesSafetyBase.Add(5*time.Minute)); err != nil || conflict {
					t.Fatalf("accepted remote baseline conflicted again during lookback: conflict=%v err=%v", conflict, err)
				}
				notesSafetyAssertFile(t, store, local.Path, localContent)
			}
		})
	}
}

func TestNotesSafetyPathCollisionPreservesFilesAndDatabase(t *testing.T) {
	for _, resolution := range []bool{false, true} {
		for _, tracked := range []bool{false, true} {
			name := fmt.Sprintf("resolve=%v/tracked=%v", resolution, tracked)
			t.Run(name, func(t *testing.T) {
				store := notesSafetyOpenStore(t)
				local := notesSafetySeed(t, store, notesSafetyRemote("collision-source", "work/source.md", "Source\n", "source-etag"))
				remote := local
				remote.Path = "work/occupied.md"
				remote.UpdatedAt = notesSafetyBase.Add(time.Minute)
				if resolution {
					notesSafetyWrite(t, store, local.Path, local.Content+"Local edit\n")
					if err := store.RefreshNoteFiles(notesSafetyBase.Add(time.Minute)); err != nil {
						t.Fatal(err)
					}
					if conflict, err := store.ApplyRemoteNote(remote, notesSafetyBase.Add(2*time.Minute)); err != nil || !conflict {
						t.Fatalf("could not prepare rename conflict: conflict=%v err=%v", conflict, err)
					}
				}
				targetContent := "Untracked personal content must survive\n"
				if tracked {
					target := notesSafetySeed(t, store, notesSafetyRemote("collision-target", remote.Path, "Other UUID content\n", "target-etag"))
					targetContent = target.Content
				} else {
					notesSafetyWrite(t, store, remote.Path, targetContent)
				}
				before, err := store.ListNotes(true, false)
				if err != nil {
					t.Fatal(err)
				}
				sourceBefore := notesSafetyRead(t, store, local.UUID)
				if resolution {
					_, err = store.ResolveNote(local.UUID, true, notesSafetyBase.Add(3*time.Minute))
				} else {
					_, err = store.ApplyRemoteNote(remote, notesSafetyBase.Add(3*time.Minute))
				}
				if err == nil {
					t.Fatal("occupied target path was accepted")
				}
				after, err := store.ListNotes(true, false)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("collision changed database records: before=%+v after=%+v", before, after)
				}
				notesSafetyAssertFile(t, store, local.Path, sourceBefore.Content)
				notesSafetyAssertFile(t, store, remote.Path, targetContent)
				if resolution {
					data, err := os.ReadFile(store.remoteNotePath(local.Path))
					if err != nil || string(data) != remote.Content {
						t.Fatalf("collision destroyed conflict sidecar: got=%q err=%v", data, err)
					}
				}
			})
		}
	}
}

func TestNotesSafetyDeletedPathReuseDoesNotResurrectOrDeleteReplacement(t *testing.T) {
	store := notesSafetyOpenStore(t)
	old := notesSafetySeed(t, store, notesSafetyRemote("old-path-owner", "work/reusable.md", "Old UUID\n", "old-etag"))
	deletedAt := notesSafetyBase.Add(time.Minute)
	old.DeletedAt = &deletedAt
	old.UpdatedAt = deletedAt
	if conflict, err := store.ApplyRemoteNote(old, notesSafetyBase.Add(2*time.Minute)); err != nil || conflict {
		t.Fatalf("could not delete original owner: conflict=%v err=%v", conflict, err)
	}
	replacement := notesSafetySeed(t, store, notesSafetyRemote("new-path-owner", old.Path, "New UUID\n", "new-etag"))
	if err := store.RefreshNoteFiles(notesSafetyBase.Add(3 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if tombstone := notesSafetyRead(t, store, old.UUID); tombstone.DeletedAt == nil || tombstone.Content != old.Content {
		t.Fatalf("replacement file resurrected the previous UUID: %+v", tombstone)
	}
	byPath, err := store.Note(old.Path)
	if err != nil || byPath.UUID != replacement.UUID {
		t.Fatalf("path lookup did not select the active owner: note=%+v err=%v", byPath, err)
	}
	old.UpdatedAt = notesSafetyBase.Add(4 * time.Minute)
	if conflict, err := store.ApplyRemoteNote(old, notesSafetyBase.Add(5*time.Minute)); err != nil || conflict {
		t.Fatalf("replayed old deletion failed: conflict=%v err=%v", conflict, err)
	}
	notesSafetyAssertFile(t, store, replacement.Path, replacement.Content)
	if got := notesSafetyRead(t, store, replacement.UUID); !reflect.DeepEqual(got, replacement) {
		t.Fatalf("old deletion changed the replacement UUID: before=%+v after=%+v", replacement, got)
	}
}

func TestNotesSafetyDeletedPathDoesNotOwnUnknownLocalData(t *testing.T) {
	store := notesSafetyOpenStore(t)
	old := notesSafetySeed(t, store, notesSafetyRemote("deleted-unknown-owner", "work/reused-locally.md", "Old note\n", ""))
	deletedAt := notesSafetyBase.Add(time.Minute)
	old.DeletedAt = &deletedAt
	old.UpdatedAt = deletedAt
	if conflict, err := store.ApplyRemoteNote(old, notesSafetyBase.Add(2*time.Minute)); err != nil || conflict {
		t.Fatalf("delete old note: conflict=%v err=%v", conflict, err)
	}
	unknownContent := "Untracked local data at a tombstoned path\n"
	notesSafetyWrite(t, store, old.Path, unknownContent)
	before := notesSafetyRead(t, store, old.UUID)
	if err := store.RefreshNoteFiles(notesSafetyBase.Add(3 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if after := notesSafetyRead(t, store, old.UUID); !reflect.DeepEqual(after, before) {
		t.Fatalf("unknown local file resurrected a tombstone: before=%+v after=%+v", before, after)
	}
	replacement := notesSafetyRemote("unknown-path-replacement", old.Path, "Incoming new UUID\n", "")
	if _, err := store.ApplyRemoteNote(replacement, notesSafetyBase.Add(4*time.Minute)); err == nil {
		t.Fatal("incoming new UUID overwrote unknown local content")
	}
	notesSafetyAssertFile(t, store, old.Path, unknownContent)
	if _, err := store.noteByUUID(replacement.UUID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed path reuse left a new database record: %v", err)
	}
	if after := notesSafetyRead(t, store, old.UUID); !reflect.DeepEqual(after, before) {
		t.Fatalf("failed new UUID import changed old tombstone: before=%+v after=%+v", before, after)
	}
}

func TestNotesSafetyOptionalETagWireAndDatabaseRestart(t *testing.T) {
	note := notesSafetyRemote("etag-roundtrip", "work/etag.md", "Wire content\n", "server-version-7")
	encoded, err := json.Marshal(noteToSync(note))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"sync_etag":"server-version-7"`) {
		t.Fatalf("wire payload omitted the opaque ETag: %s", encoded)
	}
	var wire syncNote
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if restored := noteFromSync(wire); restored.SyncETag != note.SyncETag || restored.Path != note.Path || restored.Content != note.Content {
		t.Fatalf("wire round-trip changed note or ETag: %+v", restored)
	}
	note.SyncETag = ""
	withoutETag, err := json.Marshal(noteToSync(note))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(withoutETag), "sync_etag") {
		t.Fatalf("legacy payload gained a non-optional ETag: %s", withoutETag)
	}
	note.SyncETag = "server-version-7"
	note.RemoteSyncETag = "server-version-8"
	note.RemotePath = "archive/etag-renamed.md"
	dbPath := filepath.Join(t.TempDir(), "integlife.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveNote(note); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restored := notesSafetyRead(t, store, note.UUID)
	if restored.SyncETag != note.SyncETag || restored.RemoteSyncETag != note.RemoteSyncETag || restored.RemotePath != note.RemotePath {
		t.Fatalf("DB restart lost conflict ETags/path: before=%+v after=%+v", note, restored)
	}
}

func notesSafetyDecodeResponse(t *testing.T, fields map[string]any) syncResponse {
	t.Helper()
	fields["server_time"] = notesSafetyBase.Add(10 * time.Minute).Format(time.RFC3339Nano)
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var response syncResponse
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestNotesSafetyManagedAcceptanceRefreshesBaselineWithoutConflict(t *testing.T) {
	for _, mutation := range []string{"content", "path only"} {
		t.Run(mutation, func(t *testing.T) {
			store := notesSafetyOpenStore(t)
			original := notesSafetySeed(t, store, notesSafetyRemote("managed-accepted", notesSafetyManagedPrefix+"work/managed.md", "Baseline\n", "etag-original"))
			pending := original
			pending.SyncedAt = nil
			pending.UpdatedAt = notesSafetyBase.Add(time.Minute)
			if mutation == "content" {
				pending.Content += "Uploaded local content\n"
			} else {
				pending.Path = notesSafetyManagedPrefix + "archive/managed-moved.md"
				if err := os.Remove(filepath.Join(store.NotesDir(), original.Path)); err != nil {
					t.Fatal(err)
				}
			}
			notesSafetyWrite(t, store, pending.Path, pending.Content)
			if err := store.SaveNote(pending); err != nil {
				t.Fatal(err)
			}
			accepted := pending
			accepted.SyncBasePath = pending.Path
			if mutation == "content" {
				accepted.SyncETag = "etag-accepted"
			}
			response := notesSafetyDecodeResponse(t, map[string]any{"notes_server_newer": []syncNote{noteToSync(accepted)}})
			ack := buildSyncAck(SyncBatch{Models: []string{"notes"}, Notes: []NoteRecord{pending}}, response, "managed acceptance")
			if !reflect.DeepEqual(ack.NoteSynced, []string{pending.UUID}) || len(ack.NoteConflicts) != 0 || len(ack.NoteErrors) != 0 {
				t.Fatalf("matching managed acceptance became a conflict/error: %+v", ack)
			}
			if err := NewService(store, nil).applySyncAck(ack); err != nil {
				t.Fatal(err)
			}
			got := notesSafetyRead(t, store, pending.UUID)
			if got.SyncedAt == nil || got.SyncETag != accepted.SyncETag || got.SyncBasePath != pending.Path || got.LastSyncError != "" || got.Path != pending.Path || got.Content != pending.Content {
				t.Fatalf("managed acceptance did not retain state and fresh baseline: %+v", got)
			}
			notesSafetyAssertFile(t, store, pending.Path, pending.Content)
			if _, err := os.Stat(store.remoteNotePath(pending.Path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("accepted managed upload produced a conflict sidecar: %v", err)
			}
		})
	}
}

func TestNotesSafetyExplicitConflictRetainsCurrentAndCopyReferences(t *testing.T) {
	store := notesSafetyOpenStore(t)
	original := notesSafetySeed(t, store, notesSafetyRemote("explicit-conflict", "work/conflicting.md", "Baseline\n", "etag-before"))
	notesSafetyWrite(t, store, original.Path, original.Content+"Local attempt\n")
	if err := store.RefreshNoteFiles(notesSafetyBase.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	pending := notesSafetyRead(t, store, original.UUID)
	current := original
	current.Path = "archive/server-current.md"
	current.Content += "Server current content\n"
	current.SyncETag = "etag-current"
	current.SyncBasePath = current.Path
	copy := pending
	copy.UUID = "deterministic-conflict-copy"
	copy.Path = "conflicts/deterministic-copy.md"
	copy.SyncETag = "etag-copy"
	copy.SyncBasePath = copy.Path
	response := notesSafetyDecodeResponse(t, map[string]any{
		"notes_conflicts":      []map[string]any{{"current": noteToSync(current), "attempted": noteToSync(pending), "conflict_copy": noteToSync(copy)}},
		"notes_server_newer":   []syncNote{noteToSync(current)},
		"notes_only_on_server": []syncNote{noteToSync(copy)},
	})
	ack := buildSyncAck(SyncBatch{Models: []string{"notes"}, Notes: []NoteRecord{pending}}, response, "explicit conflict")
	if len(ack.NoteSynced) != 0 || len(ack.NoteConflicts) != 1 || ack.NoteConflicts[0].UUID != pending.UUID || ack.NoteConflictCopies[pending.UUID].UUID != copy.UUID || ack.NoteConflictCopies[pending.UUID].Path != copy.Path {
		t.Fatalf("explicit conflict was duplicated, accepted, or lost its copy reference: %+v", ack)
	}
	if err := NewService(store, nil).applySyncAck(ack); err != nil {
		t.Fatal(err)
	}
	got := notesSafetyRead(t, store, pending.UUID)
	if got.Path != pending.Path || got.Content != pending.Content || got.RemotePath != current.Path || got.RemoteContent != current.Content || got.RemoteSyncETag != current.SyncETag || got.ConflictCopyUUID != copy.UUID || got.ConflictCopyPath != copy.Path || !strings.HasPrefix(got.LastSyncError, "conflict:") {
		t.Fatalf("explicit conflict overwrote local attempt or omitted server references: %+v", got)
	}
	notesSafetyAssertFile(t, store, pending.Path, pending.Content)
}

func TestNotesSafetyMissingManagedAcknowledgementRemainsPending(t *testing.T) {
	for _, missing := range []string{"entire ack", "etag", "base path", "wrong base path"} {
		t.Run(missing, func(t *testing.T) {
			store := notesSafetyOpenStore(t)
			original := notesSafetySeed(t, store, notesSafetyRemote("managed-missing-ack", notesSafetyManagedPrefix+"work/missing-ack.md", "Baseline\n", "etag-before"))
			notesSafetyWrite(t, store, original.Path, original.Content+"Local pending upload\n")
			if err := store.RefreshNoteFiles(notesSafetyBase.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			pending := notesSafetyRead(t, store, original.UUID)
			accepted := pending
			accepted.SyncETag = "etag-after"
			accepted.SyncBasePath = accepted.Path
			fields := map[string]any{}
			switch missing {
			case "etag":
				accepted.SyncETag = ""
			case "base path":
				accepted.SyncBasePath = ""
			case "wrong base path":
				accepted.SyncBasePath = "other/incorrect-base.md"
			}
			if missing != "entire ack" {
				fields["notes_server_newer"] = []syncNote{noteToSync(accepted)}
			}
			ack := buildSyncAck(SyncBatch{Models: []string{"notes"}, Notes: []NoteRecord{pending}}, notesSafetyDecodeResponse(t, fields), "missing managed acknowledgement")
			if len(ack.NoteSynced) != 0 || ack.NoteErrors[pending.UUID] == "" {
				t.Fatalf("managed upload without fresh acknowledgement was marked accepted: %+v", ack)
			}
			if err := NewService(store, nil).applySyncAck(ack); err != nil {
				t.Fatal(err)
			}
			got := notesSafetyRead(t, store, pending.UUID)
			if got.SyncedAt != nil || got.LastSyncError == "" || got.SyncETag != original.SyncETag || got.Content != pending.Content || got.Path != pending.Path {
				t.Fatalf("missing ack lost pending state or replaced the baseline: %+v", got)
			}
			notesSafetyAssertFile(t, store, pending.Path, pending.Content)
		})
	}
}

func TestNotesSafetyServiceAppliesDeletionBeforeSameResponsePathReuse(t *testing.T) {
	store := notesSafetyOpenStore(t)
	old := notesSafetySeed(t, store, notesSafetyRemote("response-old-owner", "work/reused-response.md", "Old owner\n", "old-etag"))
	deletedAt := notesSafetyBase.Add(time.Minute)
	old.DeletedAt = &deletedAt
	old.UpdatedAt = deletedAt
	replacement := notesSafetyRemote("response-new-owner", old.Path, "New owner\n", "new-etag")
	response := notesSafetyDecodeResponse(t, map[string]any{"notes_only_on_server": []syncNote{noteToSync(replacement), noteToSync(old)}})
	ack := buildSyncAck(SyncBatch{Models: []string{"notes"}}, response, "active row precedes deletion")
	if err := NewService(store, nil).applySyncAck(ack); err != nil {
		t.Fatalf("same-response path reuse was applied before releasing its previous owner: %v", err)
	}
	if got := notesSafetyRead(t, store, old.UUID); got.DeletedAt == nil {
		t.Fatalf("old owner was not tombstoned: %+v", got)
	}
	got := notesSafetyRead(t, store, replacement.UUID)
	if got.DeletedAt != nil || got.Path != replacement.Path || got.Content != replacement.Content {
		t.Fatalf("new owner failed to take the released path: %+v", got)
	}
	notesSafetyAssertFile(t, store, replacement.Path, replacement.Content)
}

func TestNotesSafetyOrdinaryNoteWithETagRetainsLegacyAcknowledgement(t *testing.T) {
	store := notesSafetyOpenStore(t)
	original := notesSafetySeed(t, store, notesSafetyRemote("ordinary-legacy-ack", "work/ordinary.md", "Ordinary note\n", "ordinary-etag"))
	notesSafetyWrite(t, store, original.Path, original.Content+"Local upload\n")
	if err := store.RefreshNoteFiles(notesSafetyBase.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	pending := notesSafetyRead(t, store, original.UUID)
	ack := buildSyncAck(SyncBatch{Models: []string{"notes"}, Notes: []NoteRecord{pending}}, notesSafetyDecodeResponse(t, map[string]any{}), "legacy acknowledgement")
	if !reflect.DeepEqual(ack.NoteSynced, []string{pending.UUID}) || len(ack.NoteErrors) != 0 || len(ack.NoteConflicts) != 0 {
		t.Fatalf("ordinary note acquired the managed vault acknowledgement gate: %+v", ack)
	}
	if err := NewService(store, nil).applySyncAck(ack); err != nil {
		t.Fatal(err)
	}
	got := notesSafetyRead(t, store, pending.UUID)
	if got.SyncedAt == nil || got.LastSyncError != "" || got.Path != pending.Path || got.Content != pending.Content {
		t.Fatalf("ordinary note was left pending despite legacy server acceptance: %+v", got)
	}
	notesSafetyAssertFile(t, store, pending.Path, pending.Content)
}

func TestNotesSafetyLocalPathOnlyChangeUsesOriginalBaseline(t *testing.T) {
	store := notesSafetyOpenStore(t)
	original := notesSafetySeed(t, store, notesSafetyRemote("local-path-only", "work/path-original.md", "Unchanged local content\n", "etag-baseline"))
	local := original
	local.Path = "archive/local-path.md"
	local.SyncedAt = nil
	local.UpdatedAt = notesSafetyBase.Add(time.Minute)
	notesSafetyWrite(t, store, local.Path, local.Content)
	if err := os.Remove(filepath.Join(store.NotesDir(), original.Path)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveNote(local); err != nil {
		t.Fatal(err)
	}
	remote := original
	remote.Content += "Concurrent remote content\n"
	remote.SyncETag = "etag-changed"
	if conflict, err := store.ApplyRemoteNote(remote, notesSafetyBase.Add(2*time.Minute)); err != nil || !conflict {
		t.Fatalf("path-only local change was treated as a clean baseline: conflict=%v err=%v", conflict, err)
	}
	notesSafetyAssertFile(t, store, local.Path, local.Content)
	got := notesSafetyRead(t, store, local.UUID)
	if got.Path != local.Path || got.RemotePath != remote.Path || got.RemoteContent != remote.Content {
		t.Fatalf("path-only conflict did not retain both paths: %+v", got)
	}
}

func TestNotesSafetyConflictAttemptRemainsImmutableAcrossEditAndRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "integlife.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	original := notesSafetyRemote("immutable-attempt", "work/attempt-original.md", "Baseline\n", "etag-original")
	original.SyncBasePath = original.Path
	original = notesSafetySeed(t, store, original)
	notesSafetyWrite(t, store, original.Path, original.Content+"First attempted upload\n")
	if err := store.RefreshNoteFiles(notesSafetyBase.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	attempted := notesSafetyRead(t, store, original.UUID)
	current := original
	current.Path = "archive/current-at-conflict.md"
	current.Content += "Concurrent server content\n"
	current.SyncETag = "etag-current"
	current.SyncBasePath = current.Path
	response := notesSafetyDecodeResponse(t, map[string]any{
		"notes_conflicts": []map[string]any{{"current": noteToSync(current), "attempted": noteToSync(attempted)}},
	})
	ack := buildSyncAck(SyncBatch{Models: []string{"notes"}, Notes: []NoteRecord{attempted}}, response, "snapshot attempted upload")
	if snapshot, ok := ack.NoteConflictAttempts[attempted.UUID]; !ok || snapshot.Content != attempted.Content || snapshot.Path != attempted.Path || snapshot.SyncBasePath != attempted.SyncBasePath {
		t.Fatalf("explicit attempted snapshot was lost before storage: %+v", ack.NoteConflictAttempts)
	}
	if err := NewService(store, nil).applySyncAck(ack); err != nil {
		t.Fatal(err)
	}
	laterContent := attempted.Content + "Later local work after the conflict\n"
	notesSafetyWrite(t, store, attempted.Path, laterContent)
	if err := store.RefreshNoteFiles(notesSafetyBase.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	store = reopened
	got := notesSafetyRead(t, store, attempted.UUID)
	if got.Content != laterContent || got.Path != attempted.Path || got.ConflictAttempt == nil {
		t.Fatalf("restart lost local edits or attempted snapshot: %+v", got)
	}
	snapshot := got.ConflictAttempt
	if snapshot.UUID != attempted.UUID || snapshot.Content != attempted.Content || snapshot.Path != attempted.Path || snapshot.SyncETag != attempted.SyncETag || snapshot.SyncBasePath != attempted.SyncBasePath {
		t.Fatalf("later local edits changed the immutable attempted state: attempted=%+v saved=%+v", attempted, snapshot)
	}
	notesSafetyAssertFile(t, store, got.Path, laterContent)
}

func TestNotesSafetyConflictSidecarCollisionPreservesProtectedBytes(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		t.Run(fmt.Sprintf("tracked=%v", tracked), func(t *testing.T) {
			store := notesSafetyOpenStore(t)
			original := notesSafetySeed(t, store, notesSafetyRemote("sidecar-source", "work/sidecar-source.md", "Baseline\n", "etag-original"))
			localContent := original.Content + "Local work\n"
			notesSafetyWrite(t, store, original.Path, localContent)
			if err := store.RefreshNoteFiles(notesSafetyBase.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			protectedPath := "work/sidecar-source.remote.md"
			protectedContent := "Unrelated scratch file at the sidecar target\n"
			var protectedNote NoteRecord
			if tracked {
				protectedNote = notesSafetySeed(t, store, notesSafetyRemote("sidecar-other-uuid", protectedPath, "Other UUID owns this file\n", "etag-other"))
				protectedContent = protectedNote.Content
			} else {
				notesSafetyWrite(t, store, protectedPath, protectedContent)
			}
			remote := original
			remote.Path = "archive/sidecar-server.md"
			remote.Content += "Server content for conflict shadow\n"
			remote.SyncETag = "etag-server"
			remote.SyncBasePath = remote.Path
			if err := store.ApplyNoteConflict(original.UUID, remote); err != nil {
				t.Fatalf("sidecar collision prevented durable conflict metadata: %v", err)
			}
			got := notesSafetyRead(t, store, original.UUID)
			if got.Content != localContent || got.RemoteContent != remote.Content || got.RemotePath != remote.Path || got.RemoteSyncETag != remote.SyncETag || !strings.HasPrefix(got.LastSyncError, "conflict:") {
				t.Fatalf("sidecar collision discarded the remote shadow metadata: %+v", got)
			}
			notesSafetyAssertFile(t, store, protectedPath, protectedContent)
			if _, err := store.ResolveNote(original.UUID, false, notesSafetyBase.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			notesSafetyAssertFile(t, store, protectedPath, protectedContent)
			notesSafetyAssertFile(t, store, original.Path, localContent)
			if tracked {
				if after := notesSafetyRead(t, store, protectedNote.UUID); !reflect.DeepEqual(after, protectedNote) {
					t.Fatalf("conflict/resolve changed the sidecar's real owner: before=%+v after=%+v", protectedNote, after)
				}
			}
		})
	}
}

func TestNotesSafetyManagedBaselineStillRequiresAckAfterMoveOutsideVault(t *testing.T) {
	store := notesSafetyOpenStore(t)
	original := notesSafetyRemote("managed-moved-out", notesSafetyManagedPrefix+"original.md", "Managed note\n", "etag-vault")
	original.SyncBasePath = original.Path
	original = notesSafetySeed(t, store, original)
	pending := original
	pending.Path = "work/moved-outside-vault.md"
	pending.SyncedAt = nil
	pending.UpdatedAt = notesSafetyBase.Add(time.Minute)
	notesSafetyWrite(t, store, pending.Path, pending.Content)
	if err := os.Remove(filepath.Join(store.NotesDir(), original.Path)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveNote(pending); err != nil {
		t.Fatal(err)
	}
	ack := buildSyncAck(SyncBatch{Models: []string{"notes"}, Notes: []NoteRecord{pending}}, notesSafetyDecodeResponse(t, map[string]any{}), "managed baseline moved out")
	if len(ack.NoteSynced) != 0 || ack.NoteErrors[pending.UUID] == "" {
		t.Fatalf("moving outside the vault bypassed its baseline acknowledgement gate: %+v", ack)
	}
	if err := NewService(store, nil).applySyncAck(ack); err != nil {
		t.Fatal(err)
	}
	if got := notesSafetyRead(t, store, pending.UUID); got.SyncedAt != nil || got.SyncBasePath != original.Path || got.LastSyncError == "" {
		t.Fatalf("missing move acknowledgement changed the managed baseline: %+v", got)
	}
	notesSafetyAssertFile(t, store, pending.Path, pending.Content)
}
