package integlife

import "time"

type Record struct {
	UUID          string
	LogType       string
	Content       string
	LoggedAt      time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	SyncedAt      *time.Time
	LastSyncError string
}

type TodoRecord struct {
	UUID                string
	ParentUUID          string
	Content             string
	Notes               string
	Completed           bool
	SortOrder           float64
	ListUUID            string
	CompletedAt         *time.Time
	DeletedAt           *time.Time
	ArchivedAt          *time.Time
	Deadline            *time.Time
	GoalUUID            string
	MilestoneUUID       string
	CategoryUUID        string
	TaskRole            string
	TodoSource          string
	CompletionMode      string
	CompletionSource    string
	AIEvaluationStatus  string
	AICompletionSummary string
	CreatedAt           time.Time
	UpdatedAt           time.Time
	ClientCreatedAt     time.Time
	ClientUpdatedAt     time.Time
	SyncedAt            *time.Time
	LastSyncError       string
}

type TodoListRecord struct {
	UUID            string
	Name            string
	Color           string
	Icon            string
	SortOrder       int
	DeletedAt       *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ClientCreatedAt time.Time
	ClientUpdatedAt time.Time
	SyncedAt        *time.Time
	LastSyncError   string
}

type TodoReplyRecord struct {
	UUID             string
	TodoUUID         string
	Content          string
	DeletedAt        *time.Time
	SourceType       string
	SourceName       string
	ActorDisplayName string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	ClientCreatedAt  time.Time
	ClientUpdatedAt  time.Time
	SyncedAt         *time.Time
	LastSyncError    string
}

type SyncFailure struct {
	UUID   string
	Detail string
}

type SyncResult struct {
	Synced   int
	Pending  int
	Failures []SyncFailure
}

type NoteRecord struct {
	UUID               string
	Path               string
	Content            string
	DeletedAt          *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	SyncedAt           *time.Time
	SyncedContentHash  string
	SyncETag           string
	SyncBasePath       string
	RemoteContent      string
	RemotePath         string
	RemoteSyncETag     string
	RemoteSyncBasePath string
	ConflictCopyUUID   string
	ConflictCopyPath   string
	ConflictAttempt    *NoteRecord
	RemoteUpdatedAt    *time.Time
	RemoteDeletedAt    *time.Time
	LastSyncError      string
}

type AITaskRunRecord struct {
	UUID                string
	ProjectType         string
	ProjectUUID         string
	TodoUUID            string
	ParentRunUUID       string
	AgentName           string
	Status              string
	Title               string
	LatestPhase         string
	LatestSummary       string
	ContextSnapshotJSON string
	StartedAt           *time.Time
	LastHeartbeatAt     *time.Time
	CompletedAt         *time.Time
	DeletedAt           *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
	ClientCreatedAt     time.Time
	ClientUpdatedAt     time.Time
	SyncedAt            *time.Time
	LastSyncError       string
}

type AITaskEventRecord struct {
	UUID               string
	RunUUID            string
	TodoUUID           string
	EventType          string
	Severity           string
	Title              string
	Content            string
	MetadataJSON       string
	PayloadHashVersion int
	PayloadHash        string
	OccurredAt         time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	ClientCreatedAt    time.Time
	ClientUpdatedAt    time.Time
	SyncedAt           *time.Time
	LastSyncError      string
}

type ActiveRunRecord struct {
	ScopeKey    string
	SessionID   string
	RunUUID     string
	CWD         string
	ProjectType string
	ProjectUUID string
	TodoUUID    string
	AgentName   string
	UpdatedAt   time.Time
}

type SyncCursor struct {
	Model     string
	CursorAt  time.Time
	UpdatedAt time.Time
}

type SyncBatch struct {
	Models       []string
	Notes        []NoteRecord
	StatusLogs   []Record
	Todos        []TodoRecord
	TodoLists    []TodoListRecord
	TodoReplies  []TodoReplyRecord
	AITaskRuns   []AITaskRunRecord
	AITaskEvents []AITaskEventRecord
	Cursors      map[string]time.Time
}

type SyncAck struct {
	Models               []string
	NoteSynced           []string
	NoteConflicts        []NoteRecord
	NoteServerRows       []NoteRecord
	NoteSyncETags        map[string]string
	NoteSyncBasePaths    map[string]string
	NoteConflictCopies   map[string]NoteRecord
	NoteConflictAttempts map[string]NoteRecord
	NoteErrors           map[string]string
	StatusLogUUIDs       []string
	TodoSynced           []string
	TodoConflicts        []string
	TodoServerRecords    []TodoRecord
	TodoReplySynced      []string
	TodoReplyConflicts   []string
	TodoReplyServerRows  []TodoReplyRecord
	TodoListSynced       []string
	TodoListConflicts    []string
	TodoListServerRows   []TodoListRecord
	AITaskRunSynced      []string
	AITaskRunConflicts   []string
	AITaskEventSynced    []string
	AITaskEventErrors    map[string]string
	ServerTime           time.Time
	Detail               string
}
