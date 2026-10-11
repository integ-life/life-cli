package integlife

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const MaxCheckpointBytes = 32 * 1024
const MaxAIResponseBytes = 2 * 1024 * 1024

type AIContextRef struct {
	ID      string `json:"id,omitempty"`
	Title   string `json:"title,omitempty"`
	URL     string `json:"url,omitempty"`
	Version string `json:"version,omitempty"`
}

// AICheckpoint is a portable handoff summary, not serialized model state.
type AICheckpoint struct {
	SchemaVersion      int            `json:"schema_version"`
	Goal               string         `json:"goal"`
	Summary            string         `json:"summary"`
	NextStep           string         `json:"next_step"`
	Constraints        []string       `json:"constraints"`
	AcceptanceCriteria []string       `json:"acceptance_criteria"`
	SourceRefs         []AIContextRef `json:"source_refs"`
	ArtifactRefs       []AIContextRef `json:"artifact_refs"`
	RunnerSessionRef   string         `json:"runner_session_ref"`
}

type AIContextRun struct {
	UUID      string `json:"uuid"`
	TodoUUID  string `json:"todo_uuid"`
	Title     string `json:"title"`
	AgentName string `json:"agent_name"`
	Status    string `json:"status"`
}

type AIContext struct {
	Run        AIContextRun      `json:"run"`
	Revision   string            `json:"revision"`
	Checkpoint AICheckpoint      `json:"checkpoint"`
	Decisions  []AIDecision      `json:"decisions"`
	Events     []json.RawMessage `json:"events"`
	Truncated  bool              `json:"truncated"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

type AIOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type AIAnswer struct {
	OptionID   string `json:"option_id"`
	Text       string `json:"text,omitempty"`
	SourceType string `json:"source_type"`
	SourceID   string `json:"source_id,omitempty"`
	SourceName string `json:"source_name,omitempty"`
	UserID     int    `json:"user_id"`
	AnsweredAt string `json:"answered_at"`
}

type AIDecision struct {
	ID                 string     `json:"id"`
	RunUUID            string     `json:"run_uuid"`
	CheckpointRevision string     `json:"checkpoint_revision"`
	Question           string     `json:"question"`
	Options            []AIOption `json:"options"`
	BridgeMessageID    string     `json:"bridge_message_id,omitempty"`
	Status             string     `json:"status"`
	CreatedAt          string     `json:"created_at"`
	ExpiresAt          string     `json:"expires_at,omitempty"`
	Answer             *AIAnswer  `json:"answer,omitempty"`
	OperationKey       string     `json:"operation_key,omitempty"`
	ConsumedAt         string     `json:"consumed_at,omitempty"`
}

type CreateAIDecision struct {
	ID               string     `json:"id"`
	ExpectedRevision string     `json:"expected_revision"`
	Question         string     `json:"question"`
	Options          []AIOption `json:"options"`
	BridgeMessageID  string     `json:"bridge_message_id,omitempty"`
	ExpiresAt        string     `json:"expires_at,omitempty"`
}

type AIDecisionResponse struct {
	Decision AIDecision `json:"decision"`
}

type AIConsumeResponse struct {
	Decision AIDecision `json:"decision"`
	Consumed bool       `json:"consumed"`
}

type AIHTTPError struct {
	Status int
}

func (e *AIHTTPError) Error() string {
	return fmt.Sprintf("Life API returned HTTP %d", e.Status)
}

func DecodeAICheckpoint(reader io.Reader) (AICheckpoint, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxCheckpointBytes+1))
	if err != nil {
		return AICheckpoint{}, fmt.Errorf("read checkpoint: %w", err)
	}
	if len(data) > MaxCheckpointBytes {
		return AICheckpoint{}, fmt.Errorf("checkpoint exceeds %d bytes", MaxCheckpointBytes)
	}
	var checkpoint AICheckpoint
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&checkpoint); err != nil {
		return AICheckpoint{}, fmt.Errorf("decode checkpoint: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return AICheckpoint{}, errors.New("checkpoint must contain one JSON object")
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return AICheckpoint{}, err
	}
	return normalizeCheckpoint(checkpoint), nil
}

func normalizeCheckpoint(checkpoint AICheckpoint) AICheckpoint {
	if checkpoint.Constraints == nil {
		checkpoint.Constraints = []string{}
	}
	if checkpoint.AcceptanceCriteria == nil {
		checkpoint.AcceptanceCriteria = []string{}
	}
	if checkpoint.SourceRefs == nil {
		checkpoint.SourceRefs = []AIContextRef{}
	}
	if checkpoint.ArtifactRefs == nil {
		checkpoint.ArtifactRefs = []AIContextRef{}
	}
	return checkpoint
}

func validateCheckpoint(checkpoint AICheckpoint) error {
	if checkpoint.SchemaVersion != 1 {
		return errors.New("checkpoint schema_version must be 1")
	}
	data, err := json.Marshal(checkpoint)
	if err != nil || len(data) > MaxCheckpointBytes {
		return fmt.Errorf("checkpoint exceeds %d bytes", MaxCheckpointBytes)
	}
	for _, refs := range [][]AIContextRef{checkpoint.SourceRefs, checkpoint.ArtifactRefs} {
		for _, ref := range refs {
			if strings.TrimSpace(ref.ID) == "" && strings.TrimSpace(ref.URL) == "" {
				return errors.New("each checkpoint reference requires id or url")
			}
			if ref.URL != "" {
				parsed, err := url.Parse(ref.URL)
				if err != nil || parsed.User != nil || parsed.RawQuery != "" {
					return errors.New("checkpoint reference URLs must not contain credentials or query parameters")
				}
			}
		}
	}
	return nil
}

func resourceID(value, name string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 || value == "." || value == ".." {
		return "", fmt.Errorf("%s must be a stable identifier without path separators", name)
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("-_.:", char)) {
			return "", fmt.Errorf("%s must contain only letters, digits, dash, underscore, dot, or colon", name)
		}
	}
	return value, nil
}

func aiRunPath(runUUID string) (string, error) {
	value, err := resourceID(runUUID, "run")
	if err != nil {
		return "", err
	}
	return "/api/ai-task-runs/" + url.PathEscape(value), nil
}

func aiDecisionPath(runUUID, decisionID string) (string, error) {
	path, err := aiRunPath(runUUID)
	if err != nil {
		return "", err
	}
	id, err := resourceID(decisionID, "decision id")
	if err != nil {
		return "", err
	}
	return path + "/decisions/" + url.PathEscape(id), nil
}

// requestAI deliberately excludes remote error bodies and transport URLs from errors.
// A remote failure must never turn a local pending write into server acceptance.
func (c *Client) requestAI(ctx context.Context, method, path string, payload, out any) error {
	if !c.CanSync() {
		return errors.New("a Life API token is required; run life login or set INTEGLIFE_API_TOKEN")
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode Life API request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL+path, body)
	if err != nil {
		return errors.New("invalid Life API URL")
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Life API request failed; no server acceptance confirmed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &AIHTTPError{Status: resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxAIResponseBytes+1))
	if err != nil || len(data) > MaxAIResponseBytes {
		return errors.New("Life API acknowledgement exceeds the bounded response limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(out); err != nil {
		return errors.New("invalid Life API acknowledgement")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid Life API acknowledgement")
	}
	return nil
}

func validateContextAck(runUUID string, result AIContext) error {
	if result.Run.UUID != runUUID || result.Revision == "" || result.Checkpoint.SchemaVersion != 1 {
		return errors.New("Life API did not acknowledge the requested run checkpoint and revision")
	}
	return nil
}

func (c *Client) GetAIContext(ctx context.Context, runUUID string) (AIContext, error) {
	path, err := aiRunPath(runUUID)
	if err != nil {
		return AIContext{}, err
	}
	var result AIContext
	if err := c.requestAI(ctx, http.MethodGet, path+"/context?limit=3", nil, &result); err != nil {
		return result, err
	}
	return result, validateContextAck(runUUID, result)
}

func (c *Client) UpdateAIContext(ctx context.Context, runUUID, revision string, checkpoint AICheckpoint) (AIContext, error) {
	if revision == "" {
		return AIContext{}, errors.New("expected checkpoint revision is required")
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return AIContext{}, err
	}
	checkpoint = normalizeCheckpoint(checkpoint)
	path, err := aiRunPath(runUUID)
	if err != nil {
		return AIContext{}, err
	}
	var result AIContext
	err = c.requestAI(ctx, http.MethodPut, path+"/context", struct {
		ExpectedRevision string       `json:"expected_revision"`
		Checkpoint       AICheckpoint `json:"checkpoint"`
	}{revision, checkpoint}, &result)
	if err != nil {
		return result, err
	}
	if err := validateContextAck(runUUID, result); err != nil {
		return result, err
	}
	requested, _ := json.Marshal(checkpoint)
	acknowledged, _ := json.Marshal(normalizeCheckpoint(result.Checkpoint))
	if !bytes.Equal(requested, acknowledged) {
		return result, errors.New("Life API did not acknowledge the submitted checkpoint contents")
	}
	return result, nil
}

func validateDecisionAck(runUUID, decisionID string, result AIDecision) error {
	if result.RunUUID != runUUID || result.ID != decisionID || result.CheckpointRevision == "" {
		return errors.New("Life API did not acknowledge the requested durable decision")
	}
	switch result.Status {
	case "pending", "answered", "consumed", "stale", "expired", "canceled":
	default:
		return errors.New("Life API returned an unknown decision status")
	}
	return nil
}

func (c *Client) GetAIDecision(ctx context.Context, runUUID, decisionID string) (AIDecision, error) {
	path, err := aiDecisionPath(runUUID, decisionID)
	if err != nil {
		return AIDecision{}, err
	}
	var result AIDecisionResponse
	if err := c.requestAI(ctx, http.MethodGet, path, nil, &result); err != nil {
		return result.Decision, err
	}
	return result.Decision, validateDecisionAck(runUUID, decisionID, result.Decision)
}

func (c *Client) CreateAIDecision(ctx context.Context, runUUID string, payload CreateAIDecision) (AIDecision, error) {
	path, err := aiRunPath(runUUID)
	if err != nil {
		return AIDecision{}, err
	}
	var result AIDecisionResponse
	if err := c.requestAI(ctx, http.MethodPost, path+"/decisions", payload, &result); err != nil {
		return result.Decision, err
	}
	return result.Decision, validateDecisionAck(runUUID, payload.ID, result.Decision)
}

func (c *Client) ImportBridgeAnswer(ctx context.Context, runUUID, decisionID, optionID, text string) (AIDecision, error) {
	path, err := aiDecisionPath(runUUID, decisionID)
	if err != nil {
		return AIDecision{}, err
	}
	var result AIDecisionResponse
	if err := c.requestAI(ctx, http.MethodPost, path+"/answer", struct {
		OptionID string `json:"option_id"`
		Text     string `json:"text,omitempty"`
		Source   string `json:"source"`
	}{optionID, text, "bridge_transport"}, &result); err != nil {
		return result.Decision, err
	}
	return result.Decision, validateDecisionAck(runUUID, decisionID, result.Decision)
}

func (c *Client) ConsumeAIDecision(ctx context.Context, runUUID, decisionID, operationKey string) (AIConsumeResponse, error) {
	if strings.TrimSpace(operationKey) == "" {
		return AIConsumeResponse{}, errors.New("an explicit operation key is required to consume a decision")
	}
	path, err := aiDecisionPath(runUUID, decisionID)
	if err != nil {
		return AIConsumeResponse{}, err
	}
	var result AIConsumeResponse
	if err := c.requestAI(ctx, http.MethodPost, path+"/consume", map[string]string{"operation_key": operationKey}, &result); err != nil {
		return result, err
	}
	if err := validateDecisionAck(runUUID, decisionID, result.Decision); err != nil {
		return result, err
	}
	if result.Decision.Status != "consumed" || result.Decision.OperationKey != operationKey || result.Decision.Answer == nil {
		return result, errors.New("Life API did not acknowledge consumption for this operation key")
	}
	return result, nil
}

// BridgeMessageID is deterministic across processes and persisted on the Life decision.
func BridgeMessageID(runUUID, decisionID string) string {
	hash := sha256.Sum256([]byte(runUUID + "\x00" + decisionID))
	return "life-" + hex.EncodeToString(hash[:16])
}

func (s *Service) GetAIContext(ctx context.Context, runUUID string) (AIContext, error) {
	if s.client == nil {
		return AIContext{}, errors.New("Life API client is required")
	}
	return s.client.GetAIContext(ctx, strings.TrimSpace(runUUID))
}

func (s *Service) UpdateAIContext(ctx context.Context, runUUID, expectedRevision string, checkpoint AICheckpoint) (AIContext, error) {
	if s.client == nil {
		return AIContext{}, errors.New("Life API client is required")
	}
	return s.client.UpdateAIContext(ctx, strings.TrimSpace(runUUID), expectedRevision, checkpoint)
}

// TaskURL returns the authenticated task page; no credentials belong in this URL.
func TaskURL(todoUUID, runUUID string) string {
	return "https://app.integ.life/#/todo/" + url.PathEscape(todoUUID) + "?run=" + url.QueryEscape(runUUID)
}

func validateOptions(options []AIOption) error {
	if len(options) < 2 || len(options) > 4 {
		return errors.New("ask requires 2 to 4 --option id=label choices")
	}
	seen := map[string]bool{}
	for _, option := range options {
		if _, err := resourceID(option.ID, "option id"); err != nil {
			return err
		}
		if strings.TrimSpace(option.Label) == "" || seen[option.ID] {
			return errors.New("option ids must be unique and labels non-empty")
		}
		seen[option.ID] = true
	}
	return nil
}

func optionAccepted(decision AIDecision, optionID string) bool {
	for _, option := range decision.Options {
		if option.ID == optionID {
			return true
		}
	}
	return false
}

func decisionMatches(decision AIDecision, payload CreateAIDecision) bool {
	if decision.Question != payload.Question || decision.BridgeMessageID != payload.BridgeMessageID || decision.ExpiresAt != payload.ExpiresAt || len(decision.Options) != len(payload.Options) {
		return false
	}
	for index := range payload.Options {
		if decision.Options[index] != payload.Options[index] {
			return false
		}
	}
	return true
}

func decisionExpired(decision AIDecision, now time.Time) bool {
	if decision.ExpiresAt == "" {
		return false
	}
	expires, err := time.Parse(time.RFC3339Nano, decision.ExpiresAt)
	return err != nil || !expires.After(now)
}
