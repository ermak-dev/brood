package clientapi

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SessionDiagnostic contains only safe local configuration facts, never credentials.
type SessionDiagnostic struct {
	State          string `json:"state"`
	Origin         string `json:"origin,omitempty"`
	SessionPresent bool   `json:"session_present"`
	CAPresent      bool   `json:"ca_present"`
}

func diagnosticFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (128<<10)+1))
	if err != nil || len(data) > 128<<10 {
		return nil, fmt.Errorf("local session metadata exceeds limits")
	}
	return data, nil
}

// DiagnoseSessions inspects an existing store without creating it or contacting a server.
func DiagnoseSessions(dir string) (SessionDiagnostic, error) {
	result := SessionDiagnostic{State: "unconfigured"}
	if !filepath.IsAbs(dir) {
		return result, fmt.Errorf("session directory must be absolute")
	}
	current, err := diagnosticFile(filepath.Join(dir, "current"))
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("local server selection is unreadable")
	}
	origin, err := NormalizeOrigin(strings.TrimSpace(string(current)))
	if err != nil {
		return result, fmt.Errorf("local server selection is invalid")
	}
	result.Origin = origin
	result.State = "selected"
	raw, err := diagnosticFile(filepath.Join(dir, sessionFileName(origin)))
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("saved session metadata is unreadable")
	}
	var stored storedFile
	if json.Unmarshal(raw, &stored) != nil || stored.Origin != origin {
		return result, fmt.Errorf("saved session metadata is invalid")
	}
	result.SessionPresent = stored.Session != "" && stored.CSRF != ""
	result.CAPresent = stored.CAPEM != ""
	if result.SessionPresent {
		result.State = "saved; validity not checked"
	}
	return result, nil
}
