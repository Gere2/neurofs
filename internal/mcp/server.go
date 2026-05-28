package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
)

// Server reads newline-delimited JSON-RPC messages from in and writes
// responses to out. Diagnostics go to the logger constructed from
// errOut so stdout stays exclusive to protocol traffic.
type Server struct {
	in       io.Reader
	out      io.Writer
	log      *log.Logger
	version  string
	repoRoot string
}

func NewServer(in io.Reader, out, errOut io.Writer, version string) *Server {
	return &Server{
		in:      in,
		out:     out,
		log:     log.New(errOut, "mcp: ", log.LstdFlags),
		version: version,
	}
}

// SetRepoRoot pins all path-taking tools (view_file, search, scan, etc.)
// to root. The security traffic agent surfaced CRIT-2: without pinning,
// a malicious tool argument (`{"repo": "/etc"}`) turned neurofs_view_file
// into an arbitrary file reader on the host. When this is set, any
// non-empty `repo` argument that does not canonicalise to root is
// refused. The CLI calls this with the process cwd at server start so
// the default deployment is secure; library/test callers can leave it
// unset to keep the legacy caller-controlled behaviour.
func (s *Server) SetRepoRoot(root string) {
	s.repoRoot = root
}

// ctxKey is the private context-value key type; using a struct here
// makes accidental collisions impossible.
type ctxKey struct{ name string }

var ctxKeyRepoRoot = &ctxKey{"repoRoot"}

func withRepoRoot(ctx context.Context, root string) context.Context {
	if root == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyRepoRoot, root)
}

func repoRootFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRepoRoot).(string); ok {
		return v
	}
	return ""
}

// Run loops over input messages until EOF, ctx cancellation, or a
// fatal write error. EOF is a clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	scanner := bufio.NewScanner(s.in)
	// 64 MiB max — large MCP messages (multi-megabyte prompt contexts or
	// search results) must not crash the server. The MCP traffic agent
	// surfaced that the prior 4 MiB cap killed the server permanently
	// on a single >4 MiB line, forcing a host restart. Starting buffer
	// is 1 MiB so typical small messages avoid repeated growth.
	scanner.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	enc := json.NewEncoder(s.out)

	lines := make(chan []byte)
	scanErr := make(chan error, 1)
	go func() {
		defer close(lines)
		for scanner.Scan() {
			b := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- b:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			scanErr <- err
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-scanErr:
			return err
		case line, ok := <-lines:
			if !ok {
				return nil
			}
			if len(line) == 0 {
				continue
			}
			resp, drop := s.handle(withRepoRoot(ctx, s.repoRoot), line)
			if drop {
				continue
			}
			if err := enc.Encode(resp); err != nil {
				if errors.Is(err, io.ErrClosedPipe) {
					return nil
				}
				return fmt.Errorf("write response: %w", err)
			}
		}
	}
}

// handle returns the response and a drop flag. drop=true means the
// inbound message was a notification (no id) and no response should
// be written.
func (s *Server) handle(ctx context.Context, line []byte) (Response, bool) {
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		s.log.Printf("parse error: %v", err)
		return errResponse(nil, codeParseError, "parse error", err.Error()), false
	}

	notification := len(req.ID) == 0 || string(req.ID) == "null"

	if req.JSONRPC != "2.0" {
		if notification {
			return Response{}, true
		}
		return errResponse(req.ID, codeInvalidRequest, "invalid request", nil), false
	}

	switch req.Method {
	case "initialize":
		return okResponse(req.ID, InitializeResult{
			ProtocolVersion: protocolVersion,
			ServerInfo:      ServerInfo{Name: "neurofs", Version: s.version},
			Capabilities:    Capabilities{},
		}), false

	case "notifications/initialized":
		return Response{}, true

	case "tools/list":
		return okResponse(req.ID, ToolsListResult{Tools: toolsList()}), false

	case "tools/call":
		var params ToolCallParams
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				return errResponse(req.ID, codeInvalidParams, "invalid params", err.Error()), false
			}
		}
		return okResponse(req.ID, callTool(ctx, params)), false

	default:
		if notification {
			return Response{}, true
		}
		return errResponse(req.ID, codeMethodNotFound, fmt.Sprintf("method not found: %s", req.Method), nil), false
	}
}

func okResponse(id json.RawMessage, result any) Response {
	return Response{JSONRPC: "2.0", ID: id, Result: result}
}

func errResponse(id json.RawMessage, code int, msg string, data any) Response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return Response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &RPCError{Code: code, Message: msg, Data: data},
	}
}
