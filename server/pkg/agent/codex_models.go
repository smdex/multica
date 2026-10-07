package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"syscall"
	"time"
)

// Discovery never creates a thread or turn. model/list is the installed
// app-server's account-aware catalog; bundled data only supplies service tiers
// (not exposed by model/list) or a non-authoritative fallback.
func discoverCodexModels(ctx context.Context, runtimeCmd Command) Catalog {
	if runtimeCmd.Path == "" {
		runtimeCmd.Path = "codex"
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	models, discoveryErr := discoverCodexAppServerModels(ctx, runtimeCmd)
	version, _ := DetectVersion(ctx, runtimeCmd)
	var bundled []Model
	if codexSupportsDebugModels(version) {
		metadataCtx, cancelMetadata := context.WithTimeout(ctx, 3*time.Second)
		raw, err := runCodexDebugModels(metadataCtx, runtimeCmd)
		cancelMetadata()
		if err == nil {
			bundled, _ = parseCodexModelCatalog(raw)
		}
	}
	if discoveryErr != nil {
		models = bundled
		if len(models) == 0 {
			models = codexStaticModels()
		}
	} else {
		// Never merge bundled model IDs, defaults, or reasoning levels into a
		// live answer: only the app-server knows which models are available.
		tiers := make(map[string][]ModelServiceTier, len(bundled))
		for _, model := range bundled {
			tiers[model.ID] = model.ServiceTiers
		}
		for i := range models {
			models[i].ServiceTiers = tiers[models[i].ID]
		}
	}
	return Catalog{
		Models:   annotateCodexExplicitStandardServiceTier(models, codexSupportsExplicitStandardServiceTier(version)),
		Fallback: discoveryErr != nil,
	}
}

func discoverCodexAppServerModels(ctx context.Context, runtimeCmd Command) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := runtimeCmd.exec(ctx, "app-server")
	hideAgentWindow(cmd)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	defer stdout.Close()
	if err := startOwnedProcessTree(cmd, runtimeCmd.logger); err != nil {
		return nil, err
	}

	client := &codexClient{stdin: stdin, pending: make(map[int]*pendingRPC)}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 4*1024*1024)
		for scanner.Scan() {
			var envelope map[string]json.RawMessage
			if json.Unmarshal(scanner.Bytes(), &envelope) != nil {
				continue
			}
			// Reuse response correlation/error handling, not execution's
			// notification dispatcher or auto-approval handler.
			if _, ok := envelope["id"]; !ok || envelope["method"] != nil {
				continue
			}
			if envelope["result"] != nil || envelope["error"] != nil {
				client.handleResponse(envelope)
			}
		}
		client.markProcessExited(scanner.Err())
	}()
	defer func() {
		cancel()
		_ = stdin.Close()
		signalProcessGroup(cmd, syscall.SIGKILL)
		_ = stdout.Close()
		_ = cmd.Wait()
		<-readerDone
		releaseProcessGroup(cmd)
	}()

	if _, err := client.request(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "multica-agent-sdk", "title": "Multica Agent SDK", "version": "0.2.0"},
		"capabilities": map[string]any{"experimentalApi": true},
	}); err != nil {
		return nil, err
	}
	client.notify("initialized")

	var models []Model
	cursor := ""
	seen := map[string]bool{}
	// Bound both total work and malformed pagination, in addition to the
	// subprocess deadline and per-response byte limit.
	for page := 0; page < 100; page++ {
		params := map[string]any{"limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := client.request(ctx, "model/list", params)
		if err != nil {
			return nil, err
		}
		var response struct {
			Data []struct {
				ID                        string `json:"id"`
				DisplayName               string `json:"displayName"`
				IsDefault                 bool   `json:"isDefault"`
				DefaultReasoningEffort    string `json:"defaultReasoningEffort"`
				SupportedReasoningEfforts []struct {
					ReasoningEffort string `json:"reasoningEffort"`
					Description     string `json:"description"`
				} `json:"supportedReasoningEfforts"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			return nil, err
		}
		if response.Data == nil || len(response.Data) > 100 {
			return nil, fmt.Errorf("codex model/list returned an invalid page")
		}
		for _, model := range response.Data {
			if model.ID == "" {
				return nil, fmt.Errorf("codex model/list returned a model without an id")
			}
			label := model.DisplayName
			if label == "" {
				label = model.ID
			}
			reasoning := codexDebugModel{DefaultReasoningLevel: model.DefaultReasoningEffort}
			for _, effort := range model.SupportedReasoningEfforts {
				reasoning.SupportedReasoningLevel = append(reasoning.SupportedReasoningLevel, codexDebugReasoningLevel{
					Effort: effort.ReasoningEffort, Description: effort.Description,
				})
			}
			models = append(models, Model{
				ID: model.ID, Label: normalizeCodexModelLabel(model.ID, label), Provider: "openai",
				Default: model.IsDefault, Thinking: codexThinkingFromDebugModel(reasoning),
			})
		}
		cursor = response.NextCursor
		if cursor == "" {
			if len(models) == 0 {
				return nil, fmt.Errorf("codex model/list returned no models")
			}
			return models, nil
		}
		if seen[cursor] {
			return nil, fmt.Errorf("codex model/list repeated a cursor")
		}
		seen[cursor] = true
	}
	return nil, fmt.Errorf("codex model/list exceeded the page limit")
}
