package integlife

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const aiBridgeTestRun = "run-recovery"
const aiBridgeTestDecision = "approve-release"
const aiBridgeTestMessage = "life-a24edaf18a03e82ac428cccc90142e84"

type aiBridgeTestPayload struct {
	ID        string `json:"id"`
	Channel   string `json:"channel"`
	Topic     string `json:"topic"`
	Level     string `json:"level"`
	Title     string `json:"title"`
	Ref       string `json:"ref"`
	ExpiresAt string `json:"expires_at"`
	Reply     struct {
		Type      string     `json:"type"`
		Options   []AIOption `json:"options"`
		AllowText bool       `json:"allow_text"`
	} `json:"reply"`
}

// Servers retain state while clients are replaced to model a stopped runner.
// No local database or real user account is involved.
type aiBridgeTestFixture struct {
	mu                 sync.Mutex
	life               *httptest.Server
	bridge             *httptest.Server
	checkpoint         AIContext
	contextResponse    func(AIContext) any
	contextPutResponse func(AIContext) any
	decision           *AIDecision
	message            *BridgeMessage
	payload            aiBridgeTestPayload
	events             []string
	lifeRequests       int
	bridgeRequests     int
	creates            int
	submits            int
	imports            int
	consumes           int
	answerSource       string
	bridgeStatus       int
	bridgePostStatus   int
	bridgeDelay        time.Duration
}

func newAIBridgeTestFixture(t *testing.T) *aiBridgeTestFixture {
	t.Helper()
	f := &aiBridgeTestFixture{checkpoint: AIContext{
		Run:      AIContextRun{UUID: aiBridgeTestRun, TodoUUID: "todo-recovery", Title: "Release update", AgentName: "codex", Status: "waiting"},
		Revision: "revision-1",
		Checkpoint: AICheckpoint{
			SchemaVersion: 1, Goal: "Release the approved update", Summary: "Build passed", NextStep: "Apply the selected release action",
			Constraints: []string{"Do not change unrelated files"}, AcceptanceCriteria: []string{"Verify the served release"},
			SourceRefs:       []AIContextRef{{ID: "note-release", Title: "Release plan", URL: "https://app.integ.life/notes/note-release", Version: "2"}},
			ArtifactRefs:     []AIContextRef{{ID: "build-17", Title: "Build", URL: "https://example.org/build-17", Version: "17"}},
			RunnerSessionRef: "codex-session-42",
		},
	}}
	f.life = httptest.NewServer(http.HandlerFunc(f.serveLife))
	f.bridge = httptest.NewServer(http.HandlerFunc(f.serveBridge))
	t.Cleanup(f.bridge.Close)
	t.Cleanup(f.life.Close)
	return f
}

func aiBridgeWriteJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func (f *aiBridgeTestFixture) serveLife(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lifeRequests++
	f.events = append(f.events, "Life "+r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer life-test-token" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	base := "/api/ai-task-runs/" + aiBridgeTestRun
	decisionPath := base + "/decisions/" + aiBridgeTestDecision
	switch {
	case r.Method == http.MethodGet && r.URL.Path == base+"/context":
		if f.contextResponse != nil {
			aiBridgeWriteJSON(w, f.contextResponse(f.checkpoint))
		} else {
			aiBridgeWriteJSON(w, f.checkpoint)
		}
	case r.Method == http.MethodPut && r.URL.Path == base+"/context":
		var request struct {
			ExpectedRevision string       `json:"expected_revision"`
			Checkpoint       AICheckpoint `json:"checkpoint"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.ExpectedRevision != f.checkpoint.Revision {
			http.Error(w, "revision conflict", http.StatusConflict)
			return
		}
		f.checkpoint.Checkpoint = request.Checkpoint
		f.checkpoint.Revision = "revision-2"
		if f.contextPutResponse != nil {
			aiBridgeWriteJSON(w, f.contextPutResponse(f.checkpoint))
		} else {
			aiBridgeWriteJSON(w, f.checkpoint)
		}
	case r.Method == http.MethodGet && r.URL.Path == decisionPath:
		if f.decision == nil {
			http.NotFound(w, r)
			return
		}
		aiBridgeWriteJSON(w, AIDecisionResponse{Decision: *f.decision})
	case r.Method == http.MethodPost && r.URL.Path == base+"/decisions":
		var request CreateAIDecision
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.ExpectedRevision != f.checkpoint.Revision || request.ID != aiBridgeTestDecision {
			http.Error(w, "invalid decision", http.StatusConflict)
			return
		}
		f.creates++
		f.decision = &AIDecision{
			ID: request.ID, RunUUID: aiBridgeTestRun, CheckpointRevision: request.ExpectedRevision, Question: request.Question,
			Options: request.Options, BridgeMessageID: request.BridgeMessageID, ExpiresAt: request.ExpiresAt, Status: "pending",
		}
		aiBridgeWriteJSON(w, AIDecisionResponse{Decision: *f.decision})
	case r.Method == http.MethodPost && r.URL.Path == decisionPath+"/answer":
		var request struct {
			OptionID string `json:"option_id"`
			Text     string `json:"text"`
			Source   string `json:"source"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || f.decision == nil {
			http.Error(w, "invalid answer", http.StatusBadRequest)
			return
		}
		f.imports++
		f.answerSource = request.Source
		f.decision.Status = "answered"
		f.decision.Answer = &AIAnswer{OptionID: request.OptionID, Text: request.Text, SourceType: request.Source, SourceID: "bridge-reply-1"}
		aiBridgeWriteJSON(w, AIDecisionResponse{Decision: *f.decision})
	case r.Method == http.MethodPost && r.URL.Path == decisionPath+"/consume":
		f.consumes++
		var request struct {
			OperationKey string `json:"operation_key"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.OperationKey == "" || f.decision == nil || f.decision.Answer == nil || (f.decision.Status != "consumed" && f.decision.CheckpointRevision != f.checkpoint.Revision) {
			http.Error(w, "cannot consume", http.StatusConflict)
			return
		}
		first := f.decision.Status == "answered"
		if !first && f.decision.OperationKey != request.OperationKey {
			http.Error(w, "operation conflict", http.StatusConflict)
			return
		}
		f.decision.Status = "consumed"
		f.decision.OperationKey = request.OperationKey
		aiBridgeWriteJSON(w, AIConsumeResponse{Decision: *f.decision, Consumed: first})
	default:
		http.NotFound(w, r)
	}
}

func (f *aiBridgeTestFixture) serveBridge(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.bridgeRequests++
	f.events = append(f.events, "Bridge "+r.Method+" "+r.URL.Path)
	delay := f.bridgeDelay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(delay):
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer bridge-test-token" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if f.bridgeStatus != 0 {
		http.Error(w, "temporary transport error", f.bridgeStatus)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/messages/"+aiBridgeTestMessage:
		if f.message == nil {
			http.NotFound(w, r)
			return
		}
		aiBridgeWriteJSON(w, *f.message)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		if json.NewDecoder(r.Body).Decode(&f.payload) != nil || f.payload.ID != aiBridgeTestMessage {
			http.Error(w, "invalid message", http.StatusBadRequest)
			return
		}
		f.submits++
		if f.bridgePostStatus != 0 {
			http.Error(w, "temporary submit error", f.bridgePostStatus)
			return
		}
		f.message = &BridgeMessage{ID: f.payload.ID, Status: "queued"}
		aiBridgeWriteJSON(w, *f.message)
	default:
		http.NotFound(w, r)
	}
}

func (f *aiBridgeTestFixture) newProcess() (*Service, *BridgeClient) {
	return NewService(nil, NewClient(f.life.URL, "life-test-token", time.Second)), NewBridgeClient(f.bridge.URL, "bridge-test-token", time.Second)
}

func aiBridgeTestAskOptions() AIAskOptions {
	return AIAskOptions{RunUUID: aiBridgeTestRun, ID: aiBridgeTestDecision, Question: "Release the verified build?", Options: []AIOption{{ID: "release", Label: "Release"}, {ID: "hold", Label: "Hold"}}, ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)}
}

func TestAIBridgeRecoveryAndConsumptionReplay(t *testing.T) {
	f := newAIBridgeTestFixture(t)
	options := aiBridgeTestAskOptions()
	service, bridge := f.newProcess()
	first, err := service.AskAITask(context.Background(), options, bridge)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ServerAccepted || first.Decision.Status != "pending" || first.BridgeState != "queued" || first.Handoff.ShouldExecute {
		t.Fatalf("unexpected initial durable ask: %+v", first)
	}
	if first.Decision.BridgeMessageID != aiBridgeTestMessage {
		t.Fatalf("message id changed: %q", first.Decision.BridgeMessageID)
	}
	f.mu.Lock()
	wantEvents := []string{
		"Life GET /api/ai-task-runs/run-recovery/context",
		"Life GET /api/ai-task-runs/run-recovery/decisions/approve-release",
		"Life POST /api/ai-task-runs/run-recovery/decisions",
		"Bridge GET /v1/messages/" + aiBridgeTestMessage,
		"Bridge POST /v1/messages",
	}
	if !reflect.DeepEqual(f.events, wantEvents) {
		t.Errorf("must persist first and GET before submit: got %v", f.events)
	}
	if f.payload.Ref != "https://app.integ.life/#/todo/todo-recovery?run=run-recovery" || f.payload.Channel != "work" || f.payload.Topic != aiBridgeTestRun || f.payload.Level != "push" || f.payload.Title != options.Question || f.payload.ExpiresAt != options.ExpiresAt || f.payload.Reply.Type != "choice" || !reflect.DeepEqual(f.payload.Reply.Options, options.Options) {
		t.Errorf("incomplete Bridge message: %+v", f.payload)
	}
	// A phone reply arrives while the runner process is absent.
	f.message.Status = "answered"
	f.message.Reply = &BridgeReply{ID: "bridge-reply-1", OptionID: "release", Text: "Use the verified build"}
	f.mu.Unlock()

	service, bridge = f.newProcess()
	recovered, err := service.AskAITask(context.Background(), options, bridge)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Decision.Status != "answered" || recovered.Handoff.ShouldExecute || recovered.Handoff.RunnerSessionRef != "codex-session-42" || recovered.Handoff.Checkpoint.NextStep != first.Handoff.Checkpoint.NextStep {
		t.Fatalf("reply did not recover the original checkpoint: %+v", recovered)
	}
	if !reflect.DeepEqual(recovered.Handoff.Checkpoint.SourceRefs, first.Handoff.Checkpoint.SourceRefs) || !reflect.DeepEqual(recovered.Handoff.RecoveryArgs, []string{"life", "ai", "decision", "--run", aiBridgeTestRun, "--id", aiBridgeTestDecision, "--json"}) {
		t.Fatalf("portable recovery handoff changed: %+v", recovered.Handoff)
	}

	sideEffects := 0
	claimed, err := service.DecisionAITask(context.Background(), aiBridgeTestRun, aiBridgeTestDecision, "release-build-17")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ShouldExecute {
		sideEffects++
	}
	// Reconciliation remains possible after the task moves on and the old
	// proposal expires. Its already-consumed operation must not execute again.
	f.mu.Lock()
	f.checkpoint.Revision = "revision-after-consumption"
	f.decision.ExpiresAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	f.mu.Unlock()
	service, _ = f.newProcess()
	replayed, err := service.DecisionAITask(context.Background(), aiBridgeTestRun, aiBridgeTestDecision, "release-build-17")
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ShouldExecute {
		sideEffects++
	}
	if !claimed.Consumed || !claimed.ShouldExecute || !claimed.Handoff.ShouldExecute || replayed.Consumed || replayed.ShouldExecute || replayed.Handoff.ShouldExecute || sideEffects != 1 {
		t.Fatalf("operation was not claimed exactly once: first=%+v replay=%+v effects=%d", claimed, replayed, sideEffects)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 1 || f.submits != 1 || f.imports != 1 || f.answerSource != "bridge_transport" || f.consumes != 2 {
		t.Fatalf("recovery changed durable identities or provenance: creates=%d submits=%d imports=%d source=%q consumes=%d", f.creates, f.submits, f.imports, f.answerSource, f.consumes)
	}
}

func TestAIBridgeMissingTokenCannotSubmit(t *testing.T) {
	f := newAIBridgeTestFixture(t)
	service := NewService(nil, NewClient(f.life.URL, "", time.Second))
	bridge := NewBridgeClient(f.bridge.URL, "bridge-test-token", time.Second)
	result, err := service.AskAITask(context.Background(), aiBridgeTestAskOptions(), bridge)
	if err == nil || result.ServerAccepted {
		t.Fatalf("missing Life token was accepted: result=%+v err=%v", result, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lifeRequests != 0 || f.bridgeRequests != 0 {
		t.Fatalf("unauthenticated ask reached a server: life=%d bridge=%d", f.lifeRequests, f.bridgeRequests)
	}
}

func TestAIBridgeDecisionCommandRecoversAndClaimsReply(t *testing.T) {
	f := newAIBridgeTestFixture(t)
	service, bridge := f.newProcess()
	first, err := service.AskAITask(context.Background(), aiBridgeTestAskOptions(), bridge)
	if err != nil {
		t.Fatal(err)
	}
	if first.Decision.Status != "pending" || first.BridgeState != "queued" {
		t.Fatalf("original ask was not queued: %+v", first)
	}
	f.mu.Lock()
	f.message.Status = "answered"
	f.message.Reply = &BridgeReply{ID: "bridge-reply-1", OptionID: "release", Text: "Continue after restarting"}
	f.mu.Unlock()

	service, bridge = f.newProcess()
	recovered, err := service.DecisionAITask(context.Background(), aiBridgeTestRun, aiBridgeTestDecision, "release-after-restart", bridge)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.ServerAccepted || !recovered.Consumed || !recovered.ShouldExecute || !recovered.Handoff.ShouldExecute || recovered.Decision.Status != "consumed" || recovered.Decision.Answer == nil || recovered.Decision.Answer.OptionID != "release" {
		t.Fatalf("decision read did not import and claim the saved reply: %+v", recovered)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 1 || f.submits != 1 || f.imports != 1 || f.answerSource != "bridge_transport" || f.consumes != 1 {
		t.Fatalf("decision recovery repeated submission or lost provenance: creates=%d submits=%d imports=%d source=%q consumes=%d", f.creates, f.submits, f.imports, f.answerSource, f.consumes)
	}
	bridgeGetCount := 0
	for _, event := range f.events {
		if event == "Bridge GET /v1/messages/"+aiBridgeTestMessage {
			bridgeGetCount++
		}
	}
	if bridgeGetCount != 2 {
		t.Fatalf("recovery did not read the original message: %v", f.events)
	}
}

func TestAIBridgeDecisionReadNeverResubmitsFailedMessage(t *testing.T) {
	f := newAIBridgeTestFixture(t)
	f.bridgePostStatus = http.StatusServiceUnavailable
	service, bridge := f.newProcess()
	first, err := service.AskAITask(context.Background(), aiBridgeTestAskOptions(), bridge)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ServerAccepted || first.Decision.Status != "pending" || first.BridgeState != "unavailable" {
		t.Fatalf("failed submit lost the durable pending decision: %+v", first)
	}
	f.mu.Lock()
	if f.submits != 1 || f.message != nil {
		t.Errorf("fixture did not simulate one failed initial POST: attempts=%d message=%+v", f.submits, f.message)
	}
	f.bridgePostStatus = 0
	f.mu.Unlock()

	service, bridge = f.newProcess()
	recovered, err := service.DecisionAITask(context.Background(), aiBridgeTestRun, aiBridgeTestDecision, "", bridge)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.ServerAccepted || recovered.Decision.Status != "pending" || recovered.ShouldExecute || recovered.BridgeState != "unavailable" {
		t.Fatalf("GET of missing Bridge message changed the pending decision: %+v", recovered)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submits != 1 || f.message != nil || f.imports != 0 || f.consumes != 0 || f.decision.Status != "pending" {
		t.Fatalf("decision read submitted a replacement: submits=%d message=%+v imports=%d consumes=%d decision=%+v", f.submits, f.message, f.imports, f.consumes, f.decision)
	}
	if f.events[len(f.events)-1] != "Bridge GET /v1/messages/"+aiBridgeTestMessage {
		t.Fatalf("decision read did not finish with the existing message GET: %v", f.events)
	}
}

func TestAIBridgeInvalidCheckpointCannotSubmit(t *testing.T) {
	cases := map[string]func(AIContext) any{
		"missing acknowledgement": func(AIContext) any { return map[string]any{} },
		"wrong run":               func(value AIContext) any { value.Run.UUID = "another-run"; return value },
		"missing revision":        func(value AIContext) any { value.Revision = ""; return value },
		"missing schema":          func(value AIContext) any { value.Checkpoint.SchemaVersion = 0; return value },
		"missing goal":            func(value AIContext) any { value.Checkpoint.Goal = ""; return value },
		"missing next step":       func(value AIContext) any { value.Checkpoint.NextStep = ""; return value },
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			f := newAIBridgeTestFixture(t)
			f.contextResponse = response
			service, bridge := f.newProcess()
			result, err := service.AskAITask(context.Background(), aiBridgeTestAskOptions(), bridge)
			if err == nil || result.ServerAccepted {
				t.Fatalf("invalid checkpoint was accepted: result=%+v err=%v", result, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.creates != 0 || f.bridgeRequests != 0 {
				t.Fatalf("invalid acknowledgement produced a decision or Bridge request: creates=%d bridge=%d", f.creates, f.bridgeRequests)
			}
		})
	}
}

func TestAIBridgeFailurePreservesPendingDecision(t *testing.T) {
	for _, name := range []string{"server error", "timeout"} {
		t.Run(name, func(t *testing.T) {
			f := newAIBridgeTestFixture(t)
			service, bridge := f.newProcess()
			if name == "server error" {
				f.bridgeStatus = http.StatusServiceUnavailable
			} else {
				f.bridgeDelay = 100 * time.Millisecond
				bridge.client.Timeout = 5 * time.Millisecond
			}
			result, err := service.AskAITask(context.Background(), aiBridgeTestAskOptions(), bridge)
			if err != nil {
				t.Fatal(err)
			}
			if !result.ServerAccepted || result.Decision.Status != "pending" || result.BridgeState != "unavailable" || result.BridgeError == "" || result.Handoff.ShouldExecute {
				t.Fatalf("Bridge failure changed durable acceptance: %+v", result)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.decision == nil || f.decision.Status != "pending" || f.imports != 0 || f.consumes != 0 {
				t.Fatalf("transport failure changed the persisted decision: decision=%+v imports=%d consumes=%d", f.decision, f.imports, f.consumes)
			}
		})
	}
}

func TestAIBridgeFiniteWaitBoundsTransportCall(t *testing.T) {
	f := newAIBridgeTestFixture(t)
	f.bridgeDelay = time.Second
	service, bridge := f.newProcess()
	options := aiBridgeTestAskOptions()
	options.Wait = 100 * time.Millisecond
	started := time.Now()
	result, err := service.AskAITask(context.Background(), options, bridge)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("finite wait must return the pending durable decision: %v", err)
	}
	if elapsed >= 700*time.Millisecond {
		t.Fatalf("100ms wait blocked on the 1s transport timeout: %v", elapsed)
	}
	if !result.ServerAccepted || result.Decision.Status != "pending" || result.Handoff.ShouldExecute {
		t.Fatalf("wait deadline authorized or discarded the decision: %+v", result)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.decision == nil || f.decision.Status != "pending" || f.submits != 0 || f.imports != 0 || f.consumes != 0 {
		t.Fatalf("wait deadline changed server state: decision=%+v submits=%d imports=%d consumes=%d", f.decision, f.submits, f.imports, f.consumes)
	}
}

func TestAIBridgeInvalidOptionIsNotImported(t *testing.T) {
	f := newAIBridgeTestFixture(t)
	f.message = &BridgeMessage{ID: aiBridgeTestMessage, Status: "answered", Reply: &BridgeReply{ID: "reply-invalid", OptionID: "release-everything"}}
	service, bridge := f.newProcess()
	result, err := service.AskAITask(context.Background(), aiBridgeTestAskOptions(), bridge)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision.Status != "pending" || result.BridgeState != "unavailable" || !strings.Contains(result.BridgeError, "option_id") || result.Handoff.ShouldExecute {
		t.Fatalf("invalid Bridge option was accepted: %+v", result)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.imports != 0 || f.consumes != 0 || f.decision.Answer != nil {
		t.Fatalf("invalid reply crossed the transport boundary: imports=%d consumes=%d decision=%+v", f.imports, f.consumes, f.decision)
	}
}

func TestAIBridgeStaleCheckpointCannotConsume(t *testing.T) {
	f := newAIBridgeTestFixture(t)
	service, _ := f.newProcess()
	if _, err := service.AskAITask(context.Background(), aiBridgeTestAskOptions(), nil); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.decision.Status = "answered"
	f.decision.Answer = &AIAnswer{OptionID: "release", SourceType: "user"}
	f.checkpoint.Revision = "revision-changed"
	f.mu.Unlock()
	service, _ = f.newProcess()
	result, err := service.DecisionAITask(context.Background(), aiBridgeTestRun, aiBridgeTestDecision, "release-changed-build")
	if err == nil || result.ShouldExecute || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale approval authorized an operation: result=%+v err=%v", result, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.consumes != 0 || f.decision.Status != "answered" || f.decision.OperationKey != "" {
		t.Fatalf("stale approval reached the claim endpoint: consumes=%d decision=%+v", f.consumes, f.decision)
	}
}

func TestAIContextTypedReferencesRoundTrip(t *testing.T) {
	f := newAIBridgeTestFixture(t)
	client := NewClient(f.life.URL, "life-test-token", time.Second)
	initial, err := client.GetAIContext(context.Background(), aiBridgeTestRun)
	if err != nil {
		t.Fatal(err)
	}
	updated := initial.Checkpoint
	updated.Summary = "Phone reply received; ready to claim"
	updated.ArtifactRefs = []AIContextRef{{ID: "build-18", Title: "Verified build", URL: "https://example.org/build-18", Version: "18"}}
	ack, err := client.UpdateAIContext(context.Background(), aiBridgeTestRun, initial.Revision, updated)
	if err != nil {
		t.Fatal(err)
	}
	client = NewClient(f.life.URL, "life-test-token", time.Second)
	recovered, err := client.GetAIContext(context.Background(), aiBridgeTestRun)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Revision != "revision-2" || !reflect.DeepEqual(recovered.Checkpoint, updated) || recovered.Run.UUID != aiBridgeTestRun || recovered.Run.TodoUUID != "todo-recovery" {
		t.Fatalf("checkpoint references or version failed the round trip: ack=%+v recovered=%+v", ack, recovered)
	}
}

func TestAIContextAcceptsBoundedLargeHistory(t *testing.T) {
	f := newAIBridgeTestFixture(t)
	history, err := json.Marshal(strings.Repeat("event history ", 25*1024))
	if err != nil {
		t.Fatal(err)
	}
	f.checkpoint.Events = []json.RawMessage{history}
	client := NewClient(f.life.URL, "life-test-token", time.Second)
	value, err := client.GetAIContext(context.Background(), aiBridgeTestRun)
	if err != nil {
		t.Fatalf("bounded context over 256 KiB was rejected: %v", err)
	}
	if len(value.Events) != 1 || !bytes.Equal(value.Events[0], history) {
		t.Fatal("bounded event history was truncated")
	}
}

func TestAIContextRejectsOldContentOnPutAcknowledgement(t *testing.T) {
	f := newAIBridgeTestFixture(t)
	oldCheckpoint := f.checkpoint.Checkpoint
	f.contextPutResponse = func(value AIContext) any {
		value.Checkpoint = oldCheckpoint
		return value
	}
	client := NewClient(f.life.URL, "life-test-token", time.Second)
	changedCheckpoint := oldCheckpoint
	changedCheckpoint.Summary = "The newly validated release must be retained"
	if _, err := client.UpdateAIContext(context.Background(), aiBridgeTestRun, "revision-1", changedCheckpoint); err == nil {
		t.Fatal("PUT acknowledged the requested checkpoint even though its returned content differs")
	}
}
