package integlife

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	apiURL     string
	apiToken   string
	httpClient *http.Client
}

func NewClient(apiURL, apiToken string, timeout time.Duration) *Client {
	return &Client{
		apiURL:   strings.TrimRight(apiURL, "/"),
		apiToken: apiToken,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

func (c *Client) CanSync() bool {
	return c.apiToken != ""
}

// Login authenticates with username/password, creates a named API token, and returns the secret.
func (c *Client) Login(ctx context.Context, username, password string) (string, error) {
	// Step 1: get JWT via username/password
	loginBody, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+"/login", bytes.NewReader(loginBody))
	if err != nil {
		return "", fmt.Errorf("build login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("send login request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("login http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	var loginResp struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
		return "", fmt.Errorf("decode login response: %w", err)
	}
	if loginResp.Token == "" {
		return "", fmt.Errorf("empty token in login response")
	}

	// Step 2: create a persistent API token using the JWT
	tokenBody, _ := json.Marshal(map[string]string{"name": "life-cli"})
	req2, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+"/api/tokens", bytes.NewReader(tokenBody))
	if err != nil {
		return "", fmt.Errorf("build create-token request: %w", err)
	}
	req2.Header.Set("Authorization", "Bearer "+loginResp.Token)
	req2.Header.Set("Content-Type", "application/json")

	resp2, err := c.httpClient.Do(req2)
	if err != nil {
		return "", fmt.Errorf("send create-token request: %w", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(io.LimitReader(resp2.Body, 300))
		return "", fmt.Errorf("create-token http %d: %s", resp2.StatusCode, strings.TrimSpace(string(data)))
	}

	var tokenResp struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tokenResp.Token == "" {
		return "", fmt.Errorf("empty API token in response")
	}

	return tokenResp.Token, nil
}

func (c *Client) SyncRecord(ctx context.Context, record Record) (string, error) {
	ack, err := c.SyncRecords(ctx, SyncBatch{StatusLogs: []Record{record}})
	if err != nil {
		return "", err
	}
	return ack.Detail, nil
}

func (c *Client) SyncRecords(ctx context.Context, batch SyncBatch) (SyncAck, error) {
	if !c.CanSync() {
		return SyncAck{}, fmt.Errorf("INTEGLIFE_API_TOKEN not set")
	}

	payload := buildSyncBatchPayload(batch)
	body, err := json.Marshal(payload)
	if err != nil {
		return SyncAck{}, fmt.Errorf("marshal sync payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+"/api/notes/sync", bytes.NewReader(body))
	if err != nil {
		return SyncAck{}, fmt.Errorf("build sync request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return SyncAck{}, fmt.Errorf("send sync request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return SyncAck{}, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	var syncResp syncResponse
	if err := json.NewDecoder(resp.Body).Decode(&syncResp); err != nil {
		return SyncAck{}, fmt.Errorf("decode sync response: %w", err)
	}
	return buildSyncAck(batch, syncResp, resp.Status), nil
}

type syncPayload struct {
	SyncModels        []string           `json:"sync_models"`
	LastSyncAtByModel map[string]*string `json:"last_sync_at_by_model"`
	Notes             []syncNote         `json:"notes"`
	Comments          []any              `json:"comments"`
	MoneyCurrencies   []any              `json:"money_currencies"`
	MoneyTransactions []any              `json:"money_transactions"`
	MoneyBooks        []any              `json:"money_books"`
	MoneyBookMembers  []any              `json:"money_book_members"`
	Goals             []any              `json:"goals"`
	GoalMilestones    []any              `json:"goal_milestones"`
	GoalCheckins      []any              `json:"goal_checkins"`
	Todos             []syncTodo         `json:"todos"`
	TodoReplies       []syncTodoReply    `json:"todo_replies"`
	TodoLists         []syncTodoList     `json:"todo_lists"`
	Events            []any              `json:"events"`
	Pomodoros         []any              `json:"pomodoros"`
	StatusLogs        []syncStatusLog    `json:"status_logs"`
	AITaskRuns        []syncAITaskRun    `json:"ai_task_runs"`
	AITaskEvents      []syncAITaskEvent  `json:"ai_task_events"`
}

type syncStatusLog struct {
	UUID      string `json:"uuid"`
	LoggedAt  string `json:"logged_at"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	UserID    int    `json:"user_id"`
	LogType   string `json:"log_type"`
	Content   string `json:"content"`
}

type syncNote struct {
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	UserID       int     `json:"user_id"`
	UUID         string  `json:"uuid"`
	DeletedAt    *string `json:"deleted_at,omitempty"`
	Path         string  `json:"path,omitempty"`
	Content      string  `json:"content"`
	SyncETag     string  `json:"sync_etag,omitempty"`
	SyncBasePath string  `json:"sync_base_path,omitempty"`
}

type syncTodo struct {
	CreatedAt           string  `json:"created_at"`
	UpdatedAt           string  `json:"updated_at"`
	UserID              int     `json:"user_id"`
	UUID                string  `json:"uuid"`
	ParentUUID          string  `json:"parent_uuid"`
	DeletedAt           *string `json:"deleted_at,omitempty"`
	ArchivedAt          *string `json:"archived_at,omitempty"`
	Deadline            *string `json:"deadline,omitempty"`
	Content             string  `json:"content"`
	Notes               string  `json:"notes"`
	Completed           bool    `json:"completed"`
	Order               float64 `json:"order"`
	GoalUUID            *string `json:"goal_uuid,omitempty"`
	MilestoneUUID       *string `json:"milestone_uuid,omitempty"`
	TaskRole            string  `json:"task_role,omitempty"`
	TodoSource          string  `json:"todo_source,omitempty"`
	CompletionMode      string  `json:"completion_mode,omitempty"`
	AIEvaluationStatus  string  `json:"ai_evaluation_status,omitempty"`
	AICompletionSummary string  `json:"ai_completion_summary,omitempty"`
	CompletedAt         *string `json:"completed_at,omitempty"`
	CompletionSource    string  `json:"completion_source,omitempty"`
	ListUUID            *string `json:"list_uuid,omitempty"`
	CategoryUUID        *string `json:"category_uuid,omitempty"`
}

type syncTodoList struct {
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	UserID    int     `json:"user_id"`
	UUID      string  `json:"uuid"`
	DeletedAt *string `json:"deleted_at,omitempty"`
	Name      string  `json:"name"`
	Color     string  `json:"color,omitempty"`
	Icon      string  `json:"icon,omitempty"`
	SortOrder int     `json:"sort_order"`
}

type syncTodoReply struct {
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
	UserID           int     `json:"user_id"`
	BookID           int     `json:"book_id"`
	UUID             string  `json:"uuid"`
	TodoUUID         string  `json:"todo_uuid"`
	DeletedAt        *string `json:"deleted_at,omitempty"`
	ActorUserID      int     `json:"actor_user_id"`
	SourceType       string  `json:"source_type,omitempty"`
	SourceName       string  `json:"source_name,omitempty"`
	ActorDisplayName string  `json:"actor_display_name,omitempty"`
	Content          string  `json:"content"`
}

func buildSyncPayload(record Record) syncPayload {
	return buildSyncBatchPayload(SyncBatch{StatusLogs: []Record{record}})
}

type syncAITaskRun struct {
	UUID                string  `json:"uuid"`
	UserID              int     `json:"user_id"`
	BookID              int     `json:"book_id"`
	ProjectType         string  `json:"project_type"`
	ProjectUUID         string  `json:"project_uuid"`
	TodoUUID            string  `json:"todo_uuid"`
	ParentRunUUID       string  `json:"parent_run_uuid"`
	AgentName           string  `json:"agent_name"`
	Status              string  `json:"status"`
	Title               string  `json:"title"`
	LatestPhase         string  `json:"latest_phase"`
	LatestSummary       string  `json:"latest_summary"`
	ContextSnapshotJSON string  `json:"context_snapshot_json"`
	StartedAt           *string `json:"started_at,omitempty"`
	LastHeartbeatAt     *string `json:"last_heartbeat_at,omitempty"`
	CompletedAt         *string `json:"completed_at,omitempty"`
	DeletedAt           *string `json:"deleted_at,omitempty"`
	CreatedAt           string  `json:"created_at"`
	UpdatedAt           string  `json:"updated_at"`
	ClientCreatedAt     string  `json:"client_created_at"`
	ClientUpdatedAt     string  `json:"client_updated_at"`
}

type syncAITaskEvent struct {
	UUID               string `json:"uuid"`
	UserID             int    `json:"user_id"`
	BookID             int    `json:"book_id"`
	RunUUID            string `json:"run_uuid"`
	TodoUUID           string `json:"todo_uuid"`
	EventType          string `json:"event_type"`
	Severity           string `json:"severity"`
	Title              string `json:"title"`
	Content            string `json:"content"`
	MetadataJSON       string `json:"metadata_json"`
	PayloadHashVersion int    `json:"payload_hash_version"`
	PayloadHash        string `json:"payload_hash"`
	OccurredAt         string `json:"occurred_at"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
	ClientCreatedAt    string `json:"client_created_at"`
	ClientUpdatedAt    string `json:"client_updated_at"`
}

type syncResponse struct {
	NotesServerNewer        []syncNote         `json:"notes_server_newer"`
	NotesOnlyOnServer       []syncNote         `json:"notes_only_on_server"`
	NotesConflicts          []syncNoteConflict `json:"notes_conflicts"`
	ServerTime              string             `json:"server_time"`
	TodosServerNewer        []syncTodo         `json:"todos_server_newer"`
	TodosOnlyOnServer       []syncTodo         `json:"todos_only_on_server"`
	TodoRepliesServerNewer  []syncTodoReply    `json:"todo_replies_server_newer"`
	TodoRepliesOnlyOnServer []syncTodoReply    `json:"todo_replies_only_on_server"`
	TodoListsServerNewer    []syncTodoList     `json:"todo_lists_server_newer"`
	TodoListsOnlyOnServer   []syncTodoList     `json:"todo_lists_only_on_server"`
	AITaskRunsServerNewer   []syncResponseRun  `json:"ai_task_runs_server_newer"`
	AITaskRunsOnlyOnServer  []syncResponseRun  `json:"ai_task_runs_only_on_server"`
	AITaskEventsAccepted    []string           `json:"ai_task_events_accepted"`
	AITaskEventsDuplicate   []string           `json:"ai_task_events_duplicate"`
	AITaskEventsRejected    []syncEventReject  `json:"ai_task_events_rejected"`
}

type syncNoteConflict struct {
	Current      syncNote `json:"current"`
	Attempted    syncNote `json:"attempted"`
	ConflictCopy syncNote `json:"conflict_copy"`
}

type syncResponseRun struct {
	UUID string `json:"uuid"`
}

type syncEventReject struct {
	UUID   string `json:"uuid"`
	Reason string `json:"reason"`
}

func buildSyncBatchPayload(batch SyncBatch) syncPayload {
	models := append([]string(nil), batch.Models...)
	if len(models) == 0 {
		if len(batch.Notes) > 0 {
			models = append(models, "notes")
		}
		if len(batch.StatusLogs) > 0 {
			models = append(models, "status_logs")
		}
		if len(batch.TodoLists) > 0 {
			models = append(models, "todo_lists")
		}
		if len(batch.Todos) > 0 {
			models = append(models, "todos")
		}
		if len(batch.TodoReplies) > 0 {
			models = append(models, "todo_replies")
		}
		if len(batch.AITaskRuns) > 0 {
			models = append(models, "ai_task_runs")
		}
		if len(batch.AITaskEvents) > 0 {
			models = append(models, "ai_task_events")
		}
	}
	if len(models) == 0 {
		models = []string{"notes", "status_logs", "todo_lists", "todos", "todo_replies", "ai_task_runs", "ai_task_events"}
	}
	lastSyncAtByModel := map[string]*string{}
	for _, model := range models {
		if model == "status_logs" {
			lastSyncAtByModel[model] = nil
			continue
		}
		if cursor, ok := batch.Cursors[model]; ok && !cursor.IsZero() {
			formatted := cursor.UTC().Format(time.RFC3339Nano)
			lastSyncAtByModel[model] = &formatted
		} else {
			lastSyncAtByModel[model] = nil
		}
	}

	statusLogs := make([]syncStatusLog, 0, len(batch.StatusLogs))
	for _, record := range batch.StatusLogs {
		statusLogs = append(statusLogs, syncStatusLog{
			UUID:      record.UUID,
			LoggedAt:  record.LoggedAt.UTC().Format(time.RFC3339Nano),
			CreatedAt: record.CreatedAt.UTC().Format(time.RFC3339Nano),
			UpdatedAt: record.UpdatedAt.UTC().Format(time.RFC3339Nano),
			UserID:    0,
			LogType:   record.LogType,
			Content:   record.Content,
		})
	}

	notes := make([]syncNote, 0, len(batch.Notes))
	for _, note := range batch.Notes {
		notes = append(notes, noteToSync(note))
	}

	todoLists := make([]syncTodoList, 0, len(batch.TodoLists))
	for _, list := range batch.TodoLists {
		todoLists = append(todoLists, todoListToSync(list))
	}

	todos := make([]syncTodo, 0, len(batch.Todos))
	for _, todo := range batch.Todos {
		todos = append(todos, todoToSync(todo))
	}

	replies := make([]syncTodoReply, 0, len(batch.TodoReplies))
	for _, reply := range batch.TodoReplies {
		replies = append(replies, todoReplyToSync(reply))
	}

	runs := make([]syncAITaskRun, 0, len(batch.AITaskRuns))
	for _, run := range batch.AITaskRuns {
		runs = append(runs, syncAITaskRun{
			UUID:                run.UUID,
			UserID:              0,
			BookID:              0,
			ProjectType:         run.ProjectType,
			ProjectUUID:         run.ProjectUUID,
			TodoUUID:            run.TodoUUID,
			ParentRunUUID:       run.ParentRunUUID,
			AgentName:           run.AgentName,
			Status:              run.Status,
			Title:               run.Title,
			LatestPhase:         run.LatestPhase,
			LatestSummary:       run.LatestSummary,
			ContextSnapshotJSON: emptyJSON(run.ContextSnapshotJSON),
			StartedAt:           timePtrString(run.StartedAt),
			LastHeartbeatAt:     timePtrString(run.LastHeartbeatAt),
			CompletedAt:         timePtrString(run.CompletedAt),
			DeletedAt:           timePtrString(run.DeletedAt),
			CreatedAt:           run.CreatedAt.UTC().Format(time.RFC3339Nano),
			UpdatedAt:           run.UpdatedAt.UTC().Format(time.RFC3339Nano),
			ClientCreatedAt:     run.ClientCreatedAt.UTC().Format(time.RFC3339Nano),
			ClientUpdatedAt:     run.ClientUpdatedAt.UTC().Format(time.RFC3339Nano),
		})
	}

	events := make([]syncAITaskEvent, 0, len(batch.AITaskEvents))
	for _, event := range batch.AITaskEvents {
		events = append(events, syncAITaskEvent{
			UUID:               event.UUID,
			UserID:             0,
			BookID:             0,
			RunUUID:            event.RunUUID,
			TodoUUID:           event.TodoUUID,
			EventType:          event.EventType,
			Severity:           event.Severity,
			Title:              event.Title,
			Content:            event.Content,
			MetadataJSON:       emptyJSON(event.MetadataJSON),
			PayloadHashVersion: event.PayloadHashVersion,
			PayloadHash:        event.PayloadHash,
			OccurredAt:         event.OccurredAt.UTC().Format(time.RFC3339Nano),
			CreatedAt:          event.CreatedAt.UTC().Format(time.RFC3339Nano),
			UpdatedAt:          event.UpdatedAt.UTC().Format(time.RFC3339Nano),
			ClientCreatedAt:    event.ClientCreatedAt.UTC().Format(time.RFC3339Nano),
			ClientUpdatedAt:    event.ClientUpdatedAt.UTC().Format(time.RFC3339Nano),
		})
	}

	return syncPayload{
		SyncModels:        models,
		LastSyncAtByModel: lastSyncAtByModel,
		Notes:             notes,
		Comments:          []any{},
		MoneyCurrencies:   []any{},
		MoneyTransactions: []any{},
		MoneyBooks:        []any{},
		MoneyBookMembers:  []any{},
		Goals:             []any{},
		GoalMilestones:    []any{},
		GoalCheckins:      []any{},
		Todos:             todos,
		TodoReplies:       replies,
		TodoLists:         todoLists,
		Events:            []any{},
		Pomodoros:         []any{},
		StatusLogs:        statusLogs,
		AITaskRuns:        runs,
		AITaskEvents:      events,
	}
}

func buildSyncAck(batch SyncBatch, resp syncResponse, detail string) SyncAck {
	serverTime := time.Now().UTC()
	if resp.ServerTime != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, resp.ServerTime); err == nil {
			serverTime = parsed.UTC()
		}
	}

	runConflicts := map[string]bool{}
	for _, run := range resp.AITaskRunsServerNewer {
		runConflicts[run.UUID] = true
	}
	ack := SyncAck{
		Models:               append([]string(nil), batch.Models...),
		NoteSynced:           []string{},
		NoteConflicts:        []NoteRecord{},
		NoteServerRows:       []NoteRecord{},
		NoteSyncETags:        map[string]string{},
		NoteSyncBasePaths:    map[string]string{},
		NoteConflictCopies:   map[string]NoteRecord{},
		NoteConflictAttempts: map[string]NoteRecord{},
		NoteErrors:           map[string]string{},
		StatusLogUUIDs:       make([]string, 0, len(batch.StatusLogs)),
		TodoSynced:           []string{},
		TodoServerRecords:    []TodoRecord{},
		TodoReplySynced:      []string{},
		TodoReplyServerRows:  []TodoReplyRecord{},
		TodoListSynced:       []string{},
		TodoListServerRows:   []TodoListRecord{},
		AITaskRunSynced:      []string{},
		AITaskEventSynced:    []string{},
		AITaskEventErrors:    map[string]string{},
		ServerTime:           serverTime,
		Detail:               detail,
	}
	if len(ack.Models) == 0 {
		ack.Models = append([]string(nil), buildSyncBatchPayload(batch).SyncModels...)
	}
	pendingNotes := map[string]NoteRecord{}
	for _, note := range batch.Notes {
		pendingNotes[note.UUID] = note
	}
	noteConflicts := map[string]bool{}
	for _, conflict := range resp.NotesConflicts {
		current := noteFromSync(conflict.Current)
		if _, pending := pendingNotes[current.UUID]; !pending || current.UUID == "" {
			continue
		}
		noteConflicts[current.UUID] = true
		ack.NoteConflicts = append(ack.NoteConflicts, current)
		if conflict.Attempted.UUID == current.UUID {
			ack.NoteConflictAttempts[current.UUID] = noteFromSync(conflict.Attempted)
		} else {
			ack.NoteConflictAttempts[current.UUID] = pendingNotes[current.UUID]
		}
		if conflict.ConflictCopy.UUID != "" {
			ack.NoteConflictCopies[current.UUID] = noteFromSync(conflict.ConflictCopy)
		}
	}
	for _, note := range append(append([]syncNote(nil), resp.NotesServerNewer...), resp.NotesOnlyOnServer...) {
		if noteConflicts[note.UUID] {
			continue
		}
		remote := noteFromSync(note)
		if attempted, pending := pendingNotes[note.UUID]; pending {
			if remote.Path == "" {
				remote.Path = attempted.Path
			}
			if noteStateHash(remote.Content, remote.DeletedAt, remote.Path) == noteStateHash(attempted.Content, attempted.DeletedAt, attempted.Path) {
				ack.NoteSyncETags[note.UUID] = remote.SyncETag
				ack.NoteSyncBasePaths[note.UUID] = remote.SyncBasePath
				continue
			}
			noteConflicts[note.UUID] = true
			ack.NoteConflicts = append(ack.NoteConflicts, remote)
			continue
		}
		ack.NoteServerRows = append(ack.NoteServerRows, remote)
	}
	for _, note := range batch.Notes {
		if !noteConflicts[note.UUID] {
			if (managedNotePath(note.Path) || managedNotePath(note.SyncBasePath)) && (ack.NoteSyncETags[note.UUID] == "" || ack.NoteSyncBasePaths[note.UUID] != note.Path) {
				ack.NoteErrors[note.UUID] = "managed note requires an explicit server acknowledgement with sync_etag and current sync_base_path"
				continue
			}
			ack.NoteSynced = append(ack.NoteSynced, note.UUID)
		}
	}
	for _, record := range batch.StatusLogs {
		ack.StatusLogUUIDs = append(ack.StatusLogUUIDs, record.UUID)
	}

	pendingLists := map[string]bool{}
	for _, list := range batch.TodoLists {
		pendingLists[list.UUID] = true
	}
	listConflicts := map[string]bool{}
	for _, list := range resp.TodoListsServerNewer {
		if pendingLists[list.UUID] {
			listConflicts[list.UUID] = true
			ack.TodoListConflicts = append(ack.TodoListConflicts, list.UUID)
			continue
		}
		ack.TodoListServerRows = append(ack.TodoListServerRows, todoListFromSync(list, serverTime))
	}
	for _, list := range resp.TodoListsOnlyOnServer {
		if !pendingLists[list.UUID] {
			ack.TodoListServerRows = append(ack.TodoListServerRows, todoListFromSync(list, serverTime))
		}
	}
	for _, list := range batch.TodoLists {
		if !listConflicts[list.UUID] {
			ack.TodoListSynced = append(ack.TodoListSynced, list.UUID)
		}
	}

	pendingTodos := map[string]bool{}
	for _, todo := range batch.Todos {
		pendingTodos[todo.UUID] = true
	}
	todoConflicts := map[string]bool{}
	for _, todo := range resp.TodosServerNewer {
		if pendingTodos[todo.UUID] {
			todoConflicts[todo.UUID] = true
			ack.TodoConflicts = append(ack.TodoConflicts, todo.UUID)
			continue
		}
		ack.TodoServerRecords = append(ack.TodoServerRecords, todoFromSync(todo, serverTime))
	}
	for _, todo := range resp.TodosOnlyOnServer {
		if !pendingTodos[todo.UUID] {
			ack.TodoServerRecords = append(ack.TodoServerRecords, todoFromSync(todo, serverTime))
		}
	}
	for _, todo := range batch.Todos {
		if !todoConflicts[todo.UUID] {
			ack.TodoSynced = append(ack.TodoSynced, todo.UUID)
		}
	}

	pendingReplies := map[string]bool{}
	for _, reply := range batch.TodoReplies {
		pendingReplies[reply.UUID] = true
	}
	replyConflicts := map[string]bool{}
	for _, reply := range resp.TodoRepliesServerNewer {
		if pendingReplies[reply.UUID] {
			replyConflicts[reply.UUID] = true
			ack.TodoReplyConflicts = append(ack.TodoReplyConflicts, reply.UUID)
			continue
		}
		ack.TodoReplyServerRows = append(ack.TodoReplyServerRows, todoReplyFromSync(reply, serverTime))
	}
	for _, reply := range resp.TodoRepliesOnlyOnServer {
		if !pendingReplies[reply.UUID] {
			ack.TodoReplyServerRows = append(ack.TodoReplyServerRows, todoReplyFromSync(reply, serverTime))
		}
	}
	for _, reply := range batch.TodoReplies {
		if !replyConflicts[reply.UUID] {
			ack.TodoReplySynced = append(ack.TodoReplySynced, reply.UUID)
		}
	}

	for _, run := range batch.AITaskRuns {
		if runConflicts[run.UUID] {
			ack.AITaskRunConflicts = append(ack.AITaskRunConflicts, run.UUID)
			continue
		}
		ack.AITaskRunSynced = append(ack.AITaskRunSynced, run.UUID)
	}

	eventOK := map[string]bool{}
	for _, uuid := range resp.AITaskEventsAccepted {
		eventOK[uuid] = true
	}
	for _, uuid := range resp.AITaskEventsDuplicate {
		eventOK[uuid] = true
	}
	for _, reject := range resp.AITaskEventsRejected {
		ack.AITaskEventErrors[reject.UUID] = reject.Reason
	}
	for _, event := range batch.AITaskEvents {
		switch {
		case eventOK[event.UUID]:
			ack.AITaskEventSynced = append(ack.AITaskEventSynced, event.UUID)
		case ack.AITaskEventErrors[event.UUID] != "":
			// Keep pending with the server-provided reason.
		default:
			ack.AITaskEventErrors[event.UUID] = "missing event ack"
		}
	}
	return ack
}

func noteToSync(note NoteRecord) syncNote {
	return syncNote{
		CreatedAt:    note.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:    note.UpdatedAt.UTC().Format(time.RFC3339Nano),
		UserID:       0,
		UUID:         note.UUID,
		DeletedAt:    timePtrString(note.DeletedAt),
		Path:         note.Path,
		Content:      note.Content,
		SyncETag:     note.SyncETag,
		SyncBasePath: note.SyncBasePath,
	}
}

func noteFromSync(note syncNote) NoteRecord {
	now := time.Now().UTC()
	return NoteRecord{
		UUID:         note.UUID,
		Path:         note.Path,
		Content:      note.Content,
		DeletedAt:    parseSyncTimePtr(note.DeletedAt),
		CreatedAt:    parseSyncTimeOrNow(note.CreatedAt, now),
		UpdatedAt:    parseSyncTimeOrNow(note.UpdatedAt, now),
		SyncETag:     note.SyncETag,
		SyncBasePath: note.SyncBasePath,
	}
}

func todoToSync(todo TodoRecord) syncTodo {
	return syncTodo{
		CreatedAt:           todo.ClientCreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:           todo.ClientUpdatedAt.UTC().Format(time.RFC3339Nano),
		UserID:              0,
		UUID:                todo.UUID,
		ParentUUID:          todo.ParentUUID,
		DeletedAt:           timePtrString(todo.DeletedAt),
		ArchivedAt:          timePtrString(todo.ArchivedAt),
		Deadline:            timePtrString(todo.Deadline),
		Content:             todo.Content,
		Notes:               todo.Notes,
		Completed:           todo.Completed,
		Order:               todo.SortOrder,
		GoalUUID:            optionalString(todo.GoalUUID),
		MilestoneUUID:       optionalString(todo.MilestoneUUID),
		TaskRole:            todo.TaskRole,
		TodoSource:          todo.TodoSource,
		CompletionMode:      defaultString(todo.CompletionMode, "manual"),
		AIEvaluationStatus:  defaultString(todo.AIEvaluationStatus, "not_requested"),
		AICompletionSummary: todo.AICompletionSummary,
		CompletedAt:         timePtrString(todo.CompletedAt),
		CompletionSource:    defaultString(todo.CompletionSource, "manual"),
		ListUUID:            optionalString(todo.ListUUID),
		CategoryUUID:        optionalString(todo.CategoryUUID),
	}
}

func todoFromSync(todo syncTodo, syncedAt time.Time) TodoRecord {
	createdAt := parseSyncTimeOrNow(todo.CreatedAt, syncedAt)
	updatedAt := parseSyncTimeOrNow(todo.UpdatedAt, syncedAt)
	return TodoRecord{
		UUID:                todo.UUID,
		ParentUUID:          todo.ParentUUID,
		Content:             todo.Content,
		Notes:               todo.Notes,
		Completed:           todo.Completed,
		SortOrder:           todo.Order,
		ListUUID:            derefString(todo.ListUUID),
		CompletedAt:         parseSyncTimePtr(todo.CompletedAt),
		DeletedAt:           parseSyncTimePtr(todo.DeletedAt),
		ArchivedAt:          parseSyncTimePtr(todo.ArchivedAt),
		Deadline:            parseSyncTimePtr(todo.Deadline),
		GoalUUID:            derefString(todo.GoalUUID),
		MilestoneUUID:       derefString(todo.MilestoneUUID),
		CategoryUUID:        derefString(todo.CategoryUUID),
		TaskRole:            todo.TaskRole,
		TodoSource:          todo.TodoSource,
		CompletionMode:      defaultString(todo.CompletionMode, "manual"),
		CompletionSource:    defaultString(todo.CompletionSource, "manual"),
		AIEvaluationStatus:  defaultString(todo.AIEvaluationStatus, "not_requested"),
		AICompletionSummary: todo.AICompletionSummary,
		CreatedAt:           createdAt,
		UpdatedAt:           updatedAt,
		ClientCreatedAt:     createdAt,
		ClientUpdatedAt:     updatedAt,
		SyncedAt:            &syncedAt,
	}
}

func todoReplyToSync(reply TodoReplyRecord) syncTodoReply {
	return syncTodoReply{
		CreatedAt:        reply.ClientCreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:        reply.ClientUpdatedAt.UTC().Format(time.RFC3339Nano),
		UserID:           0,
		BookID:           0,
		UUID:             reply.UUID,
		TodoUUID:         reply.TodoUUID,
		DeletedAt:        timePtrString(reply.DeletedAt),
		ActorUserID:      0,
		SourceType:       defaultString(reply.SourceType, "api_token"),
		SourceName:       reply.SourceName,
		ActorDisplayName: reply.ActorDisplayName,
		Content:          reply.Content,
	}
}

func todoReplyFromSync(reply syncTodoReply, syncedAt time.Time) TodoReplyRecord {
	createdAt := parseSyncTimeOrNow(reply.CreatedAt, syncedAt)
	updatedAt := parseSyncTimeOrNow(reply.UpdatedAt, syncedAt)
	return TodoReplyRecord{
		UUID:             reply.UUID,
		TodoUUID:         reply.TodoUUID,
		Content:          reply.Content,
		DeletedAt:        parseSyncTimePtr(reply.DeletedAt),
		SourceType:       reply.SourceType,
		SourceName:       reply.SourceName,
		ActorDisplayName: reply.ActorDisplayName,
		CreatedAt:        createdAt,
		UpdatedAt:        updatedAt,
		ClientCreatedAt:  createdAt,
		ClientUpdatedAt:  updatedAt,
		SyncedAt:         &syncedAt,
	}
}

func todoListToSync(list TodoListRecord) syncTodoList {
	return syncTodoList{
		CreatedAt: list.ClientCreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt: list.ClientUpdatedAt.UTC().Format(time.RFC3339Nano),
		UserID:    0,
		UUID:      list.UUID,
		DeletedAt: timePtrString(list.DeletedAt),
		Name:      list.Name,
		Color:     list.Color,
		Icon:      list.Icon,
		SortOrder: list.SortOrder,
	}
}

func todoListFromSync(list syncTodoList, syncedAt time.Time) TodoListRecord {
	createdAt := parseSyncTimeOrNow(list.CreatedAt, syncedAt)
	updatedAt := parseSyncTimeOrNow(list.UpdatedAt, syncedAt)
	return TodoListRecord{
		UUID:            list.UUID,
		Name:            list.Name,
		Color:           list.Color,
		Icon:            list.Icon,
		SortOrder:       list.SortOrder,
		DeletedAt:       parseSyncTimePtr(list.DeletedAt),
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
		ClientCreatedAt: createdAt,
		ClientUpdatedAt: updatedAt,
		SyncedAt:        &syncedAt,
	}
}

func timePtrString(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func optionalString(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	v := value
	return &v
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func parseSyncTimePtr(value *string) *time.Time {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, *value)
	if err != nil {
		return nil
	}
	return &parsed
}

func parseSyncTimeOrNow(value string, fallback time.Time) time.Time {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return fallback
	}
	return parsed
}
