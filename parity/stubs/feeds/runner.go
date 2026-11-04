package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// A deterministic stand-in for the plugin runner the marketplace calls
// (PLUGIN_RUNNER_URL). POST /v1/execute answers by tool name:
//
//	echo      returns its input, plus what the server sent about the call
//	capable   first round: asks for the capabilities in input["requests"];
//	          later rounds: returns the broker's results, canonicalized
//	loop      asks for input["requests"] every round
//	emptyreq  asks for an empty capability list
//	badreq    asks with a value that is not JSON
//	reject    500 (Swift: 502 plugin_runner_rejected)
//	garbage   200 with a body that is not JSON
//	partial   200 with a body missing fuelConsumed
//
// A wrong bearer token is a 401.

const runnerToken = "parity-runner-token"

var uuidInText = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

type executeRequest struct {
	ModuleBase64 []byte            `json:"moduleBase64"`
	ToolName     string            `json:"toolName"`
	Permissions  []string          `json:"permissions"`
	Input        map[string]string `json:"input"`
	Fuel         int               `json:"fuel"`
}

func execute(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+runnerToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req executeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var output map[string]string
	switch req.ToolName {
	case "reject":
		http.Error(w, "rejected", http.StatusInternalServerError)
		return
	case "garbage":
		_, _ = w.Write([]byte("not json"))
		return
	case "partial":
		_, _ = w.Write([]byte(`{"output":{}}`))
		return
	case "echo":
		output = map[string]string{
			"_moduleSize": strconv.Itoa(len(req.ModuleBase64)), "_fuel": strconv.Itoa(req.Fuel),
			"_permissions": strings.Join(req.Permissions, ","),
		}
		for k, v := range req.Input {
			output[k] = v
		}
	case "capable":
		if results, ok := req.Input["_capabilityResults"]; ok {
			output = map[string]string{
				"results": canonical(results), "round": req.Input["_capabilityRound"], "value": req.Input["value"],
			}
		} else {
			output = map[string]string{"_capabilityRequests": req.Input["requests"]}
		}
	case "loop":
		output = map[string]string{"_capabilityRequests": req.Input["requests"]}
	case "emptyreq":
		output = map[string]string{"_capabilityRequests": "[]"}
	case "badreq":
		output = map[string]string{"_capabilityRequests": "not json"}
	default:
		http.Error(w, "unknown tool", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"output": output, "fuelConsumed": 7})
}

// canonical re-encodes a server's JSON with sorted keys and masked UUIDs: Swift
// dictionaries have no stable order, and a UUID inside a string is out of the
// comparer's reach.
func canonical(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return "unparseable"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "unencodable"
	}
	return uuidInText.ReplaceAllString(string(b), "<uuid>")
}
