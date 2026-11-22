package server

import (
	"bytes"
	"encoding/json"
	"io"

	"net/http"
)

// actorCreation holds the role and origin of a new worker.
// originSet distinguishes an explicit empty origin from an absent field.
type actorCreation struct {
	role, origin string
	originSet    bool
}

// parseActorCreation reads the worker definition with its role and origin.
func parseActorCreation(w http.ResponseWriter, r *http.Request, actor *actorInput) (actorCreation, bool) {
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
	if raw, exists := fields["roleId"]; exists {
		if json.Unmarshal(raw, &creation.role) != nil || creation.role == "" {
			writeDetail(w, 400, "Select a project role; nothing changed")
			return actorCreation{}, false
		}
		delete(fields, "roleId")
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
	for key, target := range map[string]any{"metadata": &actor.Metadata, "actorTemplate": &actor.ActorTemplate} {
		raw, exists := fields[key]
		if !exists {
			continue
		}
		if err := json.Unmarshal(raw, target); err != nil {
			writeDetail(w, 400, key+" must be an object with a name")
			return actorCreation{}, false
		}
		delete(fields, key)
	}
	for key := range fields {
		writeDetail(w, 400, "unknown actor field: "+key+"; workers accept metadata.name, actorTemplate.name, roleId and origin")
		return actorCreation{}, false
	}
	return creation, true
}
