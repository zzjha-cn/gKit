package mcp

import (
	"github.com/ThinkInAIXYZ/go-mcp/server"
	"github.com/ThinkInAIXYZ/go-mcp/transport"
)

type McpServer struct {
	SSE        *server.Server
	SSEHandle  *transport.SSEHandler
	Streamable *server.Server
}
