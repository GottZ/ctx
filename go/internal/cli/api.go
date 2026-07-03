// ctx api — a generic REST passthrough (workflow W5, design/03 §4.1). The
// project/types/issues surfaces are real REST mounts, NOT manage actions, so the
// old `ctx manage <action>` raw reach no longer covers them. `ctx api` restores
// script-level access to EVERY route from day one: it signs the request with the
// key from config, sends the (optional) JSON body verbatim, prints the response
// JSON, and — like the other REST-shaped commands — maps a success:false
// envelope to exit code 1 instead of the PrintJSON-and-exit-0 trap.
//
//	ctx api GET  /api/project
//	ctx api POST /api/project '{"identity":"manual:x","scope":"x"}'
//	echo '{"display_name":"X"}' | ctx api PATCH /api/project/<id>
//	ctx api DELETE /api/project/<id>

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// apiMethods is the closed set of methods the passthrough accepts (a typo like
// "GTE" should fail loudly, not send a malformed request).
var apiMethods = map[string]bool{
	http.MethodGet:    true,
	http.MethodPost:   true,
	http.MethodPut:    true,
	http.MethodPatch:  true,
	http.MethodDelete: true,
}

func apiCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "api <method> <path> [json]",
		Short: "Raw REST passthrough to any /api route (JSON body via arg or stdin)",
		Long: "Send a raw authenticated request to any API route. The method is one of\n" +
			"GET/POST/PUT/PATCH/DELETE; the path starts with '/' (e.g. /api/project). A\n" +
			"JSON body may be the third argument or piped via stdin; it is sent verbatim\n" +
			"and must be valid JSON. The response JSON is printed; a success:false\n" +
			"envelope exits 1 with the server's reason. This is the script-level reach\n" +
			"for the REST surfaces (project/types/issues) that are not manage actions.",
		Example: `  ctx api GET /api/project
  ctx api POST /api/project '{"identity":"manual:x","scope":"x"}'
  echo '{"display_name":"X"}' | ctx api PATCH /api/project/<id>`,
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			method := strings.ToUpper(args[0])
			if !apiMethods[method] {
				return fmt.Errorf("unknown method %q (use GET/POST/PUT/PATCH/DELETE)", args[0])
			}
			path := args[1]
			if !strings.HasPrefix(path, "/") {
				return fmt.Errorf("path %q must start with '/' (e.g. /api/project)", path)
			}

			// Body: third arg, else stdin (if piped). Empty = no body.
			raw := ""
			if len(args) == 3 {
				raw = args[2]
			} else if stdin, ok := ReadStdin(); ok {
				raw = stdin
			}
			var body any
			if strings.TrimSpace(raw) != "" {
				if !json.Valid([]byte(raw)) {
					return fmt.Errorf("request body is not valid JSON")
				}
				body = json.RawMessage(raw)
			}

			c, err := getClient()
			if err != nil {
				return err
			}
			resp, _, err := c.Do(method, path, body)
			if err != nil {
				return err
			}
			// Exit code follows the envelope, not the HTTP status: a body that has
			// no {success} field (rare on this surface) still prints and exits 0.
			if err := checkAPIEnvelope(resp); err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// checkAPIEnvelope maps a success:false envelope to a command error (exit 1). A
// response WITHOUT a success field (e.g. a bare array or /health) is not an
// error — it prints and exits 0.
func checkAPIEnvelope(resp []byte) error {
	var env struct {
		Success *bool  `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		return nil //nolint:nilerr // non-envelope body: print as-is, exit 0
	}
	if env.Success != nil && !*env.Success {
		if env.Error == "" {
			return fmt.Errorf("request failed: %s", truncateForError(resp))
		}
		return fmt.Errorf("%s", env.Error)
	}
	return nil
}
