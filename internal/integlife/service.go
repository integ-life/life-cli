package integlife

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Service struct {
	store  *Store
	client *Client
	now    func() time.Time
}

type AIStartOptions struct {
	Project         string
	TodoUUID        string
	Title           string
	AgentName       string
	SessionID       string
	SessionExplicit bool
	NewRun          bool
}

type AIResumeOptions struct {
	Project         string
	TodoUUID        string
	AgentName       string
	SessionID       string
	SessionExplicit bool
	NewRun          bool
	Title           string
}

type AIEventOptions struct {
	RunUUID      string
	EventType    string
	Severity     string
	Title        string
	Content      string
	MetadataJSON string
}

type AICommandResult struct {
	Run        AITaskRunRecord
	Event      *AITaskEventRecord
	Synced     bool
	SyncDetail string
}

type TodoCommandResult struct {
	Todo       TodoRecord
	Synced     bool
	SyncDetail string
}

type AddTodoOptions struct {
	Content       string
	Notes         string
	ListRef       string
	ParentRef     string
	Order         float64
	Deadline      *time.Time
	GoalUUID      string
	MilestoneUUID string
}

type TodoListOptions struct {
	IncludeDeleted  bool
	CompletedFilter *bool
	ListRef         string
	ParentRef       string
	ParentFilter    bool
	RootOnly        bool
	GoalUUID        string
	MilestoneUUID   string
	DeadlineBefore  *time.Time
	DeadlineAfter   *time.Time
	Search          string
}

type TodoListCommandResult struct {
	List       TodoListRecord
	Synced     bool
	SyncDetail string
}

type TodoReplyCommandResult struct {
	Reply      TodoReplyRecord
	Todo       TodoRecord
	Synced     bool
	SyncDetail string
}

func NewService(store *Store, client *Client) *Service {
	return &Service{
		store:  store,
		client: client,
		now:    time.Now,
	}
}

func (s *Service) AddTodo(ctx context.Context, content, notes, listRef string, order float64) (TodoCommandResult, error) {
	return s.AddTodoWithOptions(ctx, AddTodoOptions{
		Content: content,
		Notes:   notes,
		ListRef: listRef,
		Order:   order,
	})
}

func (s *Service) AddTodoWithOptions(ctx context.Context, opts AddTodoOptions) (TodoCommandResult, error) {
	content := strings.TrimSpace(opts.Content)
	content = strings.TrimSpace(content)
	if content == "" {
		return TodoCommandResult{}, fmt.Errorf("content must be non-empty")
	}
	listUUID, err := s.resolveOptionalListUUID(opts.ListRef)
	if err != nil {
		return TodoCommandResult{}, err
	}
	parentUUID, err := s.resolveOptionalTodoUUID(opts.ParentRef)
	if err != nil {
		return TodoCommandResult{}, fmt.Errorf("resolve parent: %w", err)
	}
	now := s.now().UTC()
	todo := TodoRecord{
		UUID:               newUUID(),
		ParentUUID:         parentUUID,
		Content:            content,
		Notes:              opts.Notes,
		SortOrder:          opts.Order,
		ListUUID:           listUUID,
		Deadline:           opts.Deadline,
		GoalUUID:           strings.TrimSpace(opts.GoalUUID),
		MilestoneUUID:      strings.TrimSpace(opts.MilestoneUUID),
		CompletionMode:     "manual",
		CompletionSource:   "manual",
		AIEvaluationStatus: "not_requested",
		CreatedAt:          now,
		UpdatedAt:          now,
		ClientCreatedAt:    now,
		ClientUpdatedAt:    now,
	}
	if err := s.inheritParentTodoList(&todo); err != nil {
		return TodoCommandResult{}, err
	}
	if err := s.store.SaveTodo(todo); err != nil {
		return TodoCommandResult{}, err
	}
	if err := s.cascadeTodoListToChildren(todo); err != nil {
		return TodoCommandResult{}, err
	}
	synced, detail := s.bestEffortSync(ctx)
	return TodoCommandResult{Todo: todo, Synced: synced, SyncDetail: detail}, nil
}

func (s *Service) UpdateTodo(ctx context.Context, ref string, mutate func(*TodoRecord) error) (TodoCommandResult, error) {
	todo, err := s.store.ResolveTodo(ref)
	if err != nil {
		return TodoCommandResult{}, err
	}
	if mutate != nil {
		if err := mutate(&todo); err != nil {
			return TodoCommandResult{}, err
		}
	}
	if err := s.inheritParentTodoList(&todo); err != nil {
		return TodoCommandResult{}, err
	}
	now := s.now().UTC()
	todo.UpdatedAt = now
	todo.ClientUpdatedAt = now
	todo.SyncedAt = nil
	todo.LastSyncError = ""
	if err := s.store.SaveTodo(todo); err != nil {
		return TodoCommandResult{}, err
	}
	if err := s.cascadeTodoListToChildren(todo); err != nil {
		return TodoCommandResult{}, err
	}
	synced, detail := s.bestEffortSync(ctx)
	return TodoCommandResult{Todo: todo, Synced: synced, SyncDetail: detail}, nil
}

func (s *Service) CompleteTodo(ctx context.Context, ref string, completed bool) (TodoCommandResult, error) {
	return s.UpdateTodo(ctx, ref, func(todo *TodoRecord) error {
		now := s.now().UTC()
		todo.Completed = completed
		if completed {
			todo.CompletedAt = &now
		} else {
			todo.CompletedAt = nil
		}
		return nil
	})
}

func (s *Service) DeleteTodo(ctx context.Context, ref string) (TodoCommandResult, error) {
	return s.UpdateTodo(ctx, ref, func(todo *TodoRecord) error {
		now := s.now().UTC()
		todo.DeletedAt = &now
		return nil
	})
}

func (s *Service) Todo(ref string) (TodoRecord, error) {
	return s.store.ResolveTodo(ref)
}

func (s *Service) ListTodos(includeDeleted bool, completedFilter *bool, listRef string) ([]TodoRecord, error) {
	return s.ListTodosWithOptions(TodoListOptions{
		IncludeDeleted:  includeDeleted,
		CompletedFilter: completedFilter,
		ListRef:         listRef,
	})
}

func (s *Service) ListTodosWithOptions(opts TodoListOptions) ([]TodoRecord, error) {
	listUUID := ""
	if strings.TrimSpace(opts.ListRef) != "" {
		var err error
		listUUID, err = s.resolveOptionalListUUID(opts.ListRef)
		if err != nil {
			return nil, err
		}
	}
	parentUUID := ""
	if opts.ParentFilter {
		resolved, err := s.resolveOptionalTodoUUID(opts.ParentRef)
		if err != nil {
			return nil, fmt.Errorf("resolve parent: %w", err)
		}
		parentUUID = resolved
	}
	return s.store.ListTodosByQuery(TodoQuery{
		IncludeDeleted:  opts.IncludeDeleted,
		CompletedFilter: opts.CompletedFilter,
		ListUUID:        listUUID,
		ParentUUID:      parentUUID,
		ParentFilter:    opts.ParentFilter,
		RootOnly:        opts.RootOnly,
		GoalUUID:        strings.TrimSpace(opts.GoalUUID),
		MilestoneUUID:   strings.TrimSpace(opts.MilestoneUUID),
		DeadlineBefore:  opts.DeadlineBefore,
		DeadlineAfter:   opts.DeadlineAfter,
		Search:          strings.TrimSpace(opts.Search),
	})
}

func (s *Service) AddTodoList(ctx context.Context, name, color, icon string, order int) (TodoListCommandResult, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return TodoListCommandResult{}, fmt.Errorf("list name must be non-empty")
	}
	now := s.now().UTC()
	list := TodoListRecord{
		UUID:            newUUID(),
		Name:            name,
		Color:           strings.TrimSpace(color),
		Icon:            strings.TrimSpace(icon),
		SortOrder:       order,
		CreatedAt:       now,
		UpdatedAt:       now,
		ClientCreatedAt: now,
		ClientUpdatedAt: now,
	}
	if err := s.store.SaveTodoList(list); err != nil {
		return TodoListCommandResult{}, err
	}
	synced, detail := s.bestEffortSync(ctx)
	return TodoListCommandResult{List: list, Synced: synced, SyncDetail: detail}, nil
}

func (s *Service) UpdateTodoList(ctx context.Context, ref string, mutate func(*TodoListRecord) error) (TodoListCommandResult, error) {
	list, err := s.store.ResolveTodoList(ref)
	if err != nil {
		return TodoListCommandResult{}, err
	}
	if mutate != nil {
		if err := mutate(&list); err != nil {
			return TodoListCommandResult{}, err
		}
	}
	now := s.now().UTC()
	list.UpdatedAt = now
	list.ClientUpdatedAt = now
	list.SyncedAt = nil
	list.LastSyncError = ""
	if err := s.store.SaveTodoList(list); err != nil {
		return TodoListCommandResult{}, err
	}
	synced, detail := s.bestEffortSync(ctx)
	return TodoListCommandResult{List: list, Synced: synced, SyncDetail: detail}, nil
}

func (s *Service) DeleteTodoList(ctx context.Context, ref string) (TodoListCommandResult, error) {
	return s.UpdateTodoList(ctx, ref, func(list *TodoListRecord) error {
		now := s.now().UTC()
		list.DeletedAt = &now
		return nil
	})
}

func (s *Service) ListTodoLists(includeDeleted bool) ([]TodoListRecord, error) {
	return s.store.ListTodoLists(includeDeleted)
}

func (s *Service) TodoList(ref string) (TodoListRecord, error) {
	return s.store.ResolveTodoList(ref)
}

func (s *Service) AddTodoReply(ctx context.Context, todoRef, content, sourceName, actorDisplayName string) (TodoReplyCommandResult, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return TodoReplyCommandResult{}, fmt.Errorf("reply content must be non-empty")
	}
	todo, err := s.store.ResolveTodo(todoRef)
	if err != nil {
		return TodoReplyCommandResult{}, err
	}
	if todo.DeletedAt != nil {
		return TodoReplyCommandResult{}, fmt.Errorf("cannot reply to a deleted todo")
	}
	if todo.ArchivedAt != nil {
		return TodoReplyCommandResult{}, fmt.Errorf("cannot reply to an archived todo")
	}
	now := s.now().UTC()
	reply := TodoReplyRecord{
		UUID:             newUUID(),
		TodoUUID:         todo.UUID,
		Content:          content,
		SourceType:       "api_token",
		SourceName:       defaultString(sourceName, "life-cli"),
		ActorDisplayName: defaultString(actorDisplayName, "CLI"),
		CreatedAt:        now,
		UpdatedAt:        now,
		ClientCreatedAt:  now,
		ClientUpdatedAt:  now,
	}
	if err := s.store.SaveTodoReply(reply); err != nil {
		return TodoReplyCommandResult{}, err
	}
	synced, detail := s.bestEffortSync(ctx)
	return TodoReplyCommandResult{Reply: reply, Todo: todo, Synced: synced, SyncDetail: detail}, nil
}

func (s *Service) ListTodoReplies(todoRef string, includeDeleted bool) (TodoRecord, []TodoReplyRecord, error) {
	todo, err := s.store.ResolveTodo(todoRef)
	if err != nil {
		return TodoRecord{}, nil, err
	}
	replies, err := s.store.ListTodoReplies(todo.UUID, includeDeleted)
	if err != nil {
		return TodoRecord{}, nil, err
	}
	return todo, replies, nil
}

func (s *Service) resolveOptionalListUUID(ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", nil
	}
	list, err := s.store.ResolveTodoList(ref)
	if err != nil {
		return "", fmt.Errorf("resolve list: %w", err)
	}
	return list.UUID, nil
}

func (s *Service) resolveOptionalTodoUUID(ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", nil
	}
	todo, err := s.store.ResolveTodo(ref)
	if err != nil {
		return "", err
	}
	return todo.UUID, nil
}

func (s *Service) ResolveTodoUUID(ref string) (string, error) {
	return s.resolveOptionalTodoUUID(ref)
}

func (s *Service) ValidateTodoParent(todo TodoRecord) error {
	parentUUID := strings.TrimSpace(todo.ParentUUID)
	if parentUUID == "" {
		return nil
	}
	if parentUUID == todo.UUID {
		return fmt.Errorf("todo cannot be its own parent")
	}
	seen := map[string]bool{todo.UUID: true}
	for parentUUID != "" {
		if seen[parentUUID] {
			return fmt.Errorf("todo parent cycle detected")
		}
		seen[parentUUID] = true
		parent, err := s.store.Todo(parentUUID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		parentUUID = strings.TrimSpace(parent.ParentUUID)
	}
	return nil
}

func (s *Service) inheritParentTodoList(todo *TodoRecord) error {
	if strings.TrimSpace(todo.ParentUUID) == "" {
		return nil
	}
	if err := s.ValidateTodoParent(*todo); err != nil {
		return err
	}
	parent, err := s.store.Todo(todo.ParentUUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	todo.ListUUID = parent.ListUUID
	return nil
}

func (s *Service) cascadeTodoListToChildren(parent TodoRecord) error {
	children, err := s.store.ChildTodos(parent.UUID)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	for _, child := range children {
		if child.ListUUID != parent.ListUUID {
			child.ListUUID = parent.ListUUID
			child.UpdatedAt = now
			child.ClientUpdatedAt = now
			child.SyncedAt = nil
			child.LastSyncError = ""
			if err := s.store.SaveTodo(child); err != nil {
				return err
			}
		}
		if err := s.cascadeTodoListToChildren(child); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) LogAndMaybeSync(ctx context.Context, logType, content string) (Record, bool, string, error) {
	now := s.now().UTC()
	record := Record{
		UUID:      newUUID(),
		LogType:   logType,
		Content:   content,
		LoggedAt:  now,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.store.Insert(record); err != nil {
		return Record{}, false, "", err
	}

	if !s.client.CanSync() {
		return record, false, "INTEGLIFE_API_TOKEN not set", nil
	}

	result, err := s.SyncPending(ctx)
	if err != nil {
		return record, false, err.Error(), nil
	}
	if len(result.Failures) > 0 {
		return record, false, result.Failures[0].Detail, nil
	}

	syncedAt := s.now().UTC()
	record.SyncedAt = &syncedAt
	return record, true, fmt.Sprintf("synced=%d pending=%d", result.Synced, result.Pending), nil
}

func (s *Service) SyncPending(ctx context.Context) (SyncResult, error) {
	if !s.client.CanSync() {
		return SyncResult{}, fmt.Errorf("INTEGLIFE_API_TOKEN not set")
	}

	if err := s.store.RefreshNoteFiles(s.now()); err != nil {
		return SyncResult{}, err
	}
	cursors, err := s.store.SyncCursors()
	if err != nil {
		return SyncResult{}, err
	}
	notePullAck, err := s.client.SyncRecords(ctx, SyncBatch{
		Models:  []string{"notes"},
		Cursors: cursors,
	})
	if err != nil {
		return SyncResult{}, err
	}
	if err := s.applySyncAck(notePullAck); err != nil {
		return SyncResult{}, err
	}

	batch, err := s.store.PendingSyncBatch()
	if err != nil {
		return SyncResult{}, err
	}
	notePendingCount, err := s.store.PendingNoteCount()
	if err != nil {
		return SyncResult{}, err
	}
	pendingCount := notePendingCount + len(batch.StatusLogs) + len(batch.TodoLists) + len(batch.Todos) + len(batch.TodoReplies) + len(batch.AITaskRuns) + len(batch.AITaskEvents)
	result := SyncResult{Pending: pendingCount}
	if len(batch.Notes)+len(batch.StatusLogs)+len(batch.TodoLists)+len(batch.Todos)+len(batch.TodoReplies)+len(batch.AITaskRuns)+len(batch.AITaskEvents) == 0 {
		batch.Models = []string{"status_logs", "todo_lists", "todos", "todo_replies", "ai_task_runs", "ai_task_events"}
	}

	ack, err := s.client.SyncRecords(ctx, batch)
	if err != nil {
		if markErr := s.markBatchSyncError(batch, err.Error()); markErr != nil {
			return SyncResult{}, markErr
		}
		result.Failures = append(result.Failures, failuresForBatch(batch, err.Error())...)
		return result, nil
	}
	if err := s.applySyncAck(ack); err != nil {
		return SyncResult{}, err
	}

	result.Synced = len(notePullAck.NoteServerRows) + len(ack.NoteSynced) + len(ack.StatusLogUUIDs) + len(ack.TodoListSynced) + len(ack.TodoSynced) + len(ack.TodoReplySynced) +
		len(ack.TodoListServerRows) + len(ack.TodoServerRecords) + len(ack.TodoReplyServerRows) + len(ack.AITaskRunSynced) + len(ack.AITaskEventSynced)
	for _, uuid := range ack.TodoListConflicts {
		result.Failures = append(result.Failures, SyncFailure{UUID: uuid, Detail: "conflict: server newer"})
	}
	for _, uuid := range ack.TodoConflicts {
		result.Failures = append(result.Failures, SyncFailure{UUID: uuid, Detail: "conflict: server newer"})
	}
	for _, uuid := range ack.TodoReplyConflicts {
		result.Failures = append(result.Failures, SyncFailure{UUID: uuid, Detail: "conflict: server newer"})
	}
	for _, uuid := range ack.AITaskRunConflicts {
		result.Failures = append(result.Failures, SyncFailure{UUID: uuid, Detail: "conflict: server newer"})
	}
	for uuid, detail := range ack.AITaskEventErrors {
		result.Failures = append(result.Failures, SyncFailure{UUID: uuid, Detail: detail})
	}
	for uuid, detail := range ack.NoteErrors {
		result.Failures = append(result.Failures, SyncFailure{UUID: uuid, Detail: detail})
	}
	noteConflicts, err := s.store.ListNotes(true, true)
	if err != nil {
		return SyncResult{}, err
	}
	for _, note := range noteConflicts {
		result.Failures = append(result.Failures, SyncFailure{UUID: note.UUID, Detail: note.LastSyncError})
	}
	return result, nil
}

func (s *Service) StartAITask(ctx context.Context, opts AIStartOptions) (AICommandResult, error) {
	now := s.now().UTC()
	projectType, projectUUID, err := ParseProjectRef(opts.Project)
	if err != nil {
		return AICommandResult{}, err
	}
	agentName := defaultString(opts.AgentName, "codex")
	sessionID := ResolveAISessionID(opts.SessionID)
	cwd := currentWorkingDir()
	scopeKey := ScopeKey(cwd, projectType, projectUUID, opts.TodoUUID, agentName)

	if !opts.NewRun && !opts.SessionExplicit {
		running, err := s.store.RunningRunsForScope(scopeKey)
		if err != nil {
			return AICommandResult{}, err
		}
		if len(running) > 1 {
			return AICommandResult{}, fmt.Errorf("multiple running AI runs exist for this scope; pass --session <id> or --new")
		}
	}

	run := AITaskRunRecord{
		UUID:                newUUID(),
		ProjectType:         projectType,
		ProjectUUID:         projectUUID,
		TodoUUID:            opts.TodoUUID,
		AgentName:           agentName,
		Status:              "running",
		Title:               opts.Title,
		ContextSnapshotJSON: "{}",
		StartedAt:           &now,
		LastHeartbeatAt:     &now,
		CreatedAt:           now,
		UpdatedAt:           now,
		ClientCreatedAt:     now,
		ClientUpdatedAt:     now,
	}
	if err := s.store.SaveAITaskRun(run); err != nil {
		return AICommandResult{}, err
	}
	if err := s.store.SetActiveRun(ActiveRunRecord{
		ScopeKey:    scopeKey,
		SessionID:   sessionID,
		RunUUID:     run.UUID,
		CWD:         cwd,
		ProjectType: projectType,
		ProjectUUID: projectUUID,
		TodoUUID:    opts.TodoUUID,
		AgentName:   agentName,
		UpdatedAt:   now,
	}); err != nil {
		return AICommandResult{}, err
	}

	synced, detail := s.bestEffortSync(ctx)
	return AICommandResult{Run: run, Synced: synced, SyncDetail: detail}, nil
}

func (s *Service) ResumeAITask(ctx context.Context, opts AIResumeOptions) (AICommandResult, bool, error) {
	if s.client.CanSync() {
		_, _ = s.SyncPending(ctx)
	}
	projectType, projectUUID, err := ParseProjectRef(opts.Project)
	if err != nil {
		return AICommandResult{}, false, err
	}
	agentName := defaultString(opts.AgentName, "codex")
	sessionID := ResolveAISessionID(opts.SessionID)
	cwd := currentWorkingDir()
	scopeKey := ScopeKey(cwd, projectType, projectUUID, opts.TodoUUID, agentName)

	if opts.NewRun {
		started, err := s.StartAITask(ctx, AIStartOptions{
			Project: opts.Project, TodoUUID: opts.TodoUUID, Title: opts.Title, AgentName: agentName,
			SessionID: sessionID, SessionExplicit: true, NewRun: true,
		})
		return started, true, err
	}

	running, err := s.store.RunningRunsForScope(scopeKey)
	if err != nil {
		return AICommandResult{}, false, err
	}
	if !opts.SessionExplicit && len(running) > 1 {
		return AICommandResult{}, false, fmt.Errorf("multiple running AI runs exist for this scope; pass --session <id> or --new")
	}

	active, found, err := s.store.ActiveRun(scopeKey, sessionID)
	if err != nil {
		return AICommandResult{}, false, err
	}
	if found {
		run, err := s.store.GetAITaskRun(active.RunUUID)
		if err == nil && isResumableStatus(run.Status) {
			return AICommandResult{Run: run, Synced: run.SyncedAt != nil}, true, nil
		}
	}

	if !opts.SessionExplicit && len(running) > 0 {
		return AICommandResult{}, false, fmt.Errorf("running AI run exists for this scope in another session; pass --session <id> or --new")
	}

	run, ok, err := s.store.LatestRunningRun(projectType, projectUUID, opts.TodoUUID, agentName)
	if err != nil {
		return AICommandResult{}, false, err
	}
	if !ok {
		return AICommandResult{}, false, nil
	}
	if err := s.store.SetActiveRun(ActiveRunRecord{
		ScopeKey:    scopeKey,
		SessionID:   sessionID,
		RunUUID:     run.UUID,
		CWD:         cwd,
		ProjectType: projectType,
		ProjectUUID: projectUUID,
		TodoUUID:    opts.TodoUUID,
		AgentName:   agentName,
		UpdatedAt:   s.now().UTC(),
	}); err != nil {
		return AICommandResult{}, false, err
	}
	return AICommandResult{Run: run, Synced: run.SyncedAt != nil}, true, nil
}

func (s *Service) ProgressAITask(ctx context.Context, runUUID, phase, summary string) (AICommandResult, error) {
	if strings.TrimSpace(runUUID) == "" {
		return AICommandResult{}, fmt.Errorf("--run is required")
	}
	run, err := s.store.GetAITaskRun(runUUID)
	if err != nil {
		return AICommandResult{}, err
	}
	now := s.now().UTC()
	run.Status = "running"
	run.LatestPhase = phase
	run.LatestSummary = summary
	run.LastHeartbeatAt = &now
	run.UpdatedAt = now
	run.ClientUpdatedAt = now
	run.SyncedAt = nil
	run.LastSyncError = ""
	if err := s.store.SaveAITaskRun(run); err != nil {
		return AICommandResult{}, err
	}
	event, err := s.newAITaskEvent(run, AIEventOptions{
		EventType: "progress",
		Severity:  "info",
		Title:     phase,
		Content:   summary,
	})
	if err != nil {
		return AICommandResult{}, err
	}
	if err := s.store.SaveAITaskEvent(event); err != nil {
		return AICommandResult{}, err
	}
	synced, detail := s.bestEffortSync(ctx)
	return AICommandResult{Run: run, Event: &event, Synced: synced, SyncDetail: detail}, nil
}

func (s *Service) HeartbeatAITask(ctx context.Context, runUUID, summary string) (AICommandResult, error) {
	run, err := s.store.GetAITaskRun(runUUID)
	if err != nil {
		return AICommandResult{}, err
	}
	now := s.now().UTC()
	if summary != "" {
		run.LatestSummary = summary
	}
	run.LastHeartbeatAt = &now
	run.UpdatedAt = now
	run.ClientUpdatedAt = now
	run.SyncedAt = nil
	run.LastSyncError = ""
	if err := s.store.SaveAITaskRun(run); err != nil {
		return AICommandResult{}, err
	}
	synced, detail := s.bestEffortSync(ctx)
	return AICommandResult{Run: run, Synced: synced, SyncDetail: detail}, nil
}

func (s *Service) AddAITaskEvent(ctx context.Context, opts AIEventOptions) (AICommandResult, error) {
	run, err := s.store.GetAITaskRun(opts.RunUUID)
	if err != nil {
		return AICommandResult{}, err
	}
	event, err := s.newAITaskEvent(run, opts)
	if err != nil {
		return AICommandResult{}, err
	}
	if err := s.store.SaveAITaskEvent(event); err != nil {
		return AICommandResult{}, err
	}
	synced, detail := s.bestEffortSync(ctx)
	return AICommandResult{Run: run, Event: &event, Synced: synced, SyncDetail: detail}, nil
}

func (s *Service) BlockAITask(ctx context.Context, runUUID, question string) (AICommandResult, error) {
	run, err := s.store.GetAITaskRun(runUUID)
	if err != nil {
		return AICommandResult{}, err
	}
	now := s.now().UTC()
	run.Status = "blocked"
	run.LatestSummary = question
	run.LastHeartbeatAt = &now
	run.UpdatedAt = now
	run.ClientUpdatedAt = now
	run.SyncedAt = nil
	run.LastSyncError = ""
	if err := s.store.SaveAITaskRun(run); err != nil {
		return AICommandResult{}, err
	}
	event, err := s.newAITaskEvent(run, AIEventOptions{EventType: "blocker", Severity: "warning", Title: "Blocked", Content: question})
	if err != nil {
		return AICommandResult{}, err
	}
	if err := s.store.SaveAITaskEvent(event); err != nil {
		return AICommandResult{}, err
	}
	synced, detail := s.bestEffortSync(ctx)
	return AICommandResult{Run: run, Event: &event, Synced: synced, SyncDetail: detail}, nil
}

func (s *Service) CompleteAITask(ctx context.Context, runUUID, summary, artifact string) (AICommandResult, error) {
	run, err := s.store.GetAITaskRun(runUUID)
	if err != nil {
		return AICommandResult{}, err
	}
	now := s.now().UTC()
	run.Status = "completed"
	run.LatestSummary = summary
	run.LastHeartbeatAt = &now
	run.CompletedAt = &now
	run.UpdatedAt = now
	run.ClientUpdatedAt = now
	run.SyncedAt = nil
	run.LastSyncError = ""
	if err := s.store.SaveAITaskRun(run); err != nil {
		return AICommandResult{}, err
	}
	metadata := "{}"
	if strings.TrimSpace(artifact) != "" {
		data, _ := json.Marshal(map[string]any{"artifact_paths": []string{artifact}})
		metadata = string(data)
	}
	event, err := s.newAITaskEvent(run, AIEventOptions{
		EventType: "final", Severity: "info", Title: "Completed", Content: summary, MetadataJSON: metadata,
	})
	if err != nil {
		return AICommandResult{}, err
	}
	if err := s.store.SaveAITaskEvent(event); err != nil {
		return AICommandResult{}, err
	}
	if err := s.store.ClearActiveRunByRunUUID(run.UUID); err != nil {
		return AICommandResult{}, err
	}
	synced, detail := s.bestEffortSync(ctx)
	return AICommandResult{Run: run, Event: &event, Synced: synced, SyncDetail: detail}, nil
}

func (s *Service) AITaskStatus(runUUID string) (AITaskRunRecord, error) {
	return s.store.GetAITaskRun(runUUID)
}

func (s *Service) CurrentAITask(project, todoUUID, agentName, sessionID string) (AITaskRunRecord, bool, error) {
	projectType, projectUUID, err := ParseProjectRef(project)
	if err != nil {
		return AITaskRunRecord{}, false, err
	}
	scopeKey := ScopeKey(currentWorkingDir(), projectType, projectUUID, todoUUID, defaultString(agentName, "codex"))
	active, found, err := s.store.ActiveRun(scopeKey, ResolveAISessionID(sessionID))
	if err != nil || !found {
		return AITaskRunRecord{}, false, err
	}
	run, err := s.store.GetAITaskRun(active.RunUUID)
	if err != nil {
		return AITaskRunRecord{}, false, err
	}
	return run, true, nil
}

func (s *Service) newAITaskEvent(run AITaskRunRecord, opts AIEventOptions) (AITaskEventRecord, error) {
	now := s.now().UTC()
	eventType := defaultString(opts.EventType, "progress")
	if !validAITaskEventType(eventType) {
		return AITaskEventRecord{}, fmt.Errorf("invalid event type %q", eventType)
	}
	event := AITaskEventRecord{
		UUID:               newUUID(),
		RunUUID:            run.UUID,
		TodoUUID:           run.TodoUUID,
		EventType:          eventType,
		Severity:           defaultString(opts.Severity, "info"),
		Title:              opts.Title,
		Content:            opts.Content,
		MetadataJSON:       opts.MetadataJSON,
		PayloadHashVersion: 1,
		OccurredAt:         now,
		CreatedAt:          now,
		UpdatedAt:          now,
		ClientCreatedAt:    now,
		ClientUpdatedAt:    now,
	}
	hash, metadata, err := ComputeAITaskEventPayloadHash(event)
	if err != nil {
		return AITaskEventRecord{}, err
	}
	event.MetadataJSON = metadata
	event.PayloadHash = hash
	return event, nil
}

func (s *Service) bestEffortSync(ctx context.Context) (bool, string) {
	if !s.client.CanSync() {
		return false, "INTEGLIFE_API_TOKEN not set"
	}
	result, err := s.SyncPending(ctx)
	if err != nil {
		return false, err.Error()
	}
	if len(result.Failures) > 0 {
		return false, result.Failures[0].Detail
	}
	return true, fmt.Sprintf("synced=%d pending=%d", result.Synced, result.Pending)
}

func (s *Service) applySyncAck(ack SyncAck) error {
	for _, uuid := range ack.NoteSynced {
		var err error
		if etag, accepted := ack.NoteSyncETags[uuid]; accepted {
			err = s.store.MarkNoteSynced(uuid, ack.ServerTime, NoteRecord{SyncETag: etag, SyncBasePath: ack.NoteSyncBasePaths[uuid]})
		} else {
			err = s.store.MarkNoteSynced(uuid, ack.ServerTime)
		}
		if err != nil {
			return err
		}
	}
	for _, note := range ack.NoteConflicts {
		copyRef := ack.NoteConflictCopies[note.UUID]
		attempt := ack.NoteConflictAttempts[note.UUID]
		if err := s.store.ApplyNoteConflict(note.UUID, note, copyRef, attempt); err != nil {
			return err
		}
	}
	for uuid, detail := range ack.NoteErrors {
		if err := s.store.MarkNoteSyncError(uuid, detail); err != nil {
			return err
		}
	}
	// A path released by a deletion can be reused by another UUID in this response.
	for _, deleted := range []bool{true, false} {
		for _, note := range ack.NoteServerRows {
			if (note.DeletedAt != nil) != deleted {
				continue
			}
			if _, err := s.store.ApplyRemoteNote(note, ack.ServerTime); err != nil {
				return err
			}
		}
	}
	for _, uuid := range ack.StatusLogUUIDs {
		if err := s.store.MarkSynced(uuid, ack.ServerTime); err != nil {
			return err
		}
	}
	for _, uuid := range ack.TodoListSynced {
		if err := s.store.MarkTodoListSynced(uuid, ack.ServerTime); err != nil {
			return err
		}
	}
	for _, uuid := range ack.TodoListConflicts {
		if err := s.store.MarkTodoListSyncError(uuid, "conflict: server newer"); err != nil {
			return err
		}
	}
	for _, list := range ack.TodoListServerRows {
		if err := s.store.SaveTodoList(list); err != nil {
			return err
		}
	}
	for _, uuid := range ack.TodoSynced {
		if err := s.store.MarkTodoSynced(uuid, ack.ServerTime); err != nil {
			return err
		}
	}
	for _, uuid := range ack.TodoConflicts {
		if err := s.store.MarkTodoSyncError(uuid, "conflict: server newer"); err != nil {
			return err
		}
	}
	for _, todo := range ack.TodoServerRecords {
		if err := s.store.SaveTodo(todo); err != nil {
			return err
		}
	}
	for _, uuid := range ack.TodoReplySynced {
		if err := s.store.MarkTodoReplySynced(uuid, ack.ServerTime); err != nil {
			return err
		}
	}
	for _, uuid := range ack.TodoReplyConflicts {
		if err := s.store.MarkTodoReplySyncError(uuid, "conflict: server newer"); err != nil {
			return err
		}
	}
	for _, reply := range ack.TodoReplyServerRows {
		if err := s.store.SaveTodoReply(reply); err != nil {
			return err
		}
	}
	for _, uuid := range ack.AITaskRunSynced {
		if err := s.store.MarkAITaskRunSynced(uuid, ack.ServerTime); err != nil {
			return err
		}
	}
	for _, uuid := range ack.AITaskRunConflicts {
		if err := s.store.MarkAITaskRunSyncError(uuid, "conflict: server newer"); err != nil {
			return err
		}
	}
	for _, uuid := range ack.AITaskEventSynced {
		if err := s.store.MarkAITaskEventSynced(uuid, ack.ServerTime); err != nil {
			return err
		}
	}
	for uuid, detail := range ack.AITaskEventErrors {
		if err := s.store.MarkAITaskEventSyncError(uuid, detail); err != nil {
			return err
		}
	}
	cursorModels := []string{}
	for _, model := range ack.Models {
		if model != "status_logs" {
			cursorModels = append(cursorModels, model)
		}
	}
	if err := s.store.UpdateSyncCursors(cursorModels, ack.ServerTime); err != nil {
		return err
	}
	return nil
}

func (s *Service) markBatchSyncError(batch SyncBatch, detail string) error {
	for _, note := range batch.Notes {
		if err := s.store.MarkNoteSyncError(note.UUID, detail); err != nil {
			return err
		}
	}
	for _, record := range batch.StatusLogs {
		if err := s.store.MarkSyncError(record.UUID, detail); err != nil {
			return err
		}
	}
	for _, list := range batch.TodoLists {
		if err := s.store.MarkTodoListSyncError(list.UUID, detail); err != nil {
			return err
		}
	}
	for _, todo := range batch.Todos {
		if err := s.store.MarkTodoSyncError(todo.UUID, detail); err != nil {
			return err
		}
	}
	for _, reply := range batch.TodoReplies {
		if err := s.store.MarkTodoReplySyncError(reply.UUID, detail); err != nil {
			return err
		}
	}
	for _, run := range batch.AITaskRuns {
		if err := s.store.MarkAITaskRunSyncError(run.UUID, detail); err != nil {
			return err
		}
	}
	for _, event := range batch.AITaskEvents {
		if err := s.store.MarkAITaskEventSyncError(event.UUID, detail); err != nil {
			return err
		}
	}
	return nil
}

func failuresForBatch(batch SyncBatch, detail string) []SyncFailure {
	failures := []SyncFailure{}
	for _, note := range batch.Notes {
		failures = append(failures, SyncFailure{UUID: note.UUID, Detail: detail})
	}
	for _, record := range batch.StatusLogs {
		failures = append(failures, SyncFailure{UUID: record.UUID, Detail: detail})
	}
	for _, list := range batch.TodoLists {
		failures = append(failures, SyncFailure{UUID: list.UUID, Detail: detail})
	}
	for _, todo := range batch.Todos {
		failures = append(failures, SyncFailure{UUID: todo.UUID, Detail: detail})
	}
	for _, reply := range batch.TodoReplies {
		failures = append(failures, SyncFailure{UUID: reply.UUID, Detail: detail})
	}
	for _, run := range batch.AITaskRuns {
		failures = append(failures, SyncFailure{UUID: run.UUID, Detail: detail})
	}
	for _, event := range batch.AITaskEvents {
		failures = append(failures, SyncFailure{UUID: event.UUID, Detail: detail})
	}
	return failures
}

func newUUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("statuslog-%d", time.Now().UnixNano())
	}

	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80

	return fmt.Sprintf(
		"%s-%s-%s-%s-%s",
		hex.EncodeToString(buf[0:4]),
		hex.EncodeToString(buf[4:6]),
		hex.EncodeToString(buf[6:8]),
		hex.EncodeToString(buf[8:10]),
		hex.EncodeToString(buf[10:16]),
	)
}

func ParseProjectRef(ref string) (string, string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "adhoc", "", nil
	}
	kind, uuid, ok := strings.Cut(ref, ":")
	if !ok {
		return "", "", fmt.Errorf("--project must use type:uuid, for example goal:<uuid>")
	}
	switch kind {
	case "goal", "todo_list", "todo", "adhoc":
	default:
		return "", "", fmt.Errorf("unsupported project type %q", kind)
	}
	if kind != "adhoc" && strings.TrimSpace(uuid) == "" {
		return "", "", fmt.Errorf("--project %s requires a uuid", kind)
	}
	return kind, uuid, nil
}

func ResolveAISessionID(flagValue string) string {
	if strings.TrimSpace(flagValue) != "" {
		return strings.TrimSpace(flagValue)
	}
	if env := strings.TrimSpace(os.Getenv("LIFE_AI_SESSION_ID")); env != "" {
		return env
	}
	return "default"
}

func ScopeKey(cwd, projectType, projectUUID, todoUUID, agentName string) string {
	parts := []string{
		filepath.Clean(cwd),
		projectType,
		projectUUID,
		todoUUID,
		agentName,
	}
	return strings.Join(parts, "\x1f")
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func currentWorkingDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

func isResumableStatus(status string) bool {
	return status == "queued" || status == "running" || status == "blocked"
}

func validAITaskEventType(eventType string) bool {
	switch eventType {
	case "progress", "phase", "command", "artifact", "decision", "blocker", "error", "final":
		return true
	default:
		return false
	}
}
