package integlife

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db       *sql.DB
	notesDir string
}

const (
	currentSchemaVersion = 6
	sqliteBusyTimeoutMS  = 5000
)

type TodoQuery struct {
	IncludeDeleted  bool
	CompletedFilter *bool
	ListUUID        string
	ParentUUID      string
	ParentFilter    bool
	RootOnly        bool
	GoalUUID        string
	MilestoneUUID   string
	DeadlineBefore  *time.Time
	DeadlineAfter   *time.Time
	Search          string
}

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{db: db, notesDir: filepath.Join(filepath.Dir(path), "notes")}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func sqliteDSN(path string) string {
	if path == ":memory:" {
		path = "file::memory:"
	}
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + "_pragma=" + url.QueryEscape(fmt.Sprintf("busy_timeout(%d)", sqliteBusyTimeoutMS))
}

func (s *Store) init() error {
	deadline := time.Now().Add(time.Duration(sqliteBusyTimeoutMS) * time.Millisecond)
	delay := 25 * time.Millisecond
	for {
		ready, err := s.schemaReady()
		if err == nil && ready {
			return nil
		}
		if err != nil && !isSQLiteBusy(err) {
			return fmt.Errorf("check schema readiness: %w", err)
		}

		if err == nil {
			err = s.migrateOnce()
		} else {
			err = fmt.Errorf("check schema readiness: %w", err)
		}
		if err == nil {
			return nil
		}
		if !isSQLiteBusy(err) {
			return err
		}
		if time.Now().Add(delay).After(deadline) {
			return err
		}
		time.Sleep(delay)
		delay *= 2
		if delay > 250*time.Millisecond {
			delay = 250 * time.Millisecond
		}
	}
}

func (s *Store) migrateOnce() error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer tx.Rollback()

	if err := migrateSchema(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	return nil
}

func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "sqlite_busy") ||
		strings.Contains(text, "database is locked") ||
		strings.Contains(text, "database table is locked")
}

func (s *Store) schemaReady() (bool, error) {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return false, fmt.Errorf("read schema version: %w", err)
	}
	if version != currentSchemaVersion {
		return false, nil
	}

	for _, table := range []string{
		"status_logs",
		"notes",
		"todo_lists",
		"todos",
		"todo_replies",
		"ai_task_runs",
		"ai_task_events",
		"active_ai_runs",
		"sync_cursors",
	} {
		exists, err := s.hasSQLiteObject("table", table)
		if err != nil {
			return false, err
		}
		if !exists {
			return false, nil
		}
	}

	for _, index := range []string{
		"idx_notes_pending",
		"idx_notes_active_path",
		"idx_todo_lists_active_order",
		"idx_todo_lists_pending",
		"idx_todos_active_order",
		"idx_todos_list",
		"idx_todos_pending",
		"idx_todo_replies_todo_created",
		"idx_todo_replies_pending",
		"idx_ai_task_runs_scope",
		"idx_ai_task_runs_pending",
		"idx_ai_task_events_run_pending",
		"idx_active_ai_runs_run",
	} {
		exists, err := s.hasSQLiteObject("index", index)
		if err != nil {
			return false, err
		}
		if !exists {
			return false, nil
		}
	}

	hasLastSyncError, err := tableHasColumnDB(s.db, "status_logs", "last_sync_error")
	if err != nil {
		return false, err
	}
	if !hasLastSyncError {
		return false, nil
	}
	for _, column := range []string{"remote_path", "sync_etag", "sync_base_path", "remote_sync_etag", "remote_sync_base_path", "conflict_copy_uuid", "conflict_copy_path", "conflict_attempt_json"} {
		found, err := tableHasColumnDB(s.db, "notes", column)
		if err != nil || !found {
			return false, err
		}
	}

	for _, model := range []string{"ai_task_runs", "ai_task_events"} {
		var found int
		err := s.db.QueryRow(`SELECT 1 FROM sync_cursors WHERE model = ?`, model).Scan(&found)
		if err == sql.ErrNoRows {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("check sync cursor %s: %w", model, err)
		}
	}
	return true, nil
}

func (s *Store) hasSQLiteObject(objectType, name string) (bool, error) {
	var found string
	err := s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = ? AND name = ?`,
		objectType,
		name,
	).Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check sqlite %s %s: %w", objectType, name, err)
	}
	return true, nil
}

func migrateSchema(tx *sql.Tx) error {
	var version int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS status_logs (
			uuid TEXT PRIMARY KEY,
			log_type TEXT NOT NULL,
			content TEXT NOT NULL,
			logged_at TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			synced_at TEXT,
			last_sync_error TEXT NOT NULL DEFAULT ''
		)
	`); err != nil {
		return fmt.Errorf("create status_logs: %w", err)
	}
	if err := addColumnIfMissing(tx, "status_logs", "last_sync_error", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}

	if version < 2 {
		if err := createAISchema(tx); err != nil {
			return err
		}
		if err := createNoteSchema(tx); err != nil {
			return err
		}
		if err := createTodoSchema(tx); err != nil {
			return err
		}
		if err := initSyncCursors(tx, time.Now().UTC()); err != nil {
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, currentSchemaVersion)); err != nil {
			return fmt.Errorf("set schema version: %w", err)
		}
		return nil
	}

	if err := createAISchema(tx); err != nil {
		return err
	}
	if err := createNoteSchema(tx); err != nil {
		return err
	}
	if err := createTodoSchema(tx); err != nil {
		return err
	}
	if err := initSyncCursors(tx, time.Now().UTC()); err != nil {
		return err
	}
	if version < currentSchemaVersion {
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, currentSchemaVersion)); err != nil {
			return fmt.Errorf("set schema version: %w", err)
		}
	}
	return nil
}

func (s *Store) Insert(record Record) error {
	_, err := s.db.Exec(
		`INSERT INTO status_logs (uuid, log_type, content, logged_at, created_at, updated_at, synced_at, last_sync_error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		record.UUID,
		record.LogType,
		record.Content,
		record.LoggedAt.UTC().Format(time.RFC3339Nano),
		record.CreatedAt.UTC().Format(time.RFC3339Nano),
		record.UpdatedAt.UTC().Format(time.RFC3339Nano),
		formatNullTime(record.SyncedAt),
		record.LastSyncError,
	)
	if err != nil {
		return fmt.Errorf("insert record: %w", err)
	}
	return nil
}

func (s *Store) MarkSynced(uuid string, syncedAt time.Time) error {
	_, err := s.db.Exec(
		`UPDATE status_logs SET synced_at = ?, last_sync_error = '' WHERE uuid = ?`,
		syncedAt.UTC().Format(time.RFC3339Nano),
		uuid,
	)
	if err != nil {
		return fmt.Errorf("mark synced: %w", err)
	}
	return nil
}

func (s *Store) MarkSyncError(uuid, detail string) error {
	_, err := s.db.Exec(`UPDATE status_logs SET last_sync_error = ? WHERE uuid = ?`, detail, uuid)
	if err != nil {
		return fmt.Errorf("mark sync error: %w", err)
	}
	return nil
}

func (s *Store) Pending() ([]Record, error) {
	rows, err := s.db.Query(`
		SELECT uuid, log_type, content, logged_at, created_at, updated_at, synced_at, last_sync_error
		FROM status_logs
		WHERE synced_at IS NULL
		ORDER BY logged_at ASC, created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query pending records: %w", err)
	}
	defer rows.Close()

	var records []Record
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending records: %w", err)
	}
	return records, nil
}

func (s *Store) PendingSyncBatch() (SyncBatch, error) {
	notes, err := s.PendingNotes()
	if err != nil {
		return SyncBatch{}, err
	}
	statusLogs, err := s.Pending()
	if err != nil {
		return SyncBatch{}, err
	}
	runs, err := s.PendingAITaskRuns()
	if err != nil {
		return SyncBatch{}, err
	}
	events, err := s.PendingAITaskEvents()
	if err != nil {
		return SyncBatch{}, err
	}
	todoLists, err := s.PendingTodoLists()
	if err != nil {
		return SyncBatch{}, err
	}
	todos, err := s.PendingTodos()
	if err != nil {
		return SyncBatch{}, err
	}
	replies, err := s.PendingTodoReplies()
	if err != nil {
		return SyncBatch{}, err
	}
	cursors, err := s.SyncCursors()
	if err != nil {
		return SyncBatch{}, err
	}
	return SyncBatch{
		Notes:        notes,
		StatusLogs:   statusLogs,
		TodoLists:    todoLists,
		Todos:        todos,
		TodoReplies:  replies,
		AITaskRuns:   runs,
		AITaskEvents: events,
		Cursors:      cursors,
	}, nil
}

func (s *Store) SaveTodo(todo TodoRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO todos (
			uuid, parent_uuid, content, notes, completed, sort_order, list_uuid,
			completed_at, deleted_at, archived_at, deadline, goal_uuid, milestone_uuid,
			category_uuid, task_role, todo_source, completion_mode, completion_source,
			ai_evaluation_status, ai_completion_summary, created_at, updated_at,
			client_created_at, client_updated_at, synced_at, last_sync_error
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(uuid) DO UPDATE SET
			parent_uuid = excluded.parent_uuid,
			content = excluded.content,
			notes = excluded.notes,
			completed = excluded.completed,
			sort_order = excluded.sort_order,
			list_uuid = excluded.list_uuid,
			completed_at = excluded.completed_at,
			deleted_at = excluded.deleted_at,
			archived_at = excluded.archived_at,
			deadline = excluded.deadline,
			goal_uuid = excluded.goal_uuid,
			milestone_uuid = excluded.milestone_uuid,
			category_uuid = excluded.category_uuid,
			task_role = excluded.task_role,
			todo_source = excluded.todo_source,
			completion_mode = excluded.completion_mode,
			completion_source = excluded.completion_source,
			ai_evaluation_status = excluded.ai_evaluation_status,
			ai_completion_summary = excluded.ai_completion_summary,
			updated_at = excluded.updated_at,
			client_updated_at = excluded.client_updated_at,
			synced_at = excluded.synced_at,
			last_sync_error = excluded.last_sync_error
	`, todo.UUID, todo.ParentUUID, todo.Content, todo.Notes, todo.Completed, todo.SortOrder, todo.ListUUID,
		formatNullTime(todo.CompletedAt), formatNullTime(todo.DeletedAt), formatNullTime(todo.ArchivedAt), formatNullTime(todo.Deadline),
		todo.GoalUUID, todo.MilestoneUUID, todo.CategoryUUID, todo.TaskRole, todo.TodoSource,
		defaultString(todo.CompletionMode, "manual"), defaultString(todo.CompletionSource, "manual"),
		defaultString(todo.AIEvaluationStatus, "not_requested"), todo.AICompletionSummary,
		formatTime(todo.CreatedAt), formatTime(todo.UpdatedAt), formatTime(todo.ClientCreatedAt), formatTime(todo.ClientUpdatedAt),
		formatNullTime(todo.SyncedAt), todo.LastSyncError)
	if err != nil {
		return fmt.Errorf("save todo: %w", err)
	}
	return nil
}

func (s *Store) Todo(uuid string) (TodoRecord, error) {
	row := s.db.QueryRow(todoSelectSQL()+` WHERE uuid = ?`, uuid)
	return scanTodo(row)
}

func (s *Store) ResolveTodo(ref string) (TodoRecord, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return TodoRecord{}, fmt.Errorf("todo ref must be non-empty")
	}
	rows, err := s.db.Query(todoSelectSQL()+`
		WHERE deleted_at IS NULL AND (uuid = ? OR uuid LIKE ?)
		ORDER BY CASE WHEN uuid = ? THEN 0 ELSE 1 END, client_updated_at DESC
		LIMIT 2
	`, ref, ref+"%", ref)
	if err != nil {
		return TodoRecord{}, fmt.Errorf("resolve todo: %w", err)
	}
	defer rows.Close()
	var matches []TodoRecord
	for rows.Next() {
		todo, err := scanTodo(rows)
		if err != nil {
			return TodoRecord{}, err
		}
		matches = append(matches, todo)
	}
	if err := rows.Err(); err != nil {
		return TodoRecord{}, fmt.Errorf("iterate todo matches: %w", err)
	}
	if len(matches) == 0 {
		return TodoRecord{}, sql.ErrNoRows
	}
	if len(matches) > 1 && matches[0].UUID != ref {
		return TodoRecord{}, fmt.Errorf("todo ref %q is ambiguous", ref)
	}
	return matches[0], nil
}

func (s *Store) ListTodos(includeDeleted bool, completedFilter *bool, listUUID string) ([]TodoRecord, error) {
	return s.ListTodosByQuery(TodoQuery{
		IncludeDeleted:  includeDeleted,
		CompletedFilter: completedFilter,
		ListUUID:        listUUID,
	})
}

func (s *Store) ListTodosByQuery(filters TodoQuery) ([]TodoRecord, error) {
	if filters.ParentFilter && filters.RootOnly {
		return nil, fmt.Errorf("parent filter and root filter cannot both be set")
	}
	query := todoSelectSQL() + ` WHERE 1=1`
	args := []any{}
	if !filters.IncludeDeleted {
		query += ` AND deleted_at IS NULL AND archived_at IS NULL`
	}
	if filters.CompletedFilter != nil {
		query += ` AND completed = ?`
		args = append(args, *filters.CompletedFilter)
	}
	if filters.ListUUID != "" {
		query += ` AND list_uuid = ?`
		args = append(args, filters.ListUUID)
	}
	if filters.ParentFilter {
		query += ` AND parent_uuid = ?`
		args = append(args, filters.ParentUUID)
	}
	if filters.RootOnly {
		query += ` AND parent_uuid = ''`
	}
	if filters.GoalUUID != "" {
		query += ` AND goal_uuid = ?`
		args = append(args, filters.GoalUUID)
	}
	if filters.MilestoneUUID != "" {
		query += ` AND milestone_uuid = ?`
		args = append(args, filters.MilestoneUUID)
	}
	if filters.DeadlineBefore != nil {
		query += ` AND deadline IS NOT NULL AND deadline < ?`
		args = append(args, formatTime(*filters.DeadlineBefore))
	}
	if filters.DeadlineAfter != nil {
		query += ` AND deadline IS NOT NULL AND deadline >= ?`
		args = append(args, formatTime(*filters.DeadlineAfter))
	}
	if strings.TrimSpace(filters.Search) != "" {
		query += ` AND (content LIKE ? OR notes LIKE ?)`
		term := "%" + strings.TrimSpace(filters.Search) + "%"
		args = append(args, term, term)
	}
	query += ` ORDER BY completed ASC, sort_order ASC, client_created_at ASC`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list todos: %w", err)
	}
	defer rows.Close()
	var todos []TodoRecord
	for rows.Next() {
		todo, err := scanTodo(rows)
		if err != nil {
			return nil, err
		}
		todos = append(todos, todo)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate todos: %w", err)
	}
	return todos, nil
}

func (s *Store) ChildTodos(parentUUID string) ([]TodoRecord, error) {
	rows, err := s.db.Query(todoSelectSQL()+`
		WHERE deleted_at IS NULL AND parent_uuid = ?
		ORDER BY completed ASC, sort_order ASC, client_created_at ASC
	`, parentUUID)
	if err != nil {
		return nil, fmt.Errorf("query child todos: %w", err)
	}
	defer rows.Close()
	var todos []TodoRecord
	for rows.Next() {
		todo, err := scanTodo(rows)
		if err != nil {
			return nil, err
		}
		todos = append(todos, todo)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate child todos: %w", err)
	}
	return todos, nil
}

func (s *Store) PendingTodos() ([]TodoRecord, error) {
	rows, err := s.db.Query(todoSelectSQL() + `
		WHERE synced_at IS NULL
		ORDER BY client_updated_at ASC, client_created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query pending todos: %w", err)
	}
	defer rows.Close()
	var todos []TodoRecord
	for rows.Next() {
		todo, err := scanTodo(rows)
		if err != nil {
			return nil, err
		}
		todos = append(todos, todo)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending todos: %w", err)
	}
	return todos, nil
}

func (s *Store) MarkTodoSynced(uuid string, syncedAt time.Time) error {
	_, err := s.db.Exec(`UPDATE todos SET synced_at = ?, last_sync_error = '' WHERE uuid = ?`, formatTime(syncedAt), uuid)
	if err != nil {
		return fmt.Errorf("mark todo synced: %w", err)
	}
	return nil
}

func (s *Store) MarkTodoSyncError(uuid, detail string) error {
	_, err := s.db.Exec(`UPDATE todos SET last_sync_error = ? WHERE uuid = ?`, detail, uuid)
	if err != nil {
		return fmt.Errorf("mark todo sync error: %w", err)
	}
	return nil
}

func (s *Store) SaveTodoList(list TodoListRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO todo_lists (
			uuid, name, color, icon, sort_order, deleted_at, created_at, updated_at,
			client_created_at, client_updated_at, synced_at, last_sync_error
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(uuid) DO UPDATE SET
			name = excluded.name,
			color = excluded.color,
			icon = excluded.icon,
			sort_order = excluded.sort_order,
			deleted_at = excluded.deleted_at,
			updated_at = excluded.updated_at,
			client_updated_at = excluded.client_updated_at,
			synced_at = excluded.synced_at,
			last_sync_error = excluded.last_sync_error
	`, list.UUID, list.Name, list.Color, list.Icon, list.SortOrder, formatNullTime(list.DeletedAt),
		formatTime(list.CreatedAt), formatTime(list.UpdatedAt), formatTime(list.ClientCreatedAt), formatTime(list.ClientUpdatedAt),
		formatNullTime(list.SyncedAt), list.LastSyncError)
	if err != nil {
		return fmt.Errorf("save todo list: %w", err)
	}
	return nil
}

func (s *Store) ResolveTodoList(ref string) (TodoListRecord, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return TodoListRecord{}, fmt.Errorf("list ref must be non-empty")
	}
	rows, err := s.db.Query(todoListSelectSQL()+`
		WHERE deleted_at IS NULL AND (uuid = ? OR uuid LIKE ? OR name = ?)
		ORDER BY CASE WHEN uuid = ? THEN 0 WHEN name = ? THEN 1 ELSE 2 END, client_updated_at DESC
		LIMIT 2
	`, ref, ref+"%", ref, ref, ref)
	if err != nil {
		return TodoListRecord{}, fmt.Errorf("resolve todo list: %w", err)
	}
	defer rows.Close()
	var matches []TodoListRecord
	for rows.Next() {
		list, err := scanTodoList(rows)
		if err != nil {
			return TodoListRecord{}, err
		}
		matches = append(matches, list)
	}
	if err := rows.Err(); err != nil {
		return TodoListRecord{}, fmt.Errorf("iterate todo list matches: %w", err)
	}
	if len(matches) == 0 {
		return TodoListRecord{}, sql.ErrNoRows
	}
	if len(matches) > 1 && matches[0].UUID != ref && matches[0].Name != ref {
		return TodoListRecord{}, fmt.Errorf("list ref %q is ambiguous", ref)
	}
	return matches[0], nil
}

func (s *Store) TodoList(uuid string) (TodoListRecord, error) {
	row := s.db.QueryRow(todoListSelectSQL()+` WHERE uuid = ?`, uuid)
	return scanTodoList(row)
}

func (s *Store) ListTodoLists(includeDeleted bool) ([]TodoListRecord, error) {
	query := todoListSelectSQL() + ` WHERE 1=1`
	if !includeDeleted {
		query += ` AND deleted_at IS NULL`
	}
	query += ` ORDER BY sort_order ASC, client_created_at ASC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("list todo lists: %w", err)
	}
	defer rows.Close()
	var lists []TodoListRecord
	for rows.Next() {
		list, err := scanTodoList(rows)
		if err != nil {
			return nil, err
		}
		lists = append(lists, list)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate todo lists: %w", err)
	}
	return lists, nil
}

func (s *Store) PendingTodoLists() ([]TodoListRecord, error) {
	rows, err := s.db.Query(todoListSelectSQL() + `
		WHERE synced_at IS NULL
		ORDER BY client_updated_at ASC, client_created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query pending todo lists: %w", err)
	}
	defer rows.Close()
	var lists []TodoListRecord
	for rows.Next() {
		list, err := scanTodoList(rows)
		if err != nil {
			return nil, err
		}
		lists = append(lists, list)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending todo lists: %w", err)
	}
	return lists, nil
}

func (s *Store) MarkTodoListSynced(uuid string, syncedAt time.Time) error {
	_, err := s.db.Exec(`UPDATE todo_lists SET synced_at = ?, last_sync_error = '' WHERE uuid = ?`, formatTime(syncedAt), uuid)
	if err != nil {
		return fmt.Errorf("mark todo list synced: %w", err)
	}
	return nil
}

func (s *Store) MarkTodoListSyncError(uuid, detail string) error {
	_, err := s.db.Exec(`UPDATE todo_lists SET last_sync_error = ? WHERE uuid = ?`, detail, uuid)
	if err != nil {
		return fmt.Errorf("mark todo list sync error: %w", err)
	}
	return nil
}

func (s *Store) SaveTodoReply(reply TodoReplyRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO todo_replies (
			uuid, todo_uuid, content, deleted_at, source_type, source_name,
			actor_display_name, created_at, updated_at, client_created_at,
			client_updated_at, synced_at, last_sync_error
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(uuid) DO UPDATE SET
			todo_uuid = excluded.todo_uuid,
			content = excluded.content,
			deleted_at = excluded.deleted_at,
			source_type = excluded.source_type,
			source_name = excluded.source_name,
			actor_display_name = excluded.actor_display_name,
			updated_at = excluded.updated_at,
			client_updated_at = excluded.client_updated_at,
			synced_at = excluded.synced_at,
			last_sync_error = excluded.last_sync_error
	`, reply.UUID, reply.TodoUUID, reply.Content, formatNullTime(reply.DeletedAt), reply.SourceType, reply.SourceName,
		reply.ActorDisplayName, formatTime(reply.CreatedAt), formatTime(reply.UpdatedAt), formatTime(reply.ClientCreatedAt),
		formatTime(reply.ClientUpdatedAt), formatNullTime(reply.SyncedAt), reply.LastSyncError)
	if err != nil {
		return fmt.Errorf("save todo reply: %w", err)
	}
	return nil
}

func (s *Store) ListTodoReplies(todoUUID string, includeDeleted bool) ([]TodoReplyRecord, error) {
	query := todoReplySelectSQL() + ` WHERE todo_uuid = ?`
	args := []any{todoUUID}
	if !includeDeleted {
		query += ` AND deleted_at IS NULL`
	}
	query += ` ORDER BY client_created_at ASC, uuid ASC`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list todo replies: %w", err)
	}
	defer rows.Close()
	var replies []TodoReplyRecord
	for rows.Next() {
		reply, err := scanTodoReply(rows)
		if err != nil {
			return nil, err
		}
		replies = append(replies, reply)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate todo replies: %w", err)
	}
	return replies, nil
}

func (s *Store) PendingTodoReplies() ([]TodoReplyRecord, error) {
	rows, err := s.db.Query(todoReplySelectSQL() + `
		WHERE synced_at IS NULL
		ORDER BY client_updated_at ASC, client_created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query pending todo replies: %w", err)
	}
	defer rows.Close()
	var replies []TodoReplyRecord
	for rows.Next() {
		reply, err := scanTodoReply(rows)
		if err != nil {
			return nil, err
		}
		replies = append(replies, reply)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending todo replies: %w", err)
	}
	return replies, nil
}

func (s *Store) MarkTodoReplySynced(uuid string, syncedAt time.Time) error {
	_, err := s.db.Exec(`UPDATE todo_replies SET synced_at = ?, last_sync_error = '' WHERE uuid = ?`, formatTime(syncedAt), uuid)
	if err != nil {
		return fmt.Errorf("mark todo reply synced: %w", err)
	}
	return nil
}

func (s *Store) MarkTodoReplySyncError(uuid, detail string) error {
	_, err := s.db.Exec(`UPDATE todo_replies SET last_sync_error = ? WHERE uuid = ?`, detail, uuid)
	if err != nil {
		return fmt.Errorf("mark todo reply sync error: %w", err)
	}
	return nil
}

func (s *Store) SaveAITaskRun(run AITaskRunRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO ai_task_runs (
			uuid, project_type, project_uuid, todo_uuid, parent_run_uuid, agent_name, status,
			title, latest_phase, latest_summary, context_snapshot_json, started_at,
			last_heartbeat_at, completed_at, deleted_at, created_at, updated_at,
			client_created_at, client_updated_at, synced_at, last_sync_error
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(uuid) DO UPDATE SET
			project_type = excluded.project_type,
			project_uuid = excluded.project_uuid,
			todo_uuid = excluded.todo_uuid,
			parent_run_uuid = excluded.parent_run_uuid,
			agent_name = excluded.agent_name,
			status = excluded.status,
			title = excluded.title,
			latest_phase = excluded.latest_phase,
			latest_summary = excluded.latest_summary,
			context_snapshot_json = excluded.context_snapshot_json,
			started_at = excluded.started_at,
			last_heartbeat_at = excluded.last_heartbeat_at,
			completed_at = excluded.completed_at,
			deleted_at = excluded.deleted_at,
			updated_at = excluded.updated_at,
			client_updated_at = excluded.client_updated_at,
			synced_at = excluded.synced_at,
			last_sync_error = excluded.last_sync_error
	`, run.UUID, run.ProjectType, run.ProjectUUID, run.TodoUUID, run.ParentRunUUID, run.AgentName, run.Status,
		run.Title, run.LatestPhase, run.LatestSummary, emptyJSON(run.ContextSnapshotJSON),
		formatNullTime(run.StartedAt), formatNullTime(run.LastHeartbeatAt), formatNullTime(run.CompletedAt), formatNullTime(run.DeletedAt),
		formatTime(run.CreatedAt), formatTime(run.UpdatedAt), formatTime(run.ClientCreatedAt), formatTime(run.ClientUpdatedAt),
		formatNullTime(run.SyncedAt), run.LastSyncError)
	if err != nil {
		return fmt.Errorf("save ai task run: %w", err)
	}
	return nil
}

func (s *Store) GetAITaskRun(uuid string) (AITaskRunRecord, error) {
	row := s.db.QueryRow(`
		SELECT uuid, project_type, project_uuid, todo_uuid, parent_run_uuid, agent_name, status,
			title, latest_phase, latest_summary, context_snapshot_json, started_at,
			last_heartbeat_at, completed_at, deleted_at, created_at, updated_at,
			client_created_at, client_updated_at, synced_at, last_sync_error
		FROM ai_task_runs
		WHERE uuid = ?
	`, uuid)
	return scanAITaskRun(row)
}

func (s *Store) PendingAITaskRuns() ([]AITaskRunRecord, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT r.uuid, r.project_type, r.project_uuid, r.todo_uuid, r.parent_run_uuid, r.agent_name, r.status,
			r.title, r.latest_phase, r.latest_summary, r.context_snapshot_json, r.started_at,
			r.last_heartbeat_at, r.completed_at, r.deleted_at, r.created_at, r.updated_at,
			r.client_created_at, r.client_updated_at, r.synced_at, r.last_sync_error
		FROM ai_task_runs r
		WHERE r.synced_at IS NULL
		   OR r.uuid IN (
				SELECT e.run_uuid
				FROM ai_task_events e
				JOIN ai_task_runs pr ON pr.uuid = e.run_uuid
				WHERE e.synced_at IS NULL AND pr.synced_at IS NULL
		   )
		ORDER BY r.client_updated_at ASC, r.created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query pending ai task runs: %w", err)
	}
	defer rows.Close()
	var runs []AITaskRunRecord
	for rows.Next() {
		run, err := scanAITaskRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending ai task runs: %w", err)
	}
	return runs, nil
}

func (s *Store) MarkAITaskRunSynced(uuid string, syncedAt time.Time) error {
	_, err := s.db.Exec(
		`UPDATE ai_task_runs SET synced_at = ?, last_sync_error = '' WHERE uuid = ?`,
		formatTime(syncedAt), uuid,
	)
	if err != nil {
		return fmt.Errorf("mark ai task run synced: %w", err)
	}
	return nil
}

func (s *Store) MarkAITaskRunSyncError(uuid, detail string) error {
	_, err := s.db.Exec(`UPDATE ai_task_runs SET last_sync_error = ? WHERE uuid = ?`, detail, uuid)
	if err != nil {
		return fmt.Errorf("mark ai task run sync error: %w", err)
	}
	return nil
}

func (s *Store) SaveAITaskEvent(event AITaskEventRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO ai_task_events (
			uuid, run_uuid, todo_uuid, event_type, severity, title, content, metadata_json,
			payload_hash_version, payload_hash, occurred_at, created_at, updated_at,
			client_created_at, client_updated_at, synced_at, last_sync_error
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, event.UUID, event.RunUUID, event.TodoUUID, event.EventType, event.Severity, event.Title, event.Content,
		emptyJSON(event.MetadataJSON), event.PayloadHashVersion, event.PayloadHash, formatTime(event.OccurredAt),
		formatTime(event.CreatedAt), formatTime(event.UpdatedAt), formatTime(event.ClientCreatedAt), formatTime(event.ClientUpdatedAt),
		formatNullTime(event.SyncedAt), event.LastSyncError)
	if err != nil {
		return fmt.Errorf("save ai task event: %w", err)
	}
	return nil
}

func (s *Store) PendingAITaskEvents() ([]AITaskEventRecord, error) {
	rows, err := s.db.Query(`
		SELECT uuid, run_uuid, todo_uuid, event_type, severity, title, content, metadata_json,
			payload_hash_version, payload_hash, occurred_at, created_at, updated_at,
			client_created_at, client_updated_at, synced_at, last_sync_error
		FROM ai_task_events
		WHERE synced_at IS NULL
		ORDER BY occurred_at ASC, created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query pending ai task events: %w", err)
	}
	defer rows.Close()
	var events []AITaskEventRecord
	for rows.Next() {
		event, err := scanAITaskEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending ai task events: %w", err)
	}
	return events, nil
}

func (s *Store) MarkAITaskEventSynced(uuid string, syncedAt time.Time) error {
	_, err := s.db.Exec(
		`UPDATE ai_task_events SET synced_at = ?, last_sync_error = '' WHERE uuid = ?`,
		formatTime(syncedAt), uuid,
	)
	if err != nil {
		return fmt.Errorf("mark ai task event synced: %w", err)
	}
	return nil
}

func (s *Store) MarkAITaskEventSyncError(uuid, detail string) error {
	_, err := s.db.Exec(`UPDATE ai_task_events SET last_sync_error = ? WHERE uuid = ?`, detail, uuid)
	if err != nil {
		return fmt.Errorf("mark ai task event sync error: %w", err)
	}
	return nil
}

func (s *Store) SyncCursors() (map[string]time.Time, error) {
	rows, err := s.db.Query(`SELECT model, cursor_at FROM sync_cursors`)
	if err != nil {
		return nil, fmt.Errorf("query sync cursors: %w", err)
	}
	defer rows.Close()
	cursors := map[string]time.Time{}
	for rows.Next() {
		var model, cursorAt string
		if err := rows.Scan(&model, &cursorAt); err != nil {
			return nil, fmt.Errorf("scan sync cursor: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, cursorAt)
		if err != nil {
			return nil, fmt.Errorf("parse sync cursor %s: %w", model, err)
		}
		cursors[model] = parsed
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sync cursors: %w", err)
	}
	return cursors, nil
}

func (s *Store) UpdateSyncCursors(models []string, cursorAt time.Time) error {
	updatedAt := time.Now().UTC()
	for _, model := range models {
		if _, err := s.db.Exec(`
			INSERT INTO sync_cursors (model, cursor_at, updated_at)
			VALUES (?, ?, ?)
			ON CONFLICT(model) DO UPDATE SET cursor_at = excluded.cursor_at, updated_at = excluded.updated_at
		`, model, formatTime(cursorAt), formatTime(updatedAt)); err != nil {
			return fmt.Errorf("update sync cursor %s: %w", model, err)
		}
	}
	return nil
}

func (s *Store) SetActiveRun(active ActiveRunRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO active_ai_runs (
			scope_key, session_id, run_uuid, cwd, project_type, project_uuid, todo_uuid, agent_name, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(scope_key, session_id) DO UPDATE SET
			run_uuid = excluded.run_uuid,
			cwd = excluded.cwd,
			project_type = excluded.project_type,
			project_uuid = excluded.project_uuid,
			todo_uuid = excluded.todo_uuid,
			agent_name = excluded.agent_name,
			updated_at = excluded.updated_at
	`, active.ScopeKey, active.SessionID, active.RunUUID, active.CWD, active.ProjectType, active.ProjectUUID,
		active.TodoUUID, active.AgentName, formatTime(active.UpdatedAt))
	if err != nil {
		return fmt.Errorf("set active run: %w", err)
	}
	return nil
}

func (s *Store) ClearActiveRun(scopeKey, sessionID string) error {
	_, err := s.db.Exec(`DELETE FROM active_ai_runs WHERE scope_key = ? AND session_id = ?`, scopeKey, sessionID)
	if err != nil {
		return fmt.Errorf("clear active run: %w", err)
	}
	return nil
}

func (s *Store) ClearActiveRunByRunUUID(runUUID string) error {
	_, err := s.db.Exec(`DELETE FROM active_ai_runs WHERE run_uuid = ?`, runUUID)
	if err != nil {
		return fmt.Errorf("clear active run by run uuid: %w", err)
	}
	return nil
}

func (s *Store) ActiveRun(scopeKey, sessionID string) (ActiveRunRecord, bool, error) {
	row := s.db.QueryRow(`
		SELECT scope_key, session_id, run_uuid, cwd, project_type, project_uuid, todo_uuid, agent_name, updated_at
		FROM active_ai_runs
		WHERE scope_key = ? AND session_id = ?
	`, scopeKey, sessionID)
	active, err := scanActiveRun(row)
	if err == nil {
		return active, true, nil
	}
	if err == sql.ErrNoRows {
		return ActiveRunRecord{}, false, nil
	}
	return ActiveRunRecord{}, false, err
}

func (s *Store) RunningRunsForScope(scopeKey string) ([]AITaskRunRecord, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT r.uuid, r.project_type, r.project_uuid, r.todo_uuid, r.parent_run_uuid, r.agent_name, r.status,
			r.title, r.latest_phase, r.latest_summary, r.context_snapshot_json, r.started_at,
			r.last_heartbeat_at, r.completed_at, r.deleted_at, r.created_at, r.updated_at,
			r.client_created_at, r.client_updated_at, r.synced_at, r.last_sync_error
		FROM ai_task_runs r
		JOIN active_ai_runs a ON a.run_uuid = r.uuid
		WHERE a.scope_key = ?
		  AND r.status IN ('queued', 'running', 'blocked')
		  AND r.deleted_at IS NULL
		ORDER BY r.updated_at DESC
	`, scopeKey)
	if err != nil {
		return nil, fmt.Errorf("query scope running runs: %w", err)
	}
	defer rows.Close()
	var runs []AITaskRunRecord
	for rows.Next() {
		run, err := scanAITaskRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate scope running runs: %w", err)
	}
	return runs, nil
}

func (s *Store) LatestRunningRun(projectType, projectUUID, todoUUID, agentName string) (AITaskRunRecord, bool, error) {
	row := s.db.QueryRow(`
		SELECT uuid, project_type, project_uuid, todo_uuid, parent_run_uuid, agent_name, status,
			title, latest_phase, latest_summary, context_snapshot_json, started_at,
			last_heartbeat_at, completed_at, deleted_at, created_at, updated_at,
			client_created_at, client_updated_at, synced_at, last_sync_error
		FROM ai_task_runs
		WHERE project_type = ? AND project_uuid = ? AND todo_uuid = ? AND agent_name = ?
		  AND status IN ('queued', 'running', 'blocked')
		  AND deleted_at IS NULL
		ORDER BY updated_at DESC
		LIMIT 1
	`, projectType, projectUUID, todoUUID, agentName)
	run, err := scanAITaskRun(row)
	if err == nil {
		return run, true, nil
	}
	if err == sql.ErrNoRows {
		return AITaskRunRecord{}, false, nil
	}
	return AITaskRunRecord{}, false, err
}

func scanRecord(scanner interface {
	Scan(dest ...any) error
}) (Record, error) {
	var record Record
	var loggedAt string
	var createdAt string
	var updatedAt string
	var syncedAt sql.NullString
	var lastSyncError string

	if err := scanner.Scan(
		&record.UUID,
		&record.LogType,
		&record.Content,
		&loggedAt,
		&createdAt,
		&updatedAt,
		&syncedAt,
		&lastSyncError,
	); err != nil {
		return Record{}, fmt.Errorf("scan record: %w", err)
	}

	var err error
	record.LoggedAt, err = time.Parse(time.RFC3339Nano, loggedAt)
	if err != nil {
		return Record{}, fmt.Errorf("parse logged_at: %w", err)
	}
	record.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return Record{}, fmt.Errorf("parse created_at: %w", err)
	}
	record.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return Record{}, fmt.Errorf("parse updated_at: %w", err)
	}

	if syncedAt.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, syncedAt.String)
		if err != nil {
			return Record{}, fmt.Errorf("parse synced_at: %w", err)
		}
		record.SyncedAt = &parsed
	}
	record.LastSyncError = lastSyncError

	return record, nil
}

func scanTodo(scanner interface {
	Scan(dest ...any) error
}) (TodoRecord, error) {
	var todo TodoRecord
	var completedAt, deletedAt, archivedAt, deadline, syncedAt sql.NullString
	var createdAt, updatedAt, clientCreatedAt, clientUpdatedAt string
	if err := scanner.Scan(
		&todo.UUID, &todo.ParentUUID, &todo.Content, &todo.Notes, &todo.Completed, &todo.SortOrder, &todo.ListUUID,
		&completedAt, &deletedAt, &archivedAt, &deadline, &todo.GoalUUID, &todo.MilestoneUUID,
		&todo.CategoryUUID, &todo.TaskRole, &todo.TodoSource, &todo.CompletionMode, &todo.CompletionSource,
		&todo.AIEvaluationStatus, &todo.AICompletionSummary, &createdAt, &updatedAt, &clientCreatedAt,
		&clientUpdatedAt, &syncedAt, &todo.LastSyncError,
	); err != nil {
		return TodoRecord{}, err
	}
	var err error
	if todo.CompletedAt, err = parseNullTime(completedAt); err != nil {
		return TodoRecord{}, fmt.Errorf("parse completed_at: %w", err)
	}
	if todo.DeletedAt, err = parseNullTime(deletedAt); err != nil {
		return TodoRecord{}, fmt.Errorf("parse deleted_at: %w", err)
	}
	if todo.ArchivedAt, err = parseNullTime(archivedAt); err != nil {
		return TodoRecord{}, fmt.Errorf("parse archived_at: %w", err)
	}
	if todo.Deadline, err = parseNullTime(deadline); err != nil {
		return TodoRecord{}, fmt.Errorf("parse deadline: %w", err)
	}
	if todo.SyncedAt, err = parseNullTime(syncedAt); err != nil {
		return TodoRecord{}, fmt.Errorf("parse synced_at: %w", err)
	}
	if todo.CreatedAt, err = parseTime(createdAt); err != nil {
		return TodoRecord{}, fmt.Errorf("parse created_at: %w", err)
	}
	if todo.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return TodoRecord{}, fmt.Errorf("parse updated_at: %w", err)
	}
	if todo.ClientCreatedAt, err = parseTime(clientCreatedAt); err != nil {
		return TodoRecord{}, fmt.Errorf("parse client_created_at: %w", err)
	}
	if todo.ClientUpdatedAt, err = parseTime(clientUpdatedAt); err != nil {
		return TodoRecord{}, fmt.Errorf("parse client_updated_at: %w", err)
	}
	return todo, nil
}

func scanTodoList(scanner interface {
	Scan(dest ...any) error
}) (TodoListRecord, error) {
	var list TodoListRecord
	var deletedAt, syncedAt sql.NullString
	var createdAt, updatedAt, clientCreatedAt, clientUpdatedAt string
	if err := scanner.Scan(
		&list.UUID, &list.Name, &list.Color, &list.Icon, &list.SortOrder, &deletedAt,
		&createdAt, &updatedAt, &clientCreatedAt, &clientUpdatedAt, &syncedAt, &list.LastSyncError,
	); err != nil {
		return TodoListRecord{}, err
	}
	var err error
	if list.DeletedAt, err = parseNullTime(deletedAt); err != nil {
		return TodoListRecord{}, fmt.Errorf("parse deleted_at: %w", err)
	}
	if list.SyncedAt, err = parseNullTime(syncedAt); err != nil {
		return TodoListRecord{}, fmt.Errorf("parse synced_at: %w", err)
	}
	if list.CreatedAt, err = parseTime(createdAt); err != nil {
		return TodoListRecord{}, fmt.Errorf("parse created_at: %w", err)
	}
	if list.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return TodoListRecord{}, fmt.Errorf("parse updated_at: %w", err)
	}
	if list.ClientCreatedAt, err = parseTime(clientCreatedAt); err != nil {
		return TodoListRecord{}, fmt.Errorf("parse client_created_at: %w", err)
	}
	if list.ClientUpdatedAt, err = parseTime(clientUpdatedAt); err != nil {
		return TodoListRecord{}, fmt.Errorf("parse client_updated_at: %w", err)
	}
	return list, nil
}

func scanTodoReply(scanner interface {
	Scan(dest ...any) error
}) (TodoReplyRecord, error) {
	var reply TodoReplyRecord
	var deletedAt, syncedAt sql.NullString
	var createdAt, updatedAt, clientCreatedAt, clientUpdatedAt string
	if err := scanner.Scan(
		&reply.UUID, &reply.TodoUUID, &reply.Content, &deletedAt, &reply.SourceType, &reply.SourceName,
		&reply.ActorDisplayName, &createdAt, &updatedAt, &clientCreatedAt, &clientUpdatedAt,
		&syncedAt, &reply.LastSyncError,
	); err != nil {
		return TodoReplyRecord{}, err
	}
	var err error
	if reply.DeletedAt, err = parseNullTime(deletedAt); err != nil {
		return TodoReplyRecord{}, fmt.Errorf("parse deleted_at: %w", err)
	}
	if reply.SyncedAt, err = parseNullTime(syncedAt); err != nil {
		return TodoReplyRecord{}, fmt.Errorf("parse synced_at: %w", err)
	}
	if reply.CreatedAt, err = parseTime(createdAt); err != nil {
		return TodoReplyRecord{}, fmt.Errorf("parse created_at: %w", err)
	}
	if reply.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return TodoReplyRecord{}, fmt.Errorf("parse updated_at: %w", err)
	}
	if reply.ClientCreatedAt, err = parseTime(clientCreatedAt); err != nil {
		return TodoReplyRecord{}, fmt.Errorf("parse client_created_at: %w", err)
	}
	if reply.ClientUpdatedAt, err = parseTime(clientUpdatedAt); err != nil {
		return TodoReplyRecord{}, fmt.Errorf("parse client_updated_at: %w", err)
	}
	return reply, nil
}

func todoSelectSQL() string {
	return `SELECT uuid, parent_uuid, content, notes, completed, sort_order, list_uuid,
		completed_at, deleted_at, archived_at, deadline, goal_uuid, milestone_uuid,
		category_uuid, task_role, todo_source, completion_mode, completion_source,
		ai_evaluation_status, ai_completion_summary, created_at, updated_at,
		client_created_at, client_updated_at, synced_at, last_sync_error
		FROM todos`
}

func todoListSelectSQL() string {
	return `SELECT uuid, name, color, icon, sort_order, deleted_at, created_at, updated_at,
		client_created_at, client_updated_at, synced_at, last_sync_error
		FROM todo_lists`
}

func todoReplySelectSQL() string {
	return `SELECT uuid, todo_uuid, content, deleted_at, source_type, source_name,
		actor_display_name, created_at, updated_at, client_created_at,
		client_updated_at, synced_at, last_sync_error
		FROM todo_replies`
}

func formatNullTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func createTodoSchema(tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS todo_lists (
			uuid TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			color TEXT NOT NULL DEFAULT '',
			icon TEXT NOT NULL DEFAULT '',
			sort_order INTEGER NOT NULL DEFAULT 0,
			deleted_at TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			client_created_at TEXT NOT NULL,
			client_updated_at TEXT NOT NULL,
			synced_at TEXT,
			last_sync_error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_todo_lists_active_order ON todo_lists(deleted_at, sort_order, client_created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_todo_lists_pending ON todo_lists(synced_at, client_updated_at)`,
		`CREATE TABLE IF NOT EXISTS todos (
			uuid TEXT PRIMARY KEY,
			parent_uuid TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			notes TEXT NOT NULL DEFAULT '',
			completed INTEGER NOT NULL DEFAULT 0,
			sort_order REAL NOT NULL DEFAULT 0,
			list_uuid TEXT NOT NULL DEFAULT '',
			completed_at TEXT,
			deleted_at TEXT,
			archived_at TEXT,
			deadline TEXT,
			goal_uuid TEXT NOT NULL DEFAULT '',
			milestone_uuid TEXT NOT NULL DEFAULT '',
			category_uuid TEXT NOT NULL DEFAULT '',
			task_role TEXT NOT NULL DEFAULT '',
			todo_source TEXT NOT NULL DEFAULT '',
			completion_mode TEXT NOT NULL DEFAULT 'manual',
			completion_source TEXT NOT NULL DEFAULT 'manual',
			ai_evaluation_status TEXT NOT NULL DEFAULT 'not_requested',
			ai_completion_summary TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			client_created_at TEXT NOT NULL,
			client_updated_at TEXT NOT NULL,
			synced_at TEXT,
			last_sync_error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_todos_active_order ON todos(deleted_at, completed, sort_order, client_created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_todos_list ON todos(list_uuid, deleted_at, completed, sort_order)`,
		`CREATE INDEX IF NOT EXISTS idx_todos_pending ON todos(synced_at, client_updated_at)`,
		`CREATE TABLE IF NOT EXISTS todo_replies (
			uuid TEXT PRIMARY KEY,
			todo_uuid TEXT NOT NULL,
			content TEXT NOT NULL,
			deleted_at TEXT,
			source_type TEXT NOT NULL DEFAULT '',
			source_name TEXT NOT NULL DEFAULT '',
			actor_display_name TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			client_created_at TEXT NOT NULL,
			client_updated_at TEXT NOT NULL,
			synced_at TEXT,
			last_sync_error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_todo_replies_todo_created ON todo_replies(todo_uuid, deleted_at, client_created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_todo_replies_pending ON todo_replies(synced_at, client_updated_at)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("create todo schema: %w", err)
		}
	}
	return nil
}

func createAISchema(tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS ai_task_runs (
			uuid TEXT PRIMARY KEY,
			project_type TEXT NOT NULL DEFAULT '',
			project_uuid TEXT NOT NULL DEFAULT '',
			todo_uuid TEXT NOT NULL DEFAULT '',
			parent_run_uuid TEXT NOT NULL DEFAULT '',
			agent_name TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'running',
			title TEXT NOT NULL DEFAULT '',
			latest_phase TEXT NOT NULL DEFAULT '',
			latest_summary TEXT NOT NULL DEFAULT '',
			context_snapshot_json TEXT NOT NULL DEFAULT '{}',
			started_at TEXT,
			last_heartbeat_at TEXT,
			completed_at TEXT,
			deleted_at TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			client_created_at TEXT NOT NULL,
			client_updated_at TEXT NOT NULL,
			synced_at TEXT,
			last_sync_error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ai_task_runs_scope ON ai_task_runs(project_type, project_uuid, todo_uuid, agent_name, status)`,
		`CREATE INDEX IF NOT EXISTS idx_ai_task_runs_pending ON ai_task_runs(synced_at, client_updated_at)`,
		`CREATE TABLE IF NOT EXISTS ai_task_events (
			uuid TEXT PRIMARY KEY,
			run_uuid TEXT NOT NULL,
			todo_uuid TEXT NOT NULL DEFAULT '',
			event_type TEXT NOT NULL,
			severity TEXT NOT NULL DEFAULT 'info',
			title TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL DEFAULT '',
			metadata_json TEXT NOT NULL DEFAULT '{}',
			payload_hash_version INTEGER NOT NULL DEFAULT 1,
			payload_hash TEXT NOT NULL,
			occurred_at TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			client_created_at TEXT NOT NULL,
			client_updated_at TEXT NOT NULL,
			synced_at TEXT,
			last_sync_error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ai_task_events_run_pending ON ai_task_events(run_uuid, synced_at, occurred_at)`,
		`CREATE TABLE IF NOT EXISTS active_ai_runs (
			scope_key TEXT NOT NULL,
			session_id TEXT NOT NULL,
			run_uuid TEXT NOT NULL,
			cwd TEXT NOT NULL DEFAULT '',
			project_type TEXT NOT NULL DEFAULT '',
			project_uuid TEXT NOT NULL DEFAULT '',
			todo_uuid TEXT NOT NULL DEFAULT '',
			agent_name TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL,
			PRIMARY KEY (scope_key, session_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_active_ai_runs_run ON active_ai_runs(run_uuid)`,
		`CREATE TABLE IF NOT EXISTS sync_cursors (
			model TEXT PRIMARY KEY,
			cursor_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("create ai schema: %w", err)
		}
	}
	return nil
}

func initSyncCursors(tx *sql.Tx, now time.Time) error {
	for _, model := range []string{"ai_task_runs", "ai_task_events"} {
		if _, err := tx.Exec(`
			INSERT OR IGNORE INTO sync_cursors (model, cursor_at, updated_at)
			VALUES (?, ?, ?)
		`, model, formatTime(now), formatTime(now)); err != nil {
			return fmt.Errorf("init sync cursor %s: %w", model, err)
		}
	}
	return nil
}

func addColumnIfMissing(tx *sql.Tx, table, column, definition string) error {
	has, err := tableHasColumn(tx, table, column)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := tx.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)); err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}

func tableHasColumnDB(db *sql.DB, table, column string) (bool, error) {
	if !safeSQLiteIdent(table) || !safeSQLiteIdent(column) {
		return false, fmt.Errorf("unsafe sqlite identifier")
	}
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("table info %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, fmt.Errorf("scan table info %s: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate table info %s: %w", table, err)
	}
	return false, nil
}

func tableHasColumn(tx *sql.Tx, table, column string) (bool, error) {
	if !safeSQLiteIdent(table) || !safeSQLiteIdent(column) {
		return false, fmt.Errorf("unsafe sqlite identifier")
	}
	rows, err := tx.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("table info %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, fmt.Errorf("scan table info %s: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate table info %s: %w", table, err)
	}
	return false, nil
}

func safeSQLiteIdent(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func scanAITaskRun(scanner interface {
	Scan(dest ...any) error
}) (AITaskRunRecord, error) {
	var run AITaskRunRecord
	var startedAt, lastHeartbeatAt, completedAt, deletedAt, syncedAt sql.NullString
	var createdAt, updatedAt, clientCreatedAt, clientUpdatedAt string
	if err := scanner.Scan(
		&run.UUID, &run.ProjectType, &run.ProjectUUID, &run.TodoUUID, &run.ParentRunUUID, &run.AgentName, &run.Status,
		&run.Title, &run.LatestPhase, &run.LatestSummary, &run.ContextSnapshotJSON, &startedAt,
		&lastHeartbeatAt, &completedAt, &deletedAt, &createdAt, &updatedAt,
		&clientCreatedAt, &clientUpdatedAt, &syncedAt, &run.LastSyncError,
	); err != nil {
		return AITaskRunRecord{}, err
	}
	var err error
	if run.StartedAt, err = parseNullTime(startedAt); err != nil {
		return AITaskRunRecord{}, fmt.Errorf("parse started_at: %w", err)
	}
	if run.LastHeartbeatAt, err = parseNullTime(lastHeartbeatAt); err != nil {
		return AITaskRunRecord{}, fmt.Errorf("parse last_heartbeat_at: %w", err)
	}
	if run.CompletedAt, err = parseNullTime(completedAt); err != nil {
		return AITaskRunRecord{}, fmt.Errorf("parse completed_at: %w", err)
	}
	if run.DeletedAt, err = parseNullTime(deletedAt); err != nil {
		return AITaskRunRecord{}, fmt.Errorf("parse deleted_at: %w", err)
	}
	if run.SyncedAt, err = parseNullTime(syncedAt); err != nil {
		return AITaskRunRecord{}, fmt.Errorf("parse synced_at: %w", err)
	}
	if run.CreatedAt, err = parseTime(createdAt); err != nil {
		return AITaskRunRecord{}, fmt.Errorf("parse created_at: %w", err)
	}
	if run.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return AITaskRunRecord{}, fmt.Errorf("parse updated_at: %w", err)
	}
	if run.ClientCreatedAt, err = parseTime(clientCreatedAt); err != nil {
		return AITaskRunRecord{}, fmt.Errorf("parse client_created_at: %w", err)
	}
	if run.ClientUpdatedAt, err = parseTime(clientUpdatedAt); err != nil {
		return AITaskRunRecord{}, fmt.Errorf("parse client_updated_at: %w", err)
	}
	return run, nil
}

func scanAITaskEvent(scanner interface {
	Scan(dest ...any) error
}) (AITaskEventRecord, error) {
	var event AITaskEventRecord
	var occurredAt, createdAt, updatedAt, clientCreatedAt, clientUpdatedAt string
	var syncedAt sql.NullString
	if err := scanner.Scan(
		&event.UUID, &event.RunUUID, &event.TodoUUID, &event.EventType, &event.Severity, &event.Title, &event.Content,
		&event.MetadataJSON, &event.PayloadHashVersion, &event.PayloadHash, &occurredAt, &createdAt, &updatedAt,
		&clientCreatedAt, &clientUpdatedAt, &syncedAt, &event.LastSyncError,
	); err != nil {
		return AITaskEventRecord{}, err
	}
	var err error
	if event.OccurredAt, err = parseTime(occurredAt); err != nil {
		return AITaskEventRecord{}, fmt.Errorf("parse occurred_at: %w", err)
	}
	if event.CreatedAt, err = parseTime(createdAt); err != nil {
		return AITaskEventRecord{}, fmt.Errorf("parse created_at: %w", err)
	}
	if event.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return AITaskEventRecord{}, fmt.Errorf("parse updated_at: %w", err)
	}
	if event.ClientCreatedAt, err = parseTime(clientCreatedAt); err != nil {
		return AITaskEventRecord{}, fmt.Errorf("parse client_created_at: %w", err)
	}
	if event.ClientUpdatedAt, err = parseTime(clientUpdatedAt); err != nil {
		return AITaskEventRecord{}, fmt.Errorf("parse client_updated_at: %w", err)
	}
	if event.SyncedAt, err = parseNullTime(syncedAt); err != nil {
		return AITaskEventRecord{}, fmt.Errorf("parse synced_at: %w", err)
	}
	return event, nil
}

func scanActiveRun(scanner interface {
	Scan(dest ...any) error
}) (ActiveRunRecord, error) {
	var active ActiveRunRecord
	var updatedAt string
	if err := scanner.Scan(
		&active.ScopeKey, &active.SessionID, &active.RunUUID, &active.CWD, &active.ProjectType,
		&active.ProjectUUID, &active.TodoUUID, &active.AgentName, &updatedAt,
	); err != nil {
		return ActiveRunRecord{}, err
	}
	parsed, err := parseTime(updatedAt)
	if err != nil {
		return ActiveRunRecord{}, fmt.Errorf("parse active updated_at: %w", err)
	}
	active.UpdatedAt = parsed
	return active, nil
}

func parseNullTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid || value.String == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func emptyJSON(value string) string {
	if strings.TrimSpace(value) == "" {
		return "{}"
	}
	return value
}
