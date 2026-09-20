package worker

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

func modelMenuReadOnly(args string) bool {
	return args == "" || (protocol.ValidModelMenuArgs(args) &&
		(args == "--cancel" || args == "--menu" || strings.HasPrefix(args, "--menu ") || strings.HasPrefix(args, "--page ")))
}

func (s *sessionActor) codexModelMenu(ctx context.Context, client *codexadapter.Client, args string) (protocol.Result, error) {
	if args == "--cancel" {
		return protocol.Result{Text: "Model change cancelled. Session settings are unchanged."}, nil
	}
	if !modelMenuReadOnly(args) {
		text, err := s.codexModel(ctx, client, args)
		return protocol.Result{Text: text}, err
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		return protocol.Result{}, err
	}
	var available []codexadapter.ModelInfo
	seen := map[string]bool{}
	for _, model := range models {
		id := valueOr(model.ID, model.Model)
		if protocol.ModelMenuToken(id) && !seen[id] {
			model.ID = id
			available = append(available, model)
			seen[id] = true
		}
	}
	if strings.HasPrefix(args, "--menu ") {
		id := strings.TrimPrefix(args, "--menu ")
		for _, model := range available {
			if model.ID == id {
				return s.modelReasoningMenu(model)
			}
		}
		return protocol.Result{}, validationError("This model is no longer available. Run /model to see current choices.")
	}
	if len(available) == 0 {
		return protocol.Result{Text: "No models are available from this runtime."}, nil
	}
	page := 0
	if strings.HasPrefix(args, "--page ") {
		page, _ = strconv.Atoi(strings.TrimPrefix(args, "--page "))
	}
	pages := (len(available) + protocol.ModelMenuPageSize - 1) / protocol.ModelMenuPageSize
	if page >= pages {
		page = pages - 1
	}
	result := protocol.Result{Text: "Choose a model for this session. Next, choose its reasoning effort. Settings change only after the final selection.", ModelMenu: &protocol.ModelMenu{}}
	if s.session.ActiveTurnID != "" {
		result.Text += "\nWait for the current turn to finish before applying the selection."
	}
	if pages > 1 {
		result.Text += fmt.Sprintf("\nPage %d of %d.", page+1, pages)
	}
	start := page * protocol.ModelMenuPageSize
	for _, model := range available[start:min(start+protocol.ModelMenuPageSize, len(available))] {
		label := valueOr(strings.TrimSpace(model.DisplayName), model.ID)
		if model.Default {
			label += " (default)"
		}
		result.ModelMenu.Options = append(result.ModelMenu.Options, protocol.ModelOption{Args: "--menu " + model.ID, Label: permissionText(s.agent.redactor.Redact(label), 120)})
	}
	if page > 0 {
		result.ModelMenu.Options = append(result.ModelMenu.Options, protocol.ModelOption{Args: fmt.Sprintf("--page %d", page-1), Label: "Previous models"})
	}
	if page+1 < pages {
		result.ModelMenu.Options = append(result.ModelMenu.Options, protocol.ModelOption{Args: fmt.Sprintf("--page %d", page+1), Label: "Next models"})
	}
	result.ModelMenu.Options = append(result.ModelMenu.Options, protocol.ModelOption{Args: "--cancel", Label: "Cancel"})
	return result, result.ModelMenu.Validate()
}

func (s *sessionActor) modelReasoningMenu(model codexadapter.ModelInfo) (protocol.Result, error) {
	name := permissionText(s.agent.redactor.Redact(valueOr(strings.TrimSpace(model.DisplayName), model.ID)), 120)
	result := protocol.Result{Text: "Choose reasoning effort for " + name + ". Both choices apply to subsequent turns in this session.", ModelMenu: &protocol.ModelMenu{}}
	if s.session.ActiveTurnID != "" {
		result.Text += "\nWait for the current turn to finish before applying the selection."
	}
	seen := map[string]bool{}
	for _, effort := range model.ReasoningEfforts {
		if !protocol.ModelMenuToken(effort) || seen[effort] {
			continue
		}
		seen[effort] = true
		runes := []rune(effort)
		runes[0] = unicode.ToUpper(runes[0])
		label := string(runes)
		if effort == model.DefaultReasoningEffort {
			label += " (default)"
		}
		result.ModelMenu.Options = append(result.ModelMenu.Options, protocol.ModelOption{Args: model.ID + " " + effort, Label: permissionText(s.agent.redactor.Redact(label), 120)})
	}
	if len(result.ModelMenu.Options) == 0 {
		result.Text = "This model does not advertise reasoning effort choices. Apply " + name + " to subsequent turns?"
		result.ModelMenu.Options = append(result.ModelMenu.Options, protocol.ModelOption{Args: model.ID, Label: "Apply model"})
	}
	result.ModelMenu.Options = append(result.ModelMenu.Options, protocol.ModelOption{Args: "--menu", Label: "Back to models"}, protocol.ModelOption{Args: "--cancel", Label: "Cancel"})
	return result, result.ModelMenu.Validate()
}
