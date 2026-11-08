package mcp

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Guan0923/xinghe_rag/internal/catalog"
)

// loadCatalog reads one namespace's catalog document. Read failures become the
// same safe "MCP configuration cannot be read" error the Python service raises.
func (e *Endpoint) loadCatalog(ctx context.Context, namespace string) (*catalog.Data, error) {
	raw, err := e.opts.Catalog.Load(ctx, namespace)
	if err != nil {
		return nil, catalog.ErrConfigurationRead
	}
	if raw == nil {
		return catalog.NewData(), nil
	}
	data, err := catalog.Decode(raw)
	if err != nil {
		return nil, catalog.ErrConfigurationRead
	}
	return data, nil
}

// paginate ports mcp_http._paginate: the cursor is a decimal string of at most
// nine digits, the page size is 50 and an out-of-range cursor is an error.
func paginate(length int, cursor string) (start, end int, next string, err error) {
	if cursor != "" {
		if !isDecimal(cursor) || len(cursor) > 9 {
			return 0, 0, "", fmt.Errorf("Invalid list cursor")
		}
		start64, convErr := strconv.Atoi(cursor)
		if convErr != nil {
			return 0, 0, "", fmt.Errorf("Invalid list cursor")
		}
		start = start64
	}
	if start > length {
		return 0, 0, "", fmt.Errorf("List cursor is out of range")
	}
	end = start + listPageSize
	if end > length {
		end = length
	}
	if end < length {
		next = strconv.Itoa(start + listPageSize)
	}
	return start, end, next, nil
}

func isDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

// listPrompts ports NamespaceEndpoint.list_prompts.
func (e *Endpoint) listPrompts(ctx context.Context, namespace string, params *sdk.ListPromptsParams) (*sdk.ListPromptsResult, error) {
	data, err := e.loadCatalog(ctx, namespace)
	if err != nil {
		return nil, invalidParams(err.Error())
	}
	names := make([]string, 0, len(data.Prompts))
	for name := range data.Prompts {
		names = append(names, name)
	}
	sort.Strings(names)
	start, end, next, err := paginate(len(names), cursorOf(params))
	if err != nil {
		return nil, invalidParams(err.Error())
	}
	prompts := make([]*sdk.Prompt, 0, end-start)
	for _, name := range names[start:end] {
		prompt := data.Prompts[name]
		arguments := make([]*sdk.PromptArgument, 0, len(prompt.Arguments))
		for _, argument := range prompt.Arguments {
			arguments = append(arguments, &sdk.PromptArgument{
				Name:        argument.Name,
				Description: argument.Description,
				Required:    argument.Required,
			})
		}
		prompts = append(prompts, &sdk.Prompt{
			Name:        name,
			Description: prompt.Description,
			Arguments:   arguments,
		})
	}
	return &sdk.ListPromptsResult{Prompts: prompts, NextCursor: next}, nil
}

func cursorOf(params *sdk.ListPromptsParams) string {
	if params == nil {
		return ""
	}
	return params.Cursor
}

// promptHandler renders one stored prompt template. It reads the catalog on
// every call so admin updates are effective immediately.
func (e *Endpoint) promptHandler(namespace string) sdk.PromptHandler {
	return func(ctx context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
		if req == nil || req.Params == nil {
			return nil, invalidParams("Unknown prompt")
		}
		data, err := e.loadCatalog(ctx, namespace)
		if err != nil {
			return nil, invalidParams(err.Error())
		}
		prompt, ok := data.Prompts[req.Params.Name]
		if !ok {
			return nil, invalidParams(fmt.Sprintf("Unknown prompt: %s", req.Params.Name))
		}
		rendered, err := prompt.Render(req.Params.Arguments)
		if err != nil {
			return nil, invalidParams(err.Error())
		}
		messages := make([]*sdk.PromptMessage, 0, len(rendered))
		for _, message := range rendered {
			messages = append(messages, &sdk.PromptMessage{
				Role:    sdk.Role(message.Role),
				Content: &sdk.TextContent{Text: message.Text},
			})
		}
		return &sdk.GetPromptResult{Description: prompt.Description, Messages: messages}, nil
	}
}

// sdkPrompt converts a stored prompt definition into the registered form.
func sdkPrompt(name string, prompt catalog.PromptDefinition) *sdk.Prompt {
	arguments := make([]*sdk.PromptArgument, 0, len(prompt.Arguments))
	for _, argument := range prompt.Arguments {
		arguments = append(arguments, &sdk.PromptArgument{
			Name:        argument.Name,
			Description: argument.Description,
			Required:    argument.Required,
		})
	}
	return &sdk.Prompt{
		Name:        name,
		Description: prompt.Description,
		Arguments:   arguments,
	}
}
