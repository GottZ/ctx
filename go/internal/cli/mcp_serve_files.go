package cli

// mcp_serve_files.go — the opt-in file parameters of `ctx mcp serve`
// (--allow-files, #409).
//
// content_file joins every mirrored tool whose schema has a top-level `content`
// string: the proxy reads the file and sends its text as `content`. output_file
// joins `get`: the proxy writes the block's content to the file and answers the
// path and byte count in its place. One predicate (schemaTakesContent) decides
// both where content_file is offered and where a call may use it, so the list
// and the call cannot drift apart; the schemas themselves stay the server's,
// with the two properties added and `content` no longer required where
// content_file may stand in for it.
//
// Confinement. A path is made absolute (relative paths resolve against the
// working directory the server started in), its symlinks are resolved as far
// as it exists, and the result must lie under one of the --allow-files
// directories (themselves resolved at start). The file is then opened through
// an os.Root of that directory, which refuses any component that leads outside
// it, so a symlink swapped in between the check and the open cannot widen the
// reach. A path that does not exist yet (output_file) is resolved through its
// existing parent. On Windows the same rules apply through filepath and os.Root
// (reserved device names such as NUL are refused there).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/ingest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpContentFileMaxBytes is the largest file content_file reads: the server's
// content cap (50 KB) — store, update, issue_create and issue_comment, the
// tools with a content string, refuse more. A larger file is refused before it
// is read whole and nothing is sent.
const mcpContentFileMaxBytes = ingest.MaxContentBytes

// mcpGetTool is the one tool output_file joins.
const mcpGetTool = "get"

// The two parameter names the proxy owns while --allow-files is set.
const (
	mcpContentFileParam = "content_file"
	mcpOutputFileParam  = "output_file"
)

// mcpFileRoots are the --allow-files directories, absolute and with their
// symlinks resolved, plus the directory relative paths resolve against.
type mcpFileRoots struct {
	dirs []string
	cwd  string
}

func newMCPFileRoots(dirs []string) (*mcpFileRoots, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("--allow-files: working directory: %w", err)
	}
	r := &mcpFileRoots{cwd: cwd}
	for _, d := range dirs {
		abs := d
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(cwd, abs)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("--allow-files %s: %w", d, err)
		}
		fi, err := os.Stat(real)
		if err != nil {
			return nil, fmt.Errorf("--allow-files %s: %w", d, err)
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("--allow-files %s: not a directory", d)
		}
		r.dirs = append(r.dirs, real)
	}
	return r, nil
}

// errMCPOutsideRoots is a path that does not resolve into an allowed directory.
var errMCPOutsideRoots = errors.New("is outside the directories this server may read and write")

// locate resolves path to the allowed directory it lies in and its path
// relative to that directory.
func (r *mcpFileRoots) locate(path string) (dir, rel string, err error) {
	if path == "" {
		return "", "", errors.New("is empty")
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(r.cwd, abs)
	}
	real, err := resolveExisting(filepath.Clean(abs))
	if err != nil {
		return "", "", err
	}
	for _, d := range r.dirs {
		rel, err := filepath.Rel(d, real)
		if err == nil && filepath.IsLocal(rel) {
			return d, rel, nil
		}
	}
	return "", "", fmt.Errorf("%w (--allow-files: %s)", errMCPOutsideRoots, strings.Join(r.dirs, ", "))
}

// resolveExisting resolves the symlinks of path as far as it exists and keeps
// the rest as written: a file that does not exist yet resolves through its
// nearest existing parent.
func resolveExisting(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path, nil
	}
	realParent, err := resolveExisting(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(path)), nil
}

// readContentFile reads a content_file: a regular, non-empty UTF-8 file of at
// most mcpContentFileMaxBytes under an allowed directory.
func (r *mcpFileRoots) readContentFile(path string) (string, error) {
	dir, rel, err := r.locate(path)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	// Stat before Open: opening a FIFO for reading would block the call.
	fi, err := root.Stat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", errors.New("does not exist")
	case err != nil:
		return "", err
	case !fi.Mode().IsRegular():
		return "", errors.New("is not a regular file")
	case fi.Size() > mcpContentFileMaxBytes:
		return "", fmt.Errorf("is %d bytes, more than the %d a content string may carry", fi.Size(), mcpContentFileMaxBytes)
	}
	f, err := root.Open(rel)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, mcpContentFileMaxBytes+1))
	switch {
	case err != nil:
		return "", err
	case len(data) > mcpContentFileMaxBytes:
		return "", fmt.Errorf("is more than the %d bytes a content string may carry", mcpContentFileMaxBytes)
	case len(data) == 0:
		return "", errors.New("is empty")
	case !utf8.Valid(data):
		return "", errors.New("is not UTF-8 text")
	}
	return string(data), nil
}

// mcpOutputTarget is a checked output_file: where get's content goes.
type mcpOutputTarget struct {
	param string // the path as the call gave it, for the answers
	dir   string
	rel   string
}

// outputTarget checks an output_file before the get is sent: under an allowed
// directory, in a directory that exists, and either absent or a regular file
// (which is then overwritten, keeping its mode).
func (r *mcpFileRoots) outputTarget(path string) (*mcpOutputTarget, error) {
	dir, rel, err := r.locate(path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	if li, err := root.Lstat(rel); err == nil && li.Mode()&fs.ModeSymlink != 0 {
		if _, err := root.Stat(rel); err != nil {
			return nil, fmt.Errorf("is a symbolic link that cannot be followed here: %w", err)
		}
	}
	fi, err := root.Stat(rel)
	switch {
	case err == nil && !fi.Mode().IsRegular():
		return nil, errors.New("is not a regular file")
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		if pi, perr := root.Stat(filepath.Dir(rel)); perr != nil || !pi.IsDir() {
			return nil, errors.New("is in a directory that does not exist")
		}
	default:
		return nil, err
	}
	return &mcpOutputTarget{param: path, dir: dir, rel: rel}, nil
}

// write writes content to the target through the os.Root of its directory.
// A new file is created 0600: a block's content may be anything up to
// credentials.
func (t *mcpOutputTarget) write(content string) (int, error) {
	root, err := os.OpenRoot(t.dir)
	if err != nil {
		return 0, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(t.rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.WriteString(f, content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// answer replaces the content of a get answer by the file it was written to:
// the same JSON object, with "content" swapped for "output_file": {"path",
// "bytes"} at its place. An error answer of the server passes unchanged and
// nothing is written.
func (t *mcpOutputTarget) answer(res *mcp.CallToolResult) *mcp.CallToolResult {
	if res.IsError {
		return res
	}
	fail := func(reason string) *mcp.CallToolResult {
		return refusalResult(fmt.Sprintf("%s %q: %s", mcpOutputFileParam, t.param, reason))
	}
	if len(res.Content) != 1 {
		return fail("the get answer is not one text; nothing was written")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		return fail("the get answer is not one text; nothing was written")
	}
	members, err := decodeObjectMembers(text.Text)
	if err != nil {
		return fail("the get answer is not a JSON object; nothing was written")
	}
	at := -1
	var content string
	for i, m := range members {
		if m.key == "content" {
			at = i
			if err := json.Unmarshal(m.value, &content); err != nil {
				return fail("the content of the get answer is not a string; nothing was written")
			}
		}
	}
	if at < 0 {
		return fail("the get answer carries no content; nothing was written")
	}
	n, err := t.write(content)
	if err != nil {
		return fail(fmt.Sprintf("writing failed: %v", err))
	}
	ref, _ := json.Marshal(map[string]any{"path": filepath.Join(t.dir, t.rel), "bytes": n})
	members[at] = jsonMember{key: mcpOutputFileParam, value: ref}
	out := *res
	out.Content = []mcp.Content{&mcp.TextContent{Text: encodeObjectMembers(members)}}
	return &out
}

// jsonMember is one member of a JSON object, its value kept as the server sent
// it, so a rewritten answer keeps the order and bytes of everything it does
// not replace.
type jsonMember struct {
	key   string
	value json.RawMessage
}

func decodeObjectMembers(text string) ([]jsonMember, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var members []jsonMember
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		members = append(members, jsonMember{key: key, value: value})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return members, nil
}

// encodeObjectMembers writes the members back as an object indented like the
// server's answers (two spaces).
func encodeObjectMembers(members []jsonMember) string {
	var raw bytes.Buffer
	raw.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			raw.WriteByte(',')
		}
		key, _ := json.Marshal(m.key)
		raw.Write(key)
		raw.WriteByte(':')
		raw.Write(m.value)
	}
	raw.WriteByte('}')
	var out bytes.Buffer
	if err := json.Indent(&out, raw.Bytes(), "", "  "); err != nil {
		return raw.String()
	}
	return out.String()
}

// schemaTakesContent reports whether a tool's input schema has a top-level
// `content` property of type string — the tools content_file joins.
func schemaTakesContent(schema any) bool {
	s, _ := schema.(map[string]any)
	props, _ := s["properties"].(map[string]any)
	content, _ := props["content"].(map[string]any)
	switch t := content["type"].(type) {
	case string:
		return t == "string"
	case []any:
		for _, v := range t {
			if v == "string" {
				return true
			}
		}
	}
	return false
}

// withFileParams is the remote's tools/list answer with the file parameters
// added. The remote's result is not modified.
func (r *mcpFileRoots) withFileParams(res *mcp.ListToolsResult) *mcp.ListToolsResult {
	out := *res
	out.Tools = make([]*mcp.Tool, len(res.Tools))
	for i, t := range res.Tools {
		tc := *t
		tc.InputSchema = r.schemaWithFileParams(t.Name, t.InputSchema)
		out.Tools[i] = &tc
	}
	return &out
}

func (r *mcpFileRoots) schemaWithFileParams(tool string, schema any) any {
	content := schemaTakesContent(schema)
	output := tool == mcpGetTool
	if !content && !output {
		return schema
	}
	var s map[string]any
	raw, err := json.Marshal(schema)
	if err != nil || json.Unmarshal(raw, &s) != nil {
		return schema
	}
	props, _ := s["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
		s["properties"] = props
	}
	where := fmt.Sprintf("Relative paths resolve against %s; the file must lie under %s after symlink resolution.",
		r.cwd, strings.Join(r.dirs, ", "))
	if content {
		props[mcpContentFileParam] = map[string]any{
			"type": "string",
			"description": fmt.Sprintf("instead of content: path of a local UTF-8 file (at most %d bytes) whose text is sent as content; give content or content_file, not both. %s",
				mcpContentFileMaxBytes, where),
		}
		if req, ok := s["required"].([]any); ok {
			kept := make([]any, 0, len(req))
			for _, v := range req {
				if v != "content" {
					kept = append(kept, v)
				}
			}
			s["required"] = kept
		}
	}
	if output {
		props[mcpOutputFileParam] = map[string]any{
			"type": "string",
			"description": "path of a local file the block's content is written to (created, or overwritten); the answer then carries output_file with path and byte count instead of the content. " +
				where,
		}
	}
	return s
}

// prepareCall applies the file parameters of one tools/call. It returns the
// arguments to send (unchanged when the call names neither parameter), the
// output target of a get with output_file, a refusal (nothing is sent), or an
// error from listing the remote tools.
func (r *mcpFileRoots) prepareCall(ctx context.Context, cs *mcp.ClientSession, name string, args json.RawMessage) (json.RawMessage, *mcpOutputTarget, string, error) {
	m, ok := argumentObject(args)
	if !ok {
		return args, nil, "", nil // not an object: the server answers it in its own words
	}
	contentFile, hasContentFile := m[mcpContentFileParam]
	outputFile, hasOutputFile := m[mcpOutputFileParam]
	if !hasContentFile && !hasOutputFile {
		return args, nil, "", nil
	}
	var out *mcpOutputTarget
	if hasOutputFile {
		target, refusal := r.prepareOutputFile(name, outputFile)
		if refusal != "" {
			return nil, nil, refusal, nil
		}
		out = target
		delete(m, mcpOutputFileParam)
	}
	if hasContentFile {
		refusal, err := r.prepareContentFile(ctx, cs, name, m, contentFile)
		if err != nil || refusal != "" {
			return nil, nil, refusal, err
		}
	}
	rewritten, err := json.Marshal(m)
	if err != nil {
		return nil, nil, fmt.Sprintf("cannot encode the arguments: %v", err), nil
	}
	return rewritten, out, "", nil
}

// argumentObject decodes tools/call arguments that are a JSON object.
func argumentObject(args json.RawMessage) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if len(args) == 0 || json.Unmarshal(args, &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

// stringArgument decodes an argument that must be a JSON string.
func stringArgument(raw json.RawMessage) (string, bool) {
	var s string
	return s, json.Unmarshal(raw, &s) == nil
}

func (r *mcpFileRoots) prepareOutputFile(name string, raw json.RawMessage) (*mcpOutputTarget, string) {
	path, ok := stringArgument(raw)
	if !ok {
		return nil, mcpOutputFileParam + " must be a string path; nothing was sent"
	}
	if name != mcpGetTool {
		return nil, fmt.Sprintf("%s is a parameter of get only, not of %s; nothing was sent", mcpOutputFileParam, name)
	}
	target, err := r.outputTarget(path)
	if err != nil {
		return nil, fmt.Sprintf("%s %q %v; nothing was sent", mcpOutputFileParam, path, err)
	}
	return target, ""
}

// prepareContentFile replaces content_file by the file's text as content in m.
// The remote's current schema of the tool decides whether content_file applies;
// a tool the remote does not list is sent without it, and the server refuses
// the unknown tool in its own words.
func (r *mcpFileRoots) prepareContentFile(ctx context.Context, cs *mcp.ClientSession, name string, m map[string]json.RawMessage, raw json.RawMessage) (string, error) {
	path, ok := stringArgument(raw)
	if !ok {
		return mcpContentFileParam + " must be a string path; nothing was sent", nil
	}
	if _, both := m["content"]; both {
		return "give content or content_file, not both; nothing was sent", nil
	}
	tool, err := lookupRemoteTool(ctx, cs, name)
	if err != nil {
		return "", err
	}
	delete(m, mcpContentFileParam)
	if tool == nil {
		return "", nil
	}
	if !schemaTakesContent(tool.InputSchema) {
		return fmt.Sprintf("%s takes no content, so %s does not apply; nothing was sent", name, mcpContentFileParam), nil
	}
	text, err := r.readContentFile(path)
	if err != nil {
		return fmt.Sprintf("%s %q %v; nothing was sent", mcpContentFileParam, path, err), nil
	}
	encoded, _ := json.Marshal(text)
	m["content"] = encoded
	return "", nil
}

// lookupRemoteTool is the remote's current definition of one tool, nil when it
// lists none of that name. Asked per call that names content_file, so the
// proxy keeps no copy of the tool list.
func lookupRemoteTool(ctx context.Context, cs *mcp.ClientSession, name string) (*mcp.Tool, error) {
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		if tool.Name == name {
			return tool, nil
		}
	}
	return nil, nil
}
