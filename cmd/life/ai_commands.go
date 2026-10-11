package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/flyfy1/life-cli/internal/integlife"
)

func cmdAIContext(ctx context.Context, service *integlife.Service, args []string) error {
	fs := flag.NewFlagSet("life ai context", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runUUID := fs.String("run", "", "full run uuid")
	file := fs.String("file", "", "checkpoint JSON file to save; - reads stdin")
	expectedRevision := fs.String("expected-revision", "", "checkpoint revision required for a conditional write")
	jsonOut := fs.Bool("json", false, "print server acknowledgement as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*runUUID) == "" {
		return errors.New("usage: life ai context --run <run_uuid> [--file checkpoint.json] [--expected-revision revision] [--json]")
	}
	var checkpoint integlife.AICheckpoint
	if *file != "" {
		var reader io.Reader = os.Stdin
		if *file != "-" {
			opened, err := os.Open(*file)
			if err != nil {
				return fmt.Errorf("open checkpoint file: %w", err)
			}
			defer opened.Close()
			reader = opened
		}
		var err error
		checkpoint, err = integlife.DecodeAICheckpoint(reader)
		if err != nil {
			return err
		}
	} else if *expectedRevision != "" {
		return errors.New("--expected-revision requires --file")
	}
	result, err := service.GetAIContext(ctx, *runUUID)
	if err != nil {
		return err
	}
	if *file != "" {
		revision := *expectedRevision
		if revision == "" {
			revision = result.Revision
		}
		result, err = service.UpdateAIContext(ctx, *runUUID, revision, checkpoint)
		if err != nil {
			return err
		}
	}
	if *jsonOut {
		printJSON(struct {
			ServerAccepted bool `json:"server_accepted"`
			integlife.AIContext
		}{true, result})
	} else {
		fmt.Printf("server accepted: run=%s revision=%s\n", result.Run.UUID, result.Revision)
		fmt.Printf("goal: %s\nsummary: %s\nnext step: %s\n", result.Checkpoint.Goal, result.Checkpoint.Summary, result.Checkpoint.NextStep)
		fmt.Printf("task: %s\n", integlife.TaskURL(result.Run.TodoUUID, result.Run.UUID))
		if result.Truncated {
			fmt.Println("recent events or decisions are truncated; fetch additional history through the run API")
			if result.NextCursor != "" {
				fmt.Printf("next cursor: %s\n", result.NextCursor)
			}
		}
	}
	return nil
}

type aiOptionFlags []integlife.AIOption

func (options *aiOptionFlags) String() string {
	return fmt.Sprintf("%v", []integlife.AIOption(*options))
}

func (options *aiOptionFlags) Set(value string) error {
	id, label, found := strings.Cut(value, "=")
	if !found || strings.TrimSpace(id) == "" || strings.TrimSpace(label) == "" {
		return errors.New("--option requires a stable id=label pair")
	}
	*options = append(*options, integlife.AIOption{ID: strings.TrimSpace(id), Label: strings.TrimSpace(label)})
	return nil
}

func cmdAIAsk(ctx context.Context, service *integlife.Service, args []string) error {
	fs := flag.NewFlagSet("life ai ask", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runUUID := fs.String("run", "", "full run uuid")
	id := fs.String("id", "", "stable decision id reused after restart")
	question := fs.String("question", "", "question to ask")
	expiresAt := fs.String("expires-at", "", "optional absolute expiry as RFC3339")
	wait := fs.Int("wait", 0, "seconds to poll (0 to 60); timeout remains pending")
	handoffFile := fs.String("handoff-file", "", "write a portable Codex handoff JSON file")
	jsonOut := fs.Bool("json", false, "print server acknowledgement and pending/reply state as JSON")
	var options aiOptionFlags
	fs.Var(&options, "option", "choice as id=label; repeat 2 to 4 times")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("life ai ask accepts only flags")
	}
	cfg := integlife.LoadConfig()
	bridge := integlife.NewBridgeClient(cfg.BridgeURL, cfg.BridgeToken, 10*time.Second)
	result, err := service.AskAITask(ctx, integlife.AIAskOptions{
		RunUUID: *runUUID, ID: *id, Question: *question, Options: options,
		ExpiresAt: *expiresAt, Wait: time.Duration(*wait) * time.Second,
	}, bridge)
	if err != nil {
		return err
	}
	if err := saveAIHandoff(*handoffFile, result.Handoff); err != nil {
		return err
	}
	if *jsonOut {
		printJSON(result)
	} else {
		fmt.Printf("server accepted: run=%s decision=%s status=%s\n", result.Decision.RunUUID, result.Decision.ID, result.Decision.Status)
		fmt.Printf("task: %s\nbridge: %s\n", result.TaskURL, result.BridgeState)
		if result.BridgeError != "" {
			fmt.Println(result.BridgeError)
		}
		if result.Decision.Answer != nil {
			fmt.Printf("answer: option=%s text=%s source=%s\n", result.Decision.Answer.OptionID, result.Decision.Answer.Text, result.Decision.Answer.SourceType)
		}
		fmt.Println("recover this decision with life ai decision --run " + result.Decision.RunUUID + " --id " + result.Decision.ID + " --json")
		if *handoffFile != "" {
			fmt.Printf("handoff: %s\n", *handoffFile)
		}
	}
	return nil
}

func cmdAIDecision(ctx context.Context, service *integlife.Service, args []string) error {
	fs := flag.NewFlagSet("life ai decision", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runUUID := fs.String("run", "", "full run uuid")
	id := fs.String("id", "", "stable decision id")
	consume := fs.String("operation-key", "", "claim the reply using this explicit operation key")
	fs.StringVar(consume, "consume", "", "alias for --operation-key")
	handoffFile := fs.String("handoff-file", "", "write a portable Codex handoff JSON file")
	jsonOut := fs.Bool("json", false, "print decision and claim acknowledgement as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*runUUID) == "" || strings.TrimSpace(*id) == "" {
		return errors.New("usage: life ai decision --run <run_uuid> --id <decision_id> [--operation-key operation-key] [--handoff-file handoff.json] [--json]")
	}
	if flagProvided(fs, "consume") && flagProvided(fs, "operation-key") {
		return errors.New("use only one of --operation-key or --consume")
	}
	if (flagProvided(fs, "consume") || flagProvided(fs, "operation-key")) && strings.TrimSpace(*consume) == "" {
		return errors.New("--operation-key requires a non-empty operation key")
	}
	cfg := integlife.LoadConfig()
	bridge := integlife.NewBridgeClient(cfg.BridgeURL, cfg.BridgeToken, 10*time.Second)
	result, err := service.DecisionAITask(ctx, *runUUID, *id, *consume, bridge)
	if err != nil {
		return err
	}
	if err := saveAIHandoff(*handoffFile, result.Handoff); err != nil {
		return err
	}
	if *jsonOut {
		printJSON(result)
	} else {
		fmt.Printf("decision: run=%s id=%s status=%s\n", result.Decision.RunUUID, result.Decision.ID, result.Decision.Status)
		if result.BridgeState != "" {
			fmt.Printf("bridge: %s\n", result.BridgeState)
		}
		if result.BridgeError != "" {
			fmt.Println(result.BridgeError)
		}
		if result.Decision.Answer != nil {
			fmt.Printf("answer: option=%s text=%s source=%s\n", result.Decision.Answer.OptionID, result.Decision.Answer.Text, result.Decision.Answer.SourceType)
		}
		if *consume != "" {
			fmt.Printf("claimed=%t should_execute=%t operation_key=%s\n", result.Consumed, result.ShouldExecute, result.Decision.OperationKey)
			if !result.ShouldExecute {
				fmt.Println("this operation was already claimed; reconcile its artifact or external result before retrying")
			}
		}
		fmt.Printf("task: %s\n", result.TaskURL)
		if *handoffFile != "" {
			fmt.Printf("handoff: %s\n", *handoffFile)
		}
	}
	return nil
}

func saveAIHandoff(path string, handoff integlife.AIHandoff) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(handoff, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("write handoff file: %w", err)
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("protect handoff file: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write handoff file: %w", err)
	}
	return nil
}
