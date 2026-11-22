package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"brood.dev/go/brood/gitcredential"
)

// projectGitCredentialSchema — серверные Git credentials проекта (#173):
// отдельно от доставляемых актору var/file, со своей привязкой к точному
// repository. Ссылка декларации проекта хранится отдельной строкой и может
// указывать на отсутствующее имя: удаление credential отзывает доступ, а не
// отказывает из-за декларации.
//
//go:embed project_git_credential.sql
var projectGitCredentialSchema string

// verifyProjectGitCredentialSchema сверяет таблицы Git credentials.
func verifyProjectGitCredentialSchema(ctx context.Context, tx *sql.Tx) error {
	return checkSchema(ctx, tx, projectGitCredentialSchema)
}

// ErrGitCredentialLimit refuses a new credential beyond the per-project bound.
var ErrGitCredentialLimit = errors.New("project Git credential count exceeds limit")

// gitCredentialStorageError keeps database diagnostics from reflecting inputs.
func gitCredentialStorageError(err error) error {
	return opaqueStorageError(err, "project Git credential storage operation failed")
}

// ProjectGitAccess is the project's current server-side Git access: the
// declared repository and, when the declaration references a credential bound
// to exactly that repository, the credential. It is sensitive and INTERNAL:
// only the Git gateway may put the credential into an upstream request.
type ProjectGitAccess struct {
	Repository string
	Credential gitcredential.Credential
}

// HasCredential reports whether a bound credential was resolved.
func (a ProjectGitAccess) HasCredential() bool { return a.Credential.Name != "" }

// ProjectGitAccess reads the repository, the declaration reference and the
// referenced credential in one statement, so the binding is checked against
// the same snapshot without a write lock. A missing project is ErrNotFound.
func (s *Store) ProjectGitAccess(ctx context.Context, projectID string) (ProjectGitAccess, error) {
	var access ProjectGitAccess
	var origin, name, repository, username, token sql.NullString
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT r.origin, c.name, c.repository, c.username, c.token FROM projects p
		LEFT JOIN project_repositories r ON r.project_id = p.id
		LEFT JOIN project_git_credential_refs a ON a.project_id = p.id
		LEFT JOIN project_git_credentials c ON c.project_id = p.id AND c.name = a.name AND c.repository = r.origin
		WHERE p.id = ?`, projectID).Scan(&origin, &name, &repository, &username, &token)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectGitAccess{}, fmt.Errorf("%w: project", ErrNotFound)
	}
	if err != nil {
		return ProjectGitAccess{}, gitCredentialStorageError(err)
	}
	access.Repository = origin.String
	if !name.Valid {
		return access, nil
	}
	access.Credential = gitcredential.Credential{Name: name.String, Repository: repository.String, Username: username.String, Token: token.String}
	// Повреждённая строка не должна попасть в заголовок upstream-запроса.
	if gitcredential.Validate(access.Credential) != nil {
		return ProjectGitAccess{}, errors.New("project Git credential storage is inconsistent")
	}
	return access, nil
}

// ProjectGitCredentials returns names, bound repositories and the active flag,
// never usernames or tokens. A missing project is ErrNotFound.
func (s *Store) ProjectGitCredentials(ctx context.Context, projectID string) ([]gitcredential.Info, error) {
	infos := []gitcredential.Info{}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.GetProject(ctx, projectID); err != nil {
			return err
		}
		rows, err := s.conn(ctx).QueryContext(ctx, `SELECT c.name, c.repository, COALESCE(a.name IS NOT NULL AND c.repository = r.origin, 0) FROM project_git_credentials c
			LEFT JOIN project_git_credential_refs a ON a.project_id = c.project_id AND a.name = c.name
			LEFT JOIN project_repositories r ON r.project_id = c.project_id
			WHERE c.project_id = ? ORDER BY c.name`, projectID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var info gitcredential.Info
			if err := rows.Scan(&info.Name, &info.Repository, &info.Active); err != nil {
				return err
			}
			infos = append(infos, info)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, gitCredentialStorageError(err)
	}
	return infos, nil
}

// PutProjectGitCredential creates or replaces (rotates) one credential,
// including its repository binding. Values are validated before any write.
func (s *Store) PutProjectGitCredential(ctx context.Context, projectID string, c gitcredential.Credential) error {
	if err := gitcredential.Validate(c); err != nil {
		return err
	}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.GetProject(ctx, projectID); err != nil {
			return err
		}
		var count int
		var exists bool
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT count(*), EXISTS(SELECT 1 FROM project_git_credentials WHERE project_id = ? AND name = ?) FROM project_git_credentials WHERE project_id = ?`, projectID, c.Name, projectID).Scan(&count, &exists); err != nil {
			return err
		}
		if !exists && count >= gitcredential.MaxPerProject {
			return ErrGitCredentialLimit
		}
		_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO project_git_credentials(project_id,name,repository,username,token) VALUES (?,?,?,?,?)
			ON CONFLICT(project_id,name) DO UPDATE SET repository=excluded.repository, username=excluded.username, token=excluded.token`,
			projectID, c.Name, c.Repository, c.Username, c.Token)
		return err
	})
	if errors.Is(err, ErrGitCredentialLimit) {
		return ErrGitCredentialLimit
	}
	return gitCredentialStorageError(err)
}

// RemoveProjectGitCredentials deletes names in one statement. Absent names are
// harmless; a declaration reference to a removed name stays and resolves to nothing.
func (s *Store) RemoveProjectGitCredentials(ctx context.Context, projectID string, names []string) error {
	if len(names) == 0 || len(names) > gitcredential.MaxPerProject {
		return errors.New("Git credential removal requires 1 to 16 names")
	}
	args := []any{projectID}
	for _, name := range names {
		if err := gitcredential.ValidateName(name); err != nil {
			return err
		}
		args = append(args, name)
	}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.GetProject(ctx, projectID); err != nil {
			return err
		}
		_, err := s.conn(ctx).ExecContext(ctx, `DELETE FROM project_git_credentials WHERE project_id = ? AND name IN (?`+strings.Repeat(",?", len(names)-1)+`)`, args...)
		return err
	})
	return gitCredentialStorageError(err)
}

// SetProjectGitCredentialRef selects the credential used by the project Git gateway.
func (s *Store) SetProjectGitCredentialRef(ctx context.Context, projectID, name string) error {
	if name != "" {
		if err := gitcredential.ValidateName(name); err != nil {
			return err
		}
	}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.GetProject(ctx, projectID); err != nil {
			return err
		}
		if name == "" {
			_, err := s.conn(ctx).ExecContext(ctx, `DELETE FROM project_git_credential_refs WHERE project_id = ?`, projectID)
			return err
		}
		_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO project_git_credential_refs(project_id,name) VALUES (?,?) ON CONFLICT(project_id) DO UPDATE SET name=excluded.name`, projectID, name)
		return err
	})
	return gitCredentialStorageError(err)
}
