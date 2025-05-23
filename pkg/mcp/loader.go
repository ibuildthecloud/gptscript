package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/gptscript-ai/gptscript/pkg/hash"
	"github.com/gptscript-ai/gptscript/pkg/types"
	"github.com/gptscript-ai/gptscript/pkg/version"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

var (
	DefaultLoader = &Local{}
	DefaultRunner = DefaultLoader
)

type Local struct {
	nextID   int64
	lock     sync.Mutex
	sessions map[string]*Session
}

type Session struct {
	ID         string
	InitResult *mcp.InitializeResult
	Client     client.MCPClient
	Config     ServerConfig
}

type Config struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

type ServerConfig struct {
	DisableInstruction bool              `json:"disableInstruction"`
	Command            string            `json:"command"`
	Args               []string          `json:"args"`
	Env                map[string]string `json:"env"`
	Server             string            `json:"server"`
	URL                string            `json:"url"`
	BaseURL            string            `json:"baseURL,omitempty"`
	Headers            map[string]string `json:"headers"`
}

func (s *ServerConfig) GetBaseURL() string {
	if s.BaseURL != "" {
		return s.BaseURL
	}
	if s.Server != "" {
		return s.Server
	}
	return s.URL
}

func (l *Local) Load(ctx context.Context, tool types.Tool) (result []types.Tool, _ error) {
	if !tool.IsMCP() {
		return []types.Tool{tool}, nil
	}

	_, configData, _ := strings.Cut(tool.Instructions, "\n")
	var servers Config

	if err := json.Unmarshal([]byte(strings.TrimSpace(configData)), &servers); err != nil {
		return nil, fmt.Errorf("failed to parse MCP configuration: %w\n%s", err, configData)
	}

	if len(servers.MCPServers) == 0 {
		// Try to load just one server
		var server ServerConfig
		if err := json.Unmarshal([]byte(strings.TrimSpace(configData)), &server); err != nil {
			return nil, fmt.Errorf("failed to parse single MCP server configuration: %w\n%s", err, configData)
		}
		if server.Command == "" && server.URL == "" && server.Server == "" {
			return nil, fmt.Errorf("no MCP server configuration found in tool instructions: %s", configData)
		}
		servers.MCPServers = map[string]ServerConfig{
			"default": server,
		}
	}

	if len(servers.MCPServers) > 1 {
		return nil, fmt.Errorf("only a single MCP server definition is support")
	}

	for _, server := range slices.Sorted(maps.Keys(servers.MCPServers)) {
		session, err := l.loadSession(ctx, servers.MCPServers[server])
		if err != nil {
			return nil, fmt.Errorf("failed to load MCP session for server %s: %w", server, err)
		}

		return l.sessionToTools(ctx, session, tool.Name)
	}

	// This should never happen, but just in case
	return nil, fmt.Errorf("no MCP server configuration found in tool instructions: %s", configData)
}

func (l *Local) sessionToTools(ctx context.Context, session *Session, toolName string) ([]types.Tool, error) {
	tools, err := session.Client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to list tools: %w", err)
	}

	toolDefs := []types.Tool{{ /* this is a placeholder for main tool */ }}
	var toolNames []string

	for _, tool := range tools.Tools {
		var schema openapi3.Schema

		schemaData, err := json.Marshal(tool.InputSchema)
		if err != nil {
			panic(err)
		}

		if tool.Name == "" {
			// I dunno, bad tool?
			continue
		}

		if err := json.Unmarshal(schemaData, &schema); err != nil {
			return nil, fmt.Errorf("failed to unmarshal tool input schema: %w", err)
		}

		annotations, err := json.Marshal(tool.Annotations)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal tool annotations: %w", err)
		}

		toolDef := types.Tool{
			ToolDef: types.ToolDef{
				Parameters: types.Parameters{
					Name:        tool.Name,
					Description: tool.Description,
					Arguments:   &schema,
				},
				Instructions: types.MCPInvokePrefix + "." + tool.Name + " " + session.ID + " " + tool.Name,
			},
		}

		if string(annotations) != "{}" {
			toolDef.MetaData = map[string]string{
				"mcp-tool-annotations": string(annotations),
			}
		}

		if tool.Annotations.Title != "" && !slices.Contains(strings.Fields(tool.Annotations.Title), "as") {
			toolDef.Name = tool.Annotations.Title + " as " + tool.Name
		}

		toolDefs = append(toolDefs, toolDef)
		toolNames = append(toolNames, tool.Name)
	}

	main := types.Tool{
		ToolDef: types.ToolDef{
			Parameters: types.Parameters{
				Name:        toolName,
				Description: session.InitResult.ServerInfo.Name,
				Export:      toolNames,
			},
			MetaData: map[string]string{
				"bundle": "true",
			},
		},
	}

	if session.InitResult.Instructions != "" {
		data, _ := json.Marshal(map[string]any{
			"tools":        toolNames,
			"instructions": session.InitResult.Instructions,
		})
		toolDefs = append(toolDefs, types.Tool{
			ToolDef: types.ToolDef{
				Parameters: types.Parameters{
					Name: session.ID,
					Type: "context",
				},
				Instructions: types.EchoPrefix + "\n" + `# START MCP SERVER INFO: ` + session.InitResult.ServerInfo.Name + "\n" +
					`You have available the following tools from an MCP Server that has provided the following additional instructions` + "\n" +
					string(data) + "\n" +
					`# END MCP SERVER INFO` + "\n",
			},
		})

		main.ExportContext = append(main.ExportContext, session.ID)
	}

	toolDefs[0] = main
	return toolDefs, nil
}

func (l *Local) loadSession(ctx context.Context, server ServerConfig) (*Session, error) {
	id := hash.Digest(server)
	l.lock.Lock()
	existing, ok := l.sessions[id]
	l.lock.Unlock()
	if ok {
		return existing, nil
	}

	var (
		c   client.MCPClient
		err error
	)

	if server.Command != "" {
		env := make([]string, 0, len(server.Env))
		for k, v := range server.Env {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
		c, err = client.NewStdioMCPClient(server.Command, env, server.Args...)
		if err != nil {
			return nil, fmt.Errorf("failed to create MCP stdio client: %w", err)
		}
	} else {
		url := server.URL
		if url == "" {
			url = server.Server
		}
		c, err = client.NewSSEMCPClient(url, client.WithHeaders(server.Headers))
		if err != nil {
			return nil, fmt.Errorf("failed to create MCP HTTP client: %w", err)
		}
	}

	var initRequest mcp.InitializeRequest
	initRequest.Params.ClientInfo = mcp.Implementation{
		Name:    version.ProgramName,
		Version: version.Get().String(),
	}

	initResult, err := c.Initialize(ctx, initRequest)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize MCP client: %w", err)
	}

	result := &Session{
		ID:         id,
		InitResult: initResult,
		Client:     c,
		Config:     server,
	}

	l.lock.Lock()
	defer l.lock.Unlock()

	if existing, ok := l.sessions[id]; ok {
		return existing, c.Close()
	}

	if l.sessions == nil {
		l.sessions = make(map[string]*Session)
	}
	l.sessions[id] = result
	return result, nil
}
