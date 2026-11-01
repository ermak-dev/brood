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

// parseActorCreation separates the Brood role from the native actor definition.
func parseActorCreation(w http.ResponseWriter, r *http.Request, actor *ateapi.Actor) (string, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxResourceBody))
	if err != nil {
		writeDetail(w, 400, "malformed request body")
		return "", false
	}
	fields := map[string]json.RawMessage{}
	decoder := json.NewDecoder(bytes.NewReader(body))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		writeDetail(w, 400, "malformed request body")
		return "", false
	}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			writeDetail(w, 400, "malformed request body")
			return "", false
		}
		if _, duplicate := fields[key]; duplicate {
			writeDetail(w, 400, "duplicate actor field: "+key)
			return "", false
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			writeDetail(w, 400, "malformed request body")
			return "", false
		}
		fields[key] = raw
	}
	end, err := decoder.Token()
	var trailing any
	if err != nil || end != json.Delim('}') || decoder.Decode(&trailing) != io.EOF {
		writeDetail(w, 400, "malformed request body")
		return "", false
	}
	role := ""
	if raw, exists := fields["role"]; exists {
		if json.Unmarshal(raw, &role) != nil || (role != "worker" && role != "master") {
			writeDetail(w, 400, "role must be worker or master")
			return "", false
		}
		delete(fields, "role")
		body, _ = json.Marshal(fields)
	}
	if err := substrate.Unmarshal(body, actor); err != nil {
		apiError(w, r, err)
		return "", false
	}
	return role, true
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
