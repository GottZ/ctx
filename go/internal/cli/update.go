// update.go — `ctx update <id>` (issue #408): the fields of `manage update` as
// flags. The raw route, `ctx manage update <id> '<json>'`, needs a JSON body
// built in the shell, and content from a file meant
// "$(jq -n --arg c "$(cat file)" '{content:$c}')" — when the file was not
// there, the substitution was empty and the update stored content "". Here a
// content file that is missing, unreadable or empty is an error before any
// request, and the answer is one line instead of the whole block.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GottZ/ctx/internal/store"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// updateFlags are the flags of `ctx update`; data turns them into the `data`
// object of manage update.
type updateFlags struct {
	contentFile string
	editOld     []string
	editNew     []string
	replaceAll  bool
	editsFile   string
	title       string
	category    string
	typeName    string
	tags        []string
	meta        []string
	metaUnset   []string
	jsonOut     bool
}

func updateCmd(getClient func() (*Client, error)) *cobra.Command {
	f := &updateFlags{}
	cmd := &cobra.Command{
		Use:   "update <block-id>",
		Short: "Change fields of a block (manage update, from flags)",
		Long: "Change fields of an existing block: the fields of `manage update` as flags, so no\n" +
			"JSON body is built in the shell. Fields without a flag stay as they are.\n\n" +
			"--content-file replaces the whole content with a file (- = stdin); one trailing\n" +
			"newline is dropped, as `ctx get --field content` prints one. A missing,\n" +
			"unreadable or empty file is an error before any request is sent.\n" +
			"--edit-old/--edit-new pairs change part of the content instead (in order; each\n" +
			"old must occur exactly once, or at least once with --replace-all, which marks\n" +
			"every pair); --edits-file takes the list as JSON, [{\"old\":…,\"new\":…,\"replace_all\":…}].\n" +
			"--tags replaces the tag set; --meta key=value sets\n" +
			"and --meta-unset removes one metadata key, every other key stays.\n\n" +
			"The answer is one line: id, title and updated_at, tab-separated; --json prints\n" +
			"the server's whole answer. A refused update exits 1 with the server's reason.",
		Example: `  ctx update 019c4923 --content-file notes.md
  cat notes.md | ctx update 019c4923 --content-file -
  ctx update 019c4923 --edit-old "status: open" --edit-new "status: done"
  ctx update 019c4923 --title "New title" --tags infra,db --meta source=claude-code-2026-11-21
  ctx update 019c4923 --meta-unset draft --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Everything the flags say is checked, and every file read, before
			// the client exists: a refusal here sends nothing.
			data, err := f.data(cmd.Flags())
			if err != nil {
				return err
			}
			c, err := getClient()
			if err != nil {
				return err
			}
			resp, err := c.Post("manage", map[string]any{"action": "update", "id": args[0], "data": data})
			if err != nil {
				return err
			}
			if f.jsonOut {
				// The envelope contract: the body first, then the exit code.
				PrintJSON(resp)
				return checkEnvelope(resp, envelopeRequired)
			}
			if err := checkEnvelope(resp, envelopeRequired); err != nil {
				return err
			}
			return printUpdateLine(resp)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.contentFile, "content-file", "", "replace the content with this file's text (- = stdin); a missing or empty file is an error")
	fl.StringArrayVar(&f.editOld, "edit-old", nil, "text to replace (repeatable; pairs with --edit-new in order)")
	fl.StringArrayVar(&f.editNew, "edit-new", nil, "replacement text (repeatable; \"\" deletes the old text, as \"new\": \"\" does)")
	fl.BoolVar(&f.replaceAll, "replace-all", false, "replace every occurrence of each --edit-old instead of exactly one")
	fl.StringVar(&f.editsFile, "edits-file", "", "the edit list as JSON from this file (- = stdin): [{\"old\":…,\"new\":…,\"replace_all\":…}]")
	fl.StringVar(&f.title, "title", "", "new title")
	fl.StringVar(&f.category, "category", "", "new category")
	fl.StringVar(&f.typeName, "type", "", "new block type from the registry")
	fl.StringSliceVar(&f.tags, "tags", nil, "replace the tag set (comma-separated; an empty value is refused)")
	fl.StringArrayVar(&f.meta, "meta", nil, "set one metadata key, key=value (repeatable; every other key stays)")
	fl.StringArrayVar(&f.metaUnset, "meta-unset", nil, "remove one metadata key (repeatable)")
	fl.BoolVar(&f.jsonOut, "json", false, "print the server's whole answer instead of the one line")
	return cmd
}

// data is the `data` object of manage update for these flags, or the reason
// the flags cannot be sent. Only flags that were given become fields.
func (f *updateFlags) data(fs *pflag.FlagSet) (map[string]any, error) {
	data := map[string]any{}
	if err := f.contentOrEdits(fs, data); err != nil {
		return nil, err
	}
	for _, field := range []struct{ flag, value string }{
		{"title", f.title}, {"category", f.category}, {"type", f.typeName},
	} {
		if !fs.Changed(field.flag) {
			continue
		}
		// An empty value is what a failed "$(…)" hands over; no block has an
		// empty title, category or type.
		if strings.TrimSpace(field.value) == "" {
			return nil, fmt.Errorf("--%s is empty — nothing was sent", field.flag)
		}
		data[field.flag] = field.value
	}
	if fs.Changed("tags") {
		tags := []string{}
		for _, t := range f.tags {
			if t = strings.TrimSpace(t); t != "" {
				tags = append(tags, t)
			}
		}
		// The same empty value would clear the tag set, and no version of the
		// old one is kept; clearing stays on the raw route, where it is spelt
		// out.
		if len(tags) == 0 {
			return nil, errors.New(`--tags is empty — nothing was sent; to clear the tags, send ctx manage update <id> '{"tags":[]}'`)
		}
		data["tags"] = tags
	}
	merge, err := f.metadataMerge()
	if err != nil {
		return nil, err
	}
	if merge != nil {
		data["metadata_merge"] = merge
	}
	if len(data) == 0 {
		return nil, errors.New("nothing to update: give --content-file, --edit-old/--edit-new, --edits-file, --title, --category, --type, --tags, --meta or --meta-unset")
	}
	return data, nil
}

// contentOrEdits puts the content or the edit list into data: a whole new
// content from --content-file, or edits from the flag pairs or --edits-file —
// one of the three.
func (f *updateFlags) contentOrEdits(fs *pflag.FlagSet, data map[string]any) error {
	pairs := len(f.editOld) > 0 || len(f.editNew) > 0
	contentFile, editsFile := fs.Changed("content-file"), fs.Changed("edits-file")
	switch {
	case contentFile && (pairs || editsFile):
		return errors.New("--content-file replaces the whole content; it does not go with --edit-old/--edit-new or --edits-file")
	case editsFile && pairs:
		return errors.New("--edits-file and --edit-old/--edit-new both give the edit list; use one of them")
	case f.replaceAll && !pairs:
		return errors.New("--replace-all marks the --edit-old/--edit-new pairs; in --edits-file set \"replace_all\" per edit")
	case len(f.editOld) != len(f.editNew):
		return fmt.Errorf("--edit-old and --edit-new come in pairs: got %d --edit-old and %d --edit-new", len(f.editOld), len(f.editNew))
	}
	switch {
	case contentFile:
		content, err := readUpdateFile("--content-file", f.contentFile)
		if err != nil {
			return err
		}
		data["content"] = content
	case editsFile:
		raw, err := readUpdateFile("--edits-file", f.editsFile)
		if err != nil {
			return err
		}
		edits, err := parseEditsFile(raw)
		if err != nil {
			return err
		}
		data["edits"] = edits
	case pairs:
		edits := make([]store.ContentEdit, len(f.editOld))
		for i := range f.editOld {
			edits[i] = store.ContentEdit{Old: f.editOld[i], New: &f.editNew[i], ReplaceAll: f.replaceAll}
		}
		data["edits"] = edits
	}
	return nil
}

// readUpdateFile reads the text a file flag names, "-" for stdin, and drops
// one trailing newline — the newline `ctx get --field content` ends its value
// with, and the one `ctx save` drops from stdin — so a value printed by one
// and read by the other comes back unchanged. A file that cannot be read, or
// holds nothing but whitespace, is an error: that is the case in which the
// shell route sent an empty content and the server stored it.
func readUpdateFile(flag, path string) (string, error) {
	var raw []byte
	var err error
	if path == "-" {
		if StdinIsTTY() {
			return "", fmt.Errorf("%s -: stdin is a terminal or a device, not piped text — nothing was sent", flag)
		}
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w — nothing was sent", flag, err)
	}
	text := strings.TrimSuffix(string(raw), "\n")
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s %s: the file is empty — nothing was sent", flag, path)
	}
	return text, nil
}

// parseEditsFile reads an --edits-file: a JSON list of edits with the keys of
// the wire (old, new, replace_all) and no others, so a misspelt key is an
// error here instead of a key the server never reads. What an edit may hold
// (an empty old, a missing new, the count) the server checks.
func parseEditsFile(raw string) ([]store.ContentEdit, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var edits []store.ContentEdit
	if err := dec.Decode(&edits); err != nil {
		return nil, fmt.Errorf("--edits-file: not a JSON list of edits: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("--edits-file: text after the JSON list")
	}
	if len(edits) == 0 {
		return nil, errors.New("--edits-file: the list holds no edit")
	}
	return edits, nil
}

// metadataMerge is the metadata_merge object of --meta and --meta-unset (nil
// when neither is given): a set key carries its value as a string, a removed
// key null. A key named twice is refused — which of the two was meant cannot
// be told.
func (f *updateFlags) metadataMerge() (map[string]any, error) {
	if len(f.meta) == 0 && len(f.metaUnset) == 0 {
		return nil, nil
	}
	merge := map[string]any{}
	for _, kv := range f.meta {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--meta %q: want key=value", kv)
		}
		if _, dup := merge[k]; dup {
			return nil, fmt.Errorf("--meta: the key %q is named twice", k)
		}
		merge[k] = v
	}
	for _, k := range f.metaUnset {
		if k == "" {
			return nil, errors.New("--meta-unset: the key is empty")
		}
		if _, dup := merge[k]; dup {
			return nil, fmt.Errorf("--meta-unset: the key %q is named twice", k)
		}
		merge[k] = nil
	}
	return merge, nil
}

// printUpdateLine prints the answer line of a successful update: the block's
// id, title and updated_at, tab-separated (lineCell keeps each a cell).
func printUpdateLine(resp []byte) error {
	var answer struct {
		Block struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			UpdatedAt string `json:"updated_at"`
		} `json:"block"`
	}
	if err := json.Unmarshal(resp, &answer); err != nil || answer.Block.ID == "" {
		return fmt.Errorf("unexpected answer, no block in it: %s", truncateForError(resp))
	}
	tty := StdoutIsTTY()
	fmt.Println(lineCell(answer.Block.ID, tty) + "\t" + lineCell(answer.Block.Title, tty) + "\t" + lineCell(answer.Block.UpdatedAt, tty))
	return nil
}
