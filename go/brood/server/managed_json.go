package server

import (
	"bytes"
	"encoding/json"
	"io"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"net/http"

	"brood.dev/go/brood/substrate"
	"google.golang.org/protobuf/proto"
)

// actorCreation holds managed creation fields outside the protobuf Actor.
// originSet distinguishes an explicit empty origin from an absent field.
type actorCreation struct {
	role, origin string
	originSet    bool
}

// parseActorCreation separates the Brood role and origin from the native actor definition.
func parseActorCreation(w http.ResponseWriter, r *http.Request, actor *ateapi.Actor) (actorCreation, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxResourceBody))
	if err != nil {
		writeDetail(w, 400, "malformed request body")
		return actorCreation{}, false
	}
	fields := map[string]json.RawMessage{}
	decoder := json.NewDecoder(bytes.NewReader(body))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		writeDetail(w, 400, "malformed request body")
		return actorCreation{}, false
	}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			writeDetail(w, 400, "malformed request body")
			return actorCreation{}, false
		}
		if _, duplicate := fields[key]; duplicate {
			writeDetail(w, 400, "duplicate actor field: "+key)
			return actorCreation{}, false
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			writeDetail(w, 400, "malformed request body")
			return actorCreation{}, false
		}
		fields[key] = raw
	}
	end, err := decoder.Token()
	var trailing any
	if err != nil || end != json.Delim('}') || decoder.Decode(&trailing) != io.EOF {
		writeDetail(w, 400, "malformed request body")
		return actorCreation{}, false
	}
	var creation actorCreation
	if raw, exists := fields["role"]; exists {
		if json.Unmarshal(raw, &creation.role) != nil || (creation.role != "worker" && creation.role != "master") {
			writeDetail(w, 400, "role must be worker or master")
			return actorCreation{}, false
		}
		delete(fields, "role")
	}
	if raw, exists := fields["origin"]; exists {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '"' || json.Unmarshal(raw, &creation.origin) != nil {
			writeDetail(w, 400, "origin must be a string")
			return actorCreation{}, false
		}
		creation.originSet = true
		delete(fields, "origin")
	}
	if creation.role != "" || creation.originSet {
		body, _ = json.Marshal(fields)
	}
	if err := substrate.Unmarshal(body, actor); err != nil {
		apiError(w, r, err)
		return actorCreation{}, false
	}
	return creation, true
}

func writeManagedList[T proto.Message](w http.ResponseWriter, native []T, managed []any) {
	rows := make([]json.RawMessage, 0, len(native)+len(managed))
	for _, item := range native {
		data, err := substrate.Marshal(item)
		if err != nil {
			writeDetail(w, 500, "cannot encode native inventory")
			return
		}
		rows = append(rows, data)
	}
	for _, item := range managed {
		data, err := json.Marshal(item)
		if err != nil {
			writeDetail(w, 500, "cannot encode managed inventory")
			return
		}
		rows = append(rows, data)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": rows})
}
