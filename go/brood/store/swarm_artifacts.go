package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxArtifactBytes is the largest accepted version body.
	MaxArtifactBytes = 4 << 20
	// MaxSwarmArtifactBytes is the total body size allowed across all versions in a swarm.
	MaxSwarmArtifactBytes = 16 << 20
	// MaxSwarmArtifactVersions bounds metadata overhead from small publications.
	MaxSwarmArtifactVersions = 1024
	maxArtifactNameBytes     = 256
)

var (
	// ErrInvalidArtifactName indicates a name that cannot be stored.
	ErrInvalidArtifactName = errors.New("artifact name must be nonempty UTF-8, at most 256 bytes, with no control characters")
	// ErrInvalidArtifactMediaType indicates a type outside the artifact allowlist.
	ErrInvalidArtifactMediaType = errors.New("artifact media type must be text/plain, text/html, image/png, image/jpeg, image/gif, or image/webp")
	// ErrInvalidArtifactContent indicates empty content or content inconsistent with its media type.
	ErrInvalidArtifactContent = errors.New("artifact body must be nonempty and match its media type (text must be UTF-8)")
	// ErrArtifactTooLarge indicates that the body exceeds MaxArtifactBytes.
	ErrArtifactTooLarge = fmt.Errorf("artifact exceeds the %d-byte limit; publish a smaller body", MaxArtifactBytes)
	// ErrArtifactQuotaExceeded indicates that another version would exceed a swarm limit.
	ErrArtifactQuotaExceeded = fmt.Errorf("swarm artifact quota exceeded (%d bytes or %d versions); no version was published", MaxSwarmArtifactBytes, MaxSwarmArtifactVersions)
)

// ArtifactVersion is an immutable stored revision. ID identifies this exact
// version; Version numbers start at one for each name within a swarm.
type ArtifactVersion struct {
	ID        string
	Name      string
	Version   int
	MediaType string
	Content   []byte
}

// PublishArtifact appends a version in an active swarm. Names are trimmed and
// otherwise compared exactly. The caller must authorize access to the swarm.
func (s *Store) PublishArtifact(ctx context.Context, swarmID, name, mediaType string, content []byte) (ArtifactVersion, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxArtifactNameBytes || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return ArtifactVersion{}, ErrInvalidArtifactName
	}
	if len(content) > MaxArtifactBytes {
		return ArtifactVersion{}, ErrArtifactTooLarge
	}
	typ, err := validateArtifactMedia(mediaType, content)
	if err != nil {
		return ArtifactVersion{}, err
	}
	v := ArtifactVersion{ID: NewID(), Name: name, MediaType: typ}
	// BEGIN IMMEDIATE берёт блокировку записи до чтения номера и квоты,
	// поэтому другая публикация не может использовать тот же остаток.
	err = s.WithTx(ctx, func(ctx context.Context) error {
		db := s.conn(ctx)
		var used int64
		var count int
		err := db.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(CASE WHEN a.name = ? THEN a.version END), 0) + 1,
			       COALESCE(SUM(length(a.content)), 0), COUNT(a.id)
			FROM swarms s LEFT JOIN swarm_artifact_versions a ON a.swarm_id = s.id
			WHERE s.id = ? AND s.state = 'active'
			GROUP BY s.id`, name, swarmID).Scan(&v.Version, &used, &count)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if count >= MaxSwarmArtifactVersions || used+int64(len(content)) > MaxSwarmArtifactBytes {
			return ErrArtifactQuotaExceeded
		}
		_, err = db.ExecContext(ctx, `
			INSERT INTO swarm_artifact_versions (id, swarm_id, name, version, media_type, content)
			VALUES (?, ?, ?, ?, ?, ?)`, v.ID, swarmID, v.Name, v.Version, v.MediaType, content)
		return err
	})
	if err == ErrArtifactQuotaExceeded {
		return ArtifactVersion{}, err
	}
	if err != nil {
		return ArtifactVersion{}, opaqueStorageError(err, "could not store the artifact version")
	}
	v.Content = bytes.Clone(content)
	return v, nil
}

// GetArtifact returns the requested version only from the specified active swarm.
// The caller must authorize access to the swarm; the version ID is not a grant.
func (s *Store) GetArtifact(ctx context.Context, swarmID, id string) (ArtifactVersion, error) {
	var v ArtifactVersion
	err := s.conn(ctx).QueryRowContext(ctx, `
		SELECT a.id, a.name, a.version, a.media_type, a.content
		FROM swarm_artifact_versions a JOIN swarms s ON s.id = a.swarm_id
		WHERE a.swarm_id = ? AND a.id = ? AND s.state = 'active'`, swarmID, id).
		Scan(&v.ID, &v.Name, &v.Version, &v.MediaType, &v.Content)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactVersion{}, ErrNotFound
	}
	if err != nil {
		return ArtifactVersion{}, opaqueStorageError(err, "could not read the artifact version")
	}
	return v, nil
}

func validateArtifactMedia(mediaType string, content []byte) (string, error) {
	typ, params, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return "", ErrInvalidArtifactMediaType
	}
	switch typ {
	case "text/plain", "text/html":
		if len(params) != 0 && (len(params) != 1 || !strings.EqualFold(params["charset"], "utf-8")) {
			return "", ErrInvalidArtifactMediaType
		}
		if len(content) == 0 || !utf8.Valid(content) {
			return "", ErrInvalidArtifactContent
		}
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		if len(params) != 0 {
			return "", ErrInvalidArtifactMediaType
		}
		if http.DetectContentType(content) != typ {
			return "", ErrInvalidArtifactContent
		}
	default:
		return "", ErrInvalidArtifactMediaType
	}
	return typ, nil
}
