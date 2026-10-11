package integlife

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type BridgeClient struct {
	baseURL     string
	token       string
	client      *http.Client
	configError error
}

type BridgeReply struct {
	ID         string `json:"id"`
	OptionID   string `json:"option_id"`
	Text       string `json:"text,omitempty"`
	AnsweredAt string `json:"answered_at"`
}

type BridgeMessage struct {
	ID     string       `json:"id"`
	Status string       `json:"status"`
	Reply  *BridgeReply `json:"reply,omitempty"`
}

type bridgeHTTPError struct {
	status int
}

func (e *bridgeHTTPError) Error() string {
	return fmt.Sprintf("Bridge returned HTTP %d", e.status)
}

// NewBridgeClient returns nil when Bridge is unconfigured. Credentials stay in memory.
// HTTPS is required except for explicit loopback development endpoints.
func NewBridgeClient(baseURL, token string, timeout time.Duration) *BridgeClient {
	if baseURL == "" && token == "" {
		return nil
	}
	result := &BridgeClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("Bridge redirects are not supported")
		},
	}}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || token == "" {
		result.configError = errors.New("Bridge requires a credential-free base URL and LIFE_BRIDGE_TOKEN")
		return result
	}
	loopback := parsed.Hostname() == "localhost"
	if ip := net.ParseIP(parsed.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		result.configError = errors.New("Bridge requires HTTPS; HTTP is allowed only for loopback development")
	}
	return result
}

func (b *BridgeClient) request(ctx context.Context, method, path string, payload any) (BridgeMessage, error) {
	if b.configError != nil {
		return BridgeMessage{}, b.configError
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return BridgeMessage{}, errors.New("cannot encode Bridge message")
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path, body)
	if err != nil {
		return BridgeMessage{}, errors.New("invalid Bridge URL")
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return BridgeMessage{}, ctx.Err()
		}
		return BridgeMessage{}, errors.New("Bridge unavailable; durable Life decision remains pending")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return BridgeMessage{}, &bridgeHTTPError{resp.StatusCode}
	}
	var message BridgeMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&message); err != nil {
		return message, errors.New("invalid Bridge acknowledgement")
	}
	return message, nil
}

func (b *BridgeClient) getOrSubmit(ctx context.Context, decision AIDecision, taskURL string) (BridgeMessage, error) {
	message, err := b.request(ctx, http.MethodGet, "/v1/messages/"+url.PathEscape(decision.BridgeMessageID), nil)
	var httpError *bridgeHTTPError
	if errors.As(err, &httpError) && httpError.status == http.StatusNotFound {
		// The same immutable ID and payload are used after an uncertain POST or restart.
		// Choice IDs and absolute expiry are explicit Bridge wire-contract extensions.
		message, err = b.request(ctx, http.MethodPost, "/v1/messages", struct {
			ID        string `json:"id"`
			Channel   string `json:"channel"`
			Topic     string `json:"topic"`
			Level     string `json:"level"`
			Title     string `json:"title"`
			Ref       string `json:"ref"`
			ExpiresAt string `json:"expires_at,omitempty"`
			Reply     struct {
				Type      string     `json:"type"`
				Options   []AIOption `json:"options"`
				AllowText bool       `json:"allow_text"`
			} `json:"reply"`
		}{
			ID: decision.BridgeMessageID, Channel: "work", Topic: decision.RunUUID,
			Level: "push", Title: decision.Question, Ref: taskURL, ExpiresAt: decision.ExpiresAt,
			Reply: struct {
				Type      string     `json:"type"`
				Options   []AIOption `json:"options"`
				AllowText bool       `json:"allow_text"`
			}{"choice", decision.Options, true},
		})
	}
	if err != nil {
		return message, err
	}
	return message, validateBridgeMessage(decision, message)
}

// getExisting recovers a reply without creating or notifying a new message.
func (b *BridgeClient) getExisting(ctx context.Context, decision AIDecision) (BridgeMessage, error) {
	message, err := b.request(ctx, http.MethodGet, "/v1/messages/"+url.PathEscape(decision.BridgeMessageID), nil)
	if err != nil {
		return message, err
	}
	return message, validateBridgeMessage(decision, message)
}

func validateBridgeMessage(decision AIDecision, message BridgeMessage) error {
	if message.ID != decision.BridgeMessageID {
		return errors.New("Bridge did not acknowledge the persisted message id")
	}
	switch message.Status {
	case "queued", "delivered", "seen", "expired", "cancelled":
	case "answered":
		if message.Reply == nil || message.Reply.ID == "" || !optionAccepted(decision, message.Reply.OptionID) {
			return errors.New("Bridge reply requires a reply id and an exact allowed option_id")
		}
	default:
		return errors.New("Bridge returned an unknown message status")
	}
	return nil
}

type AIAskOptions struct {
	RunUUID   string
	ID        string
	Question  string
	Options   []AIOption
	ExpiresAt string
	Wait      time.Duration
}

type AIHandoff struct {
	SchemaVersion        int          `json:"schema_version"`
	Runner               string       `json:"runner"`
	RunnerSessionRef     string       `json:"runner_session_ref"`
	RunUUID              string       `json:"run_uuid"`
	CheckpointRevision   string       `json:"checkpoint_revision"`
	Checkpoint           AICheckpoint `json:"checkpoint"`
	Decision             AIDecision   `json:"decision"`
	ShouldExecute        bool         `json:"should_execute"`
	ActionScope          string       `json:"action_scope"`
	RequiresRunnerResume bool         `json:"requires_runner_resume"`
	RecoveryArgs         []string     `json:"recovery_args"`
}

type AIAskResult struct {
	ServerAccepted bool       `json:"server_accepted"`
	Decision       AIDecision `json:"decision"`
	TaskURL        string     `json:"task_url"`
	BridgeState    string     `json:"bridge_state"`
	BridgeError    string     `json:"bridge_error,omitempty"`
	Handoff        AIHandoff  `json:"handoff"`
}

type AIDecisionResult struct {
	ServerAccepted bool       `json:"server_accepted"`
	Decision       AIDecision `json:"decision"`
	TaskURL        string     `json:"task_url"`
	Consumed       bool       `json:"consumed"`
	ShouldExecute  bool       `json:"should_execute"`
	BridgeState    string     `json:"bridge_state,omitempty"`
	BridgeError    string     `json:"bridge_error,omitempty"`
	Handoff        AIHandoff  `json:"handoff"`
}

func newHandoff(ctx AIContext, decision AIDecision, shouldExecute bool) AIHandoff {
	return AIHandoff{
		SchemaVersion: 1, Runner: "codex", RunnerSessionRef: ctx.Checkpoint.RunnerSessionRef,
		RunUUID: decision.RunUUID, CheckpointRevision: ctx.Revision,
		Checkpoint: ctx.Checkpoint, Decision: decision, ShouldExecute: shouldExecute, ActionScope: "handle_selected_option", RequiresRunnerResume: true,
		RecoveryArgs: []string{"life", "ai", "decision", "--run", decision.RunUUID, "--id", decision.ID, "--json"},
	}
}

// AskAITask persists its identity before any Bridge operation. Finite waits never approve.
func (s *Service) AskAITask(ctx context.Context, opts AIAskOptions, bridge *BridgeClient) (AIAskResult, error) {
	result := AIAskResult{BridgeState: "unconfigured"}
	if s.client == nil {
		return result, errors.New("Life API client is required")
	}
	var err error
	opts.RunUUID, err = resourceID(opts.RunUUID, "run")
	if err != nil {
		return result, err
	}
	opts.ID, err = resourceID(opts.ID, "decision id")
	if err != nil {
		return result, err
	}
	opts.Question = strings.TrimSpace(opts.Question)
	if opts.Question == "" {
		return result, errors.New("--question is required")
	}
	if err := validateOptions(opts.Options); err != nil {
		return result, err
	}
	if opts.Wait < 0 || opts.Wait > time.Minute {
		return result, errors.New("--wait must be between 0 and 60 seconds")
	}
	if opts.ExpiresAt != "" {
		expires, err := time.Parse(time.RFC3339Nano, opts.ExpiresAt)
		if err != nil {
			return result, errors.New("--expires-at must be an RFC3339 timestamp")
		}
		opts.ExpiresAt = expires.UTC().Format(time.RFC3339Nano)
	}
	checkpoint, err := s.client.GetAIContext(ctx, opts.RunUUID)
	if err != nil {
		return result, err
	}
	if strings.TrimSpace(checkpoint.Checkpoint.Goal) == "" || strings.TrimSpace(checkpoint.Checkpoint.NextStep) == "" {
		return result, errors.New("save a checkpoint with goal and next_step using life ai context --file before asking")
	}
	payload := CreateAIDecision{
		ID: opts.ID, ExpectedRevision: checkpoint.Revision, Question: opts.Question,
		Options: opts.Options, BridgeMessageID: BridgeMessageID(opts.RunUUID, opts.ID), ExpiresAt: opts.ExpiresAt,
	}
	decision, err := s.client.GetAIDecision(ctx, opts.RunUUID, opts.ID)
	var httpError *AIHTTPError
	if errors.As(err, &httpError) && httpError.Status == http.StatusNotFound {
		decision, err = s.client.CreateAIDecision(ctx, opts.RunUUID, payload)
	}
	if err != nil {
		return result, err
	}
	if !decisionMatches(decision, payload) {
		return result, errors.New("decision id is already bound to another question, options, expiry, or Bridge message; use a new id for a changed proposal")
	}
	result.ServerAccepted = true
	result.Decision = decision
	result.TaskURL = TaskURL(checkpoint.Run.TodoUUID, opts.RunUUID)
	finish := func() AIAskResult {
		result.Handoff = newHandoff(checkpoint, result.Decision, false)
		return result
	}
	if decision.CheckpointRevision != checkpoint.Revision || decisionExpired(decision, time.Now()) || decision.Status != "pending" {
		return finish(), nil
	}
	deadline := time.Now().Add(opts.Wait)
	pollCtx := ctx
	if opts.Wait > 0 {
		var cancel context.CancelFunc
		pollCtx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	waitExpired := func() bool {
		return ctx.Err() == nil && opts.Wait > 0 && errors.Is(pollCtx.Err(), context.DeadlineExceeded)
	}
	for {
		if bridge != nil {
			message, bridgeErr := bridge.getOrSubmit(pollCtx, result.Decision, result.TaskURL)
			if bridgeErr != nil {
				if waitExpired() {
					result.BridgeState = "pending"
					result.BridgeError = "bounded wait elapsed; recover the same durable decision"
					return finish(), nil
				}
				result.BridgeState = "unavailable"
				result.BridgeError = bridgeErr.Error()
			} else {
				result.BridgeState = message.Status
				result.BridgeError = ""
				if message.Status == "answered" {
					decision, err = s.client.ImportBridgeAnswer(pollCtx, opts.RunUUID, opts.ID, message.Reply.OptionID, message.Reply.Text)
					if err != nil {
						if waitExpired() {
							result.BridgeError = "reply import has no server acknowledgement; recover the same durable decision"
							return finish(), nil
						}
						return finish(), err
					}
					result.Decision = decision
				}
				if message.Status == "expired" || message.Status == "cancelled" {
					bridge = nil
				}
			}
		}
		if result.Decision.Status != "pending" || !time.Now().Before(deadline) {
			return finish(), nil
		}
		delay := time.Until(deadline)
		if delay > time.Second {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return finish(), ctx.Err()
		case <-timer.C:
		}
		if !time.Now().Before(deadline) {
			return finish(), nil
		}
		decision, err = s.client.GetAIDecision(pollCtx, opts.RunUUID, opts.ID)
		if err != nil {
			if waitExpired() {
				return finish(), nil
			}
			return finish(), err
		}
		result.Decision = decision
		if result.Decision.Status != "pending" || decisionExpired(decision, time.Now()) {
			return finish(), nil
		}
	}
}

// DecisionAITask reads or claims a reply. It never executes shell commands.
// Only a first, explicitly acknowledged claim returns should_execute=true.
// The caller must handle answer.option_id; a decline never approves the proposed action.
func (s *Service) DecisionAITask(ctx context.Context, runUUID, decisionID, operationKey string, bridges ...*BridgeClient) (AIDecisionResult, error) {
	result := AIDecisionResult{}
	if s.client == nil {
		return result, errors.New("Life API client is required")
	}
	checkpoint, err := s.client.GetAIContext(ctx, runUUID)
	if err != nil {
		return result, err
	}
	decision, err := s.client.GetAIDecision(ctx, runUUID, decisionID)
	if err != nil {
		return result, err
	}
	if len(bridges) > 0 && bridges[0] != nil && decision.Status == "pending" && decision.CheckpointRevision == checkpoint.Revision && !decisionExpired(decision, time.Now()) {
		if decision.BridgeMessageID != BridgeMessageID(runUUID, decisionID) {
			return result, errors.New("persisted Bridge message id does not match this decision")
		}
		message, bridgeErr := bridges[0].getExisting(ctx, decision)
		if bridgeErr != nil {
			result.BridgeState = "unavailable"
			result.BridgeError = bridgeErr.Error()
		} else {
			result.BridgeState = message.Status
			if message.Status == "answered" {
				decision, err = s.client.ImportBridgeAnswer(ctx, runUUID, decisionID, message.Reply.OptionID, message.Reply.Text)
				if err != nil {
					return result, err
				}
			}
		}
	}
	if operationKey != "" {
		alreadyClaimed := decision.Status == "consumed" && decision.OperationKey == operationKey
		if !alreadyClaimed && (decision.CheckpointRevision != checkpoint.Revision || decisionExpired(decision, time.Now())) {
			return result, errors.New("decision checkpoint is stale or expired; it cannot authorize this operation")
		}
		consumption, err := s.client.ConsumeAIDecision(ctx, runUUID, decisionID, operationKey)
		if err != nil {
			return result, err
		}
		decision = consumption.Decision
		if !optionAccepted(decision, decision.Answer.OptionID) {
			return result, errors.New("Life API returned an answer outside the decision options")
		}
		result.Consumed = consumption.Consumed
		result.ShouldExecute = consumption.Consumed
	}
	result.ServerAccepted = true
	result.Decision = decision
	result.TaskURL = TaskURL(checkpoint.Run.TodoUUID, runUUID)
	result.Handoff = newHandoff(checkpoint, decision, result.ShouldExecute)
	return result, nil
}
