package integlife

import (
	"path/filepath"
	"testing"
	"time"
)

func newSyncAckTestService(t *testing.T) (*Service, *Store, *time.Time) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "integlife.db"))
	if err != nil {
		t.Fatalf("OpenStore() error = %v", err)
	}
	t.Cleanup(func() { store.Close() })
	service := NewService(store, NewClient("", "", time.Second))
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	return service, store, &now
}

func ackBatch(t *testing.T, service *Service, store *Store) SyncAck {
	t.Helper()
	batch, err := store.PendingSyncBatch()
	if err != nil {
		t.Fatalf("PendingSyncBatch() error = %v", err)
	}
	return buildSyncAck(batch, syncResponse{}, "200 OK")
}

func TestApplySyncAckKeepsTodoEditedDuringRequest(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		service, store, now := newSyncAckTestService(t)
		created, err := service.AddTodo(testContext(), "first", "", "", 0)
		if err != nil {
			t.Fatalf("AddTodo() error = %v", err)
		}
		ack := ackBatch(t, service, store)
		if concurrent {
			*now = now.Add(time.Minute)
			edited := created.Todo
			edited.Content = "edited by another process"
			edited.ClientUpdatedAt = *now
			edited.SyncedAt = nil
			if err := store.SaveTodo(edited); err != nil {
				t.Fatalf("SaveTodo() error = %v", err)
			}
		}
		if err := service.applySyncAck(ack); err != nil {
			t.Fatalf("applySyncAck() error = %v", err)
		}
		pending, err := store.PendingTodos()
		if err != nil {
			t.Fatalf("PendingTodos() error = %v", err)
		}
		if concurrent && (len(pending) != 1 || pending[0].Content != "edited by another process") {
			t.Fatalf("pending todos = %+v, want the concurrently edited todo", pending)
		}
		if !concurrent && len(pending) != 0 {
			t.Fatalf("pending todos = %+v, want none after clean ack", pending)
		}
	}
}

func TestApplySyncAckKeepsTodoListEditedDuringRequest(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		service, store, now := newSyncAckTestService(t)
		created, err := service.AddTodoList(testContext(), "Work", "blue", "briefcase", 1)
		if err != nil {
			t.Fatalf("AddTodoList() error = %v", err)
		}
		ack := ackBatch(t, service, store)
		if concurrent {
			*now = now.Add(time.Minute)
			edited := created.List
			edited.Name = "Renamed"
			edited.ClientUpdatedAt = *now
			edited.SyncedAt = nil
			if err := store.SaveTodoList(edited); err != nil {
				t.Fatalf("SaveTodoList() error = %v", err)
			}
		}
		if err := service.applySyncAck(ack); err != nil {
			t.Fatalf("applySyncAck() error = %v", err)
		}
		pending, err := store.PendingTodoLists()
		if err != nil {
			t.Fatalf("PendingTodoLists() error = %v", err)
		}
		if concurrent && (len(pending) != 1 || pending[0].Name != "Renamed") {
			t.Fatalf("pending lists = %+v, want the renamed list", pending)
		}
		if !concurrent && len(pending) != 0 {
			t.Fatalf("pending lists = %+v, want none after clean ack", pending)
		}
	}
}

func TestApplySyncAckKeepsAITaskRunEditedDuringRequest(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		service, store, now := newSyncAckTestService(t)
		started, err := service.StartAITask(testContext(), AIStartOptions{
			Project: "goal:goal-1", TodoUUID: "todo-1", Title: "Implement", AgentName: "codex",
		})
		if err != nil {
			t.Fatalf("StartAITask() error = %v", err)
		}
		ack := ackBatch(t, service, store)
		if concurrent {
			*now = now.Add(time.Minute)
			if _, err := service.HeartbeatAITask(testContext(), started.Run.UUID, "newer heartbeat"); err != nil {
				t.Fatalf("HeartbeatAITask() error = %v", err)
			}
		}
		if err := service.applySyncAck(ack); err != nil {
			t.Fatalf("applySyncAck() error = %v", err)
		}
		pending, err := store.PendingAITaskRuns()
		if err != nil {
			t.Fatalf("PendingAITaskRuns() error = %v", err)
		}
		if concurrent && (len(pending) != 1 || pending[0].LatestSummary != "newer heartbeat") {
			t.Fatalf("pending runs = %+v, want the heartbeat run", pending)
		}
		if !concurrent && len(pending) != 0 {
			t.Fatalf("pending runs = %+v, want none after clean ack", pending)
		}
	}
}
