package server

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"net/url"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/store"
)

//go:embed static/artifact_view.html
var artifactViewHTML string

var artifactViewTemplate = template.Must(template.New("artifact view").Parse(artifactViewHTML))

const artifactDocumentPolicy = "default-src 'none'; base-uri 'none'; form-action 'none'; frame-src 'none'; connect-src 'none'; style-src 'unsafe-inline'; img-src data:"

type artifactViewData struct {
	Swarm    string
	Name     string
	Version  int
	Document string
	Text     string
	Image    template.URL
}

func (s *Server) handleArtifactView(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-DNS-Prefetch-Control", "off")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")

	swarmID, versionID := r.PathValue("swarm"), r.PathValue("version")
	if !cleanViewPath(r) || !viewSegment(swarmID) || !viewSegment(versionID) {
		http.NotFound(w, r)
		return
	}
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		path := "/artifacts/" + swarmID + "/" + versionID
		http.Redirect(w, r, "/#/login?artifact="+url.QueryEscape(path), http.StatusSeeOther)
		return
	}

	allowed, err := s.app.Can(user.ID, app.ActionViewSwarm, app.SwarmRes(swarmID))
	if err != nil {
		http.Error(w, "Could not check access to the artifact. Try again.", http.StatusInternalServerError)
		return
	}
	if !allowed {
		http.NotFound(w, r)
		return
	}
	v, err := s.store.GetArtifact(r.Context(), swarmID, versionID)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Could not open the artifact. Try again.", http.StatusInternalServerError)
		return
	}

	swarm, err := s.store.GetSwarm(r.Context(), swarmID)
	if err != nil || swarm.State != "active" {
		http.NotFound(w, r)
		return
	}
	project, err := s.store.GetProject(r.Context(), swarm.ProjectID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data := artifactViewData{Swarm: project.Name + "/" + swarm.Name, Name: v.Name, Version: v.Version}
	policy := "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'; style-src 'unsafe-inline'; img-src data:"
	switch v.MediaType {
	case "text/html":
		// srcdoc не запрашивает отдельный ресурс с cookie. CSP оболочки
		// наследуется iframe; мета-политика действует до разметки артефакта.
		policy = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'; frame-src about:; style-src 'unsafe-inline'; img-src data:"
		doc, err := artifactMarkup(v.Content)
		if err != nil {
			http.Error(w, "Could not render the HTML example. Publish it as text or a supported image.", http.StatusUnsupportedMediaType)
			return
		}
		data.Document = `<!doctype html><meta http-equiv="Content-Security-Policy" content="` + artifactDocumentPolicy + `">` + doc
	case "text/plain":
		data.Text = string(v.Content)
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		// Только разрешенные растровые типы; base64 не содержит разметки.
		data.Image = template.URL("data:" + v.MediaType + ";base64," + base64.StdEncoding.EncodeToString(v.Content))
	default:
		http.Error(w, "This artifact type cannot be previewed. Publish it as text, HTML, or a supported image.", http.StatusUnsupportedMediaType)
		return
	}
	var page bytes.Buffer
	if err := artifactViewTemplate.Execute(&page, data); err != nil {
		http.Error(w, "Could not render the artifact. Try again.", http.StatusInternalServerError)
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", policy)
	_, _ = w.Write(page.Bytes())
}
