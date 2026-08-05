package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/Gere2/neurofs/internal/runid"
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
	// A normal MCP server is long-lived and shared across calls. Its launch
	// environment cannot identify the current request, so explicitly suppress
	// ambient correlation until request-scoped IDs are part of the protocol.
	var err error
	ctx, err = runid.WithAvailability(ctx, runid.ForPersistentServer())
	if err != nil {
		return fmt.Errorf("declare MCP correlation: %w", err)
	}
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
// inbound message was a valid notification (a valid request object with
// no id) and no response should be written. JSON-RPC 2.0 §4.1: a
// notification never gets a response, even a successful one. Invalid
// values and malformed request objects are not notifications merely
// because they lack an id; they receive -32600 with a null id.
func (s *Server) handle(ctx context.Context, line []byte) (any, bool) {
	if !json.Valid(line) {
		var syntax any
		err := json.Unmarshal(line, &syntax)
		s.log.Printf("parse error: %v", err)
		return errResponse(nil, codeParseError, "parse error", err.Error()), false
	}

	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return s.handleRequest(ctx, line)
	}

	var batch []json.RawMessage
	if err := json.Unmarshal(trimmed, &batch); err != nil {
		return errResponse(nil, codeInvalidRequest, "invalid request", err.Error()), false
	}
	if len(batch) == 0 {
		return errResponse(nil, codeInvalidRequest, "invalid request", nil), false
	}

	responses := make([]Response, 0, len(batch))
	for _, raw := range batch {
		resp, drop := s.handleRequest(ctx, raw)
		if !drop {
			responses = append(responses, resp)
		}
	}
	if len(responses) == 0 {
		return nil, true
	}
	return responses, false
}

func (s *Server) handleRequest(ctx context.Context, line []byte) (Response, bool) {
	req, notification, err := decodeRequest(line)
	if err != nil {
		return errResponse(nil, codeInvalidRequest, "invalid request", err.Error()), false
	}

	resp, drop := s.dispatchMethod(ctx, req)
	// Notification suppression is the LAST step so a side-effecting
	// method (tools/call as fire-and-forget) still runs — only its
	// response is dropped on the wire.
	if notification {
		return Response{}, true
	}
	return resp, drop
}

// decodeRequest validates the JSON-RPC request envelope before deciding
// whether it is a notification. Decoding directly into Request is not
// sufficient: encoding/json accepts both `null` and `{}` as a zero-valued
// struct, which previously made invalid values disappear as notifications.
func decodeRequest(line []byte) (Request, bool, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(line, &members); err != nil {
		return Request{}, false, err
	}
	if members == nil {
		return Request{}, false, fmt.Errorf("request must be an object")
	}

	jsonrpc, ok := requestStringMember(members, "jsonrpc")
	if !ok || jsonrpc != "2.0" {
		return Request{}, false, fmt.Errorf("jsonrpc must be %q", "2.0")
	}
	method, ok := requestStringMember(members, "method")
	if !ok {
		return Request{}, false, fmt.Errorf("method must be a string")
	}

	req := Request{
		JSONRPC: jsonrpc,
		Method:  method,
	}
	id, hasID := members["id"]
	if hasID {
		if !validRequestID(id) {
			return Request{}, false, fmt.Errorf("id must be a string, number, or null")
		}
		req.ID = id
	}
	if params, hasParams := members["params"]; hasParams {
		trimmed := bytes.TrimSpace(params)
		if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
			return Request{}, false, fmt.Errorf("params must be an object or array")
		}
		req.Params = params
	}
	return req, !hasID, nil
}

func requestStringMember(members map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := members[name]
	if !ok {
		return "", false
	}
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return "", false
	}
	return *value, true
}

func validRequestID(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] == '"' {
		var value string
		return json.Unmarshal(trimmed, &value) == nil
	}
	var value json.Number
	return json.Unmarshal(trimmed, &value) == nil
}

func (s *Server) dispatchMethod(ctx context.Context, req Request) (Response, bool) {
	switch req.Method {
	case "initialize":
		return okResponse(req.ID, InitializeResult{
			ProtocolVersion: protocolVersion,
			ServerInfo:      ServerInfo{Name: "neurofs", Version: s.version},
			Capabilities:    Capabilities{},
		}), false

	case "notifications/initialized":
		return okResponse(req.ID, struct{}{}), false

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
