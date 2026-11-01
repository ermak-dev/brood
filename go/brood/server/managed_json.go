package server

import (
	"encoding/json"
	"net/http"

	"brood.dev/go/brood/substrate"
	"google.golang.org/protobuf/proto"
)

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
