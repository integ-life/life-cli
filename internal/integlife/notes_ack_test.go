package integlife

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestNotesAcknowledgementBindsSentSnapshotAcrossConcurrentEdits(t *testing.T) {
	tests := []struct {
		name     string
		managed  bool
		mutation string
		newBase  string
	}{
		{name: "managed concurrent content", managed: true, mutation: "content"},
		{name: "managed concurrent path", managed: true, mutation: "path"},
		{name: "managed concurrent deletion", managed: true, mutation: "deletion"},
		{name: "managed unchanged", managed: true},
		{name: "ordinary noack concurrent content", mutation: "content"},
		{name: "ordinary noack unchanged"},
		{name: "newer remote etag remains current", managed: true, mutation: "content", newBase: "etag"},
		{name: "newer remote base path remains current", managed: true, mutation: "content", newBase: "path"},
		{name: "newer remote etag and path remain current", managed: true, mutation: "content", newBase: "both"},
		{name: "newer remote baseline hash remains current", managed: true, mutation: "content", newBase: "hash"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "integlife.db")
			store, err := OpenStore(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			path := "work/ack-snapshot.md"
			if test.managed {
				path = notesSafetyManagedPrefix + "ack-snapshot.md"
			}
			original := notesSafetyRemote("ack-snapshot", path, "Previously synced A\n", "etag-A")
			original.SyncBasePath = path
			original = notesSafetySeed(t, store, original)
			sent := original
			sent.Content += "Sent immutable B\n"
			sent.UpdatedAt = notesSafetyBase.Add(time.Minute)
			sent.SyncedAt = nil
			notesSafetyWrite(t, store, sent.Path, sent.Content)
			if err := store.SaveNote(sent); err != nil {
				t.Fatal(err)
			}

			current := sent
			switch test.mutation {
			case "content":
				current.Content += "Concurrent local C\n"
			case "path":
				current.Path = notesSafetyManagedPrefix + "local-C-moved.md"
			case "deletion":
				deletedAt := notesSafetyBase.Add(2 * time.Minute)
				current.DeletedAt = &deletedAt
			}
			if test.mutation != "" {
				current.UpdatedAt = notesSafetyBase.Add(2 * time.Minute)
			}
			if test.newBase != "" {
				newRemoteBase := notesSafetyRemote(sent.UUID, sent.SyncBasePath, "Accepted newer remote D\n", sent.SyncETag)
				if test.newBase == "etag" || test.newBase == "both" {
					newRemoteBase.SyncETag = "etag-D"
				}
				if test.newBase == "path" || test.newBase == "both" {
					newRemoteBase.Path = notesSafetyManagedPrefix + "remote-D.md"
				}
				current.SyncETag = newRemoteBase.SyncETag
				current.SyncBasePath = newRemoteBase.Path
				current.SyncedContentHash = noteStateHash(newRemoteBase.Content, newRemoteBase.DeletedAt, newRemoteBase.Path)
			}
			accepted := sent
			if test.managed {
				accepted.SyncETag = "etag-B"
				accepted.SyncBasePath = sent.Path
			}
			wantBaseline := noteStateHash(sent.Content, sent.DeletedAt, sent.Path)
			wantETag, wantBasePath := accepted.SyncETag, accepted.SyncBasePath
			if test.newBase != "" {
				wantBaseline = current.SyncedContentHash
				wantETag, wantBasePath = current.SyncETag, current.SyncBasePath
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request syncPayload
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode sent snapshot: %v", err)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				if len(request.Notes) != 1 || !reflect.DeepEqual(request.Notes[0], noteToSync(sent)) {
					t.Errorf("HTTP payload did not contain immutable B: %+v", request.Notes)
					http.Error(w, "wrong snapshot", http.StatusBadRequest)
					return
				}
				// Another local process writes C before the success response for B.
				writer, err := OpenStore(dbPath)
				if err != nil {
					t.Errorf("reopen concurrent writer: %v", err)
					http.Error(w, "writer unavailable", http.StatusInternalServerError)
					return
				}
				defer writer.Close()
				if current.DeletedAt != nil {
					err = os.Remove(filepath.Join(writer.NotesDir(), sent.Path))
				} else {
					err = writer.writeNoteFile(current.Path, current.Content)
					if err == nil && current.Path != sent.Path {
						err = os.Remove(filepath.Join(writer.NotesDir(), sent.Path))
					}
				}
				if err == nil {
					err = writer.SaveNote(current)
				}
				if err != nil {
					t.Errorf("persist concurrent C: %v", err)
					http.Error(w, "writer failed", http.StatusInternalServerError)
					return
				}
				response := map[string]any{"server_time": notesSafetyBase.Add(3 * time.Minute).Format(time.RFC3339Nano)}
				if test.managed {
					response["notes_server_newer"] = []syncNote{noteToSync(accepted)}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			client := NewClient(server.URL, "ack-test-token", time.Second)
			ack, err := client.SyncRecords(context.Background(), SyncBatch{Models: []string{"notes"}, Notes: []NoteRecord{sent}})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(ack.NoteSynced, []string{sent.UUID}) || len(ack.NoteErrors) != 0 || len(ack.NoteConflicts) != 0 {
				t.Fatalf("valid success for B was rejected: %+v", ack)
			}
			attempt, exists := ack.NoteAttempts[sent.UUID]
			if !exists || attempt.Content != sent.Content || attempt.Path != sent.Path || attempt.SyncETag != sent.SyncETag || attempt.SyncBasePath != sent.SyncBasePath {
				t.Fatalf("ack lost the immutable sent attempt: %+v", ack.NoteAttempts)
			}
			acknowledged, exists := ack.NoteAccepted[sent.UUID]
			if !exists || acknowledged.Content != sent.Content || acknowledged.Path != sent.Path {
				t.Fatalf("ack recorded current C instead of accepted B: %+v", ack.NoteAccepted)
			}
			if err := NewService(store, nil).applySyncAck(ack); err != nil {
				t.Fatal(err)
			}
			got := notesSafetyRead(t, store, sent.UUID)
			if got.Content != current.Content || got.Path != current.Path || !reflect.DeepEqual(got.DeletedAt, current.DeletedAt) || got.UpdatedAt != current.UpdatedAt {
				t.Fatalf("B acknowledgement changed concurrent C: want=%+v got=%+v", current, got)
			}
			if got.SyncedContentHash != wantBaseline || got.SyncETag != wantETag || got.SyncBasePath != wantBasePath {
				t.Fatalf("B acknowledgement bound the wrong state or rolled back newer D: want hash=%s etag=%s path=%s got=%+v", wantBaseline, wantETag, wantBasePath, got)
			}
			if test.mutation == "" {
				if got.SyncedAt == nil {
					t.Fatalf("unchanged accepted B remained pending: %+v", got)
				}
			} else {
				if got.SyncedAt != nil {
					t.Fatalf("concurrent C was falsely marked synced by B: %+v", got)
				}
				pending, err := store.PendingNotes()
				if err != nil || len(pending) != 1 || pending[0].UUID != sent.UUID {
					t.Fatalf("concurrent C disappeared from pending uploads: pending=%+v err=%v", pending, err)
				}
			}
			if current.DeletedAt == nil {
				notesSafetyAssertFile(t, store, current.Path, current.Content)
			}
			// Persisted baselines must also survive a process restart.
			reopened, err := OpenStore(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			restored := notesSafetyRead(t, reopened, sent.UUID)
			if !reflect.DeepEqual(restored, got) {
				t.Fatalf("restart changed acknowledged state: before=%+v after=%+v", got, restored)
			}
		})
	}
}
