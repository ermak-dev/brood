package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"brood.dev/go/brood/gitorigin"
)

// ErrGitLabDenied means current durable authority is absent or no longer valid.
var ErrGitLabDenied = errors.New("GitLab authority denied")

// ErrGitLabConflict means intent, snapshot, revision, or terminal state differs.
var ErrGitLabConflict = errors.New("GitLab intent or revision conflicts")

// GitLabPrincipal is supplied by a trusted authenticated adapter, never guest arguments.
// The store rechecks the durable identity; live native namespace checks belong to the server.
type GitLabPrincipal struct {
	ProjectID   string
	SwarmID     string
	AtespaceUID string
	ActorUID    string
	Incarnation int64
}

// GitLabBinding explicitly enables API access independently of Git transport.
// CredentialRef names a server-side configuration entry, never credential bytes.
// The configuring server must verify the numeric upstream project's repository
// before writing; the store validates only local identity and origin syntax.
type GitLabBinding struct {
	ProjectID       string
	Repository      string
	APIOrigin       string
	GitLabProjectID int64
	TargetBranch    string
	CredentialRef   string
	Enabled         bool
	Revision        int64
}

// GitLabGrant explicitly delegates only read, MR creation, or merge proposal.
// The granting human and exact actor incarnation are retained after revocation.
type GitLabGrant struct {
	ID              string
	Principal       GitLabPrincipal
	HumanID         string
	BindingRevision int64
	Read            bool
	Create          bool
	Propose         bool
	ExpiresAt       time.Time
	Revoked         bool
	Revision        int64
}

// GitLabOperation records intent, not execution permission. Exact retries return
// this record without replaying it. The payload itself is not stored; callers
// supply stable canonical bytes containing all upstream mutation arguments.
// Binding preserves the exact upstream target after configuration rotation.
type GitLabOperation struct {
	ID              string
	Principal       GitLabPrincipal
	Kind            string
	RequestID       string
	GrantID         string
	BindingRevision int64
	Binding         GitLabBinding
	PayloadHash     string
	Snapshot        GitLabMergeSnapshot
	State           string
	Revision        int64
	CreatedAt       string
	UpdatedAt       string
}

// GitLabMergeSnapshot is the immutable approval object. Both source and target
// are the bound numeric project; forks are unsupported. PipelineSHA must equal
// SourceSHA: merged-result and train pipelines require a later explicit design.
// No merge is executed by this storage layer.
type GitLabMergeSnapshot struct {
	Requester       GitLabPrincipal
	BindingRevision int64
	GitLabProjectID int64
	MRIID           int64
	SourceBranch    string
	SourceSHA       string
	PipelineID      int64
	PipelineSHA     string
	TargetBranch    string
	Squash          bool
}

// GitLabApproval is a single-use decision on the proposal's immutable snapshot.
// SessionHash is an internal hash, not a cookie/token and not a guest DTO.
type GitLabApproval struct {
	OperationID string
	HumanID     string
	SessionHash string
	ExpiresAt   time.Time
	State       string
	Revision    int64
}

// GitLabApprovalContext is passed to an explicit merge-approver policy.
// Ordinary swarm-management rights are necessary but never sufficient.
type GitLabApprovalContext struct {
	HumanID     string
	SessionHash string
	Snapshot    GitLabMergeSnapshot
}

// GitLabApproverPolicy runs inside the write transaction, at approval and claim.
// It must check the current explicit approver policy, using ctx for store reads;
// it must not perform network I/O, start a separate transaction, or authorize
// from model prose/generic swarm answers. Nil always denies. It cannot weaken
// the store's current user, session, project, swarm, or requester checks.
type GitLabApproverPolicy func(context.Context, GitLabApprovalContext) error

var gitlabReference = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var gitlabSHA = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

func gitlabBranch(branch string) bool {
	if branch == "" || len(branch) > 255 || branch == "@" || strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".") {
		return false
	}
	if strings.ContainsAny(branch, " ~^:?*[\\") || strings.Contains(branch, "..") || strings.Contains(branch, "@{") {
		return false
	}
	for _, part := range strings.Split(branch, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	for _, c := range branch {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func (s *Store) gitlabHuman(ctx context.Context, human, project, swarm string, owner bool) error {
	return s.gitlabHumanState(ctx, human, project, swarm, owner, true)
}

// Отзыв grant требует текущих прав, но не работоспособности роя или актора.
func (s *Store) gitlabHumanState(ctx context.Context, human, project, swarm string, owner, active bool) error {
	var allowed bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users u JOIN swarms sw ON sw.id=? AND sw.project_id=? AND (?=0 OR sw.state='active')
 WHERE u.id=? AND u.disabled=0 AND (u.is_admin=1 OR (
 EXISTS(SELECT 1 FROM project_members pm WHERE pm.project_id=sw.project_id AND pm.user_id=u.id) AND
 EXISTS(SELECT 1 FROM swarm_members sm WHERE sm.swarm_id=sw.id AND sm.user_id=u.id AND (sm.role='owner' OR (?=0 AND sm.role='editor'))))))`, swarm, project, active, human, owner).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrGitLabDenied
	}
	return nil
}

func (s *Store) gitlabIdentity(ctx context.Context, p GitLabPrincipal) error {
	var allowed bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM managed_actors a JOIN swarms sw ON sw.id=a.swarm_id
 WHERE a.uid=? AND a.incarnation=? AND a.swarm_id=? AND sw.project_id=? AND a.atespace_uid=? AND sw.atespace_uid=a.atespace_uid AND
 sw.state='active' AND a.phase='running' AND a.desired='running' AND a.stage='done' AND a.operation='start')`, p.ActorUID, p.Incarnation, p.SwarmID, p.ProjectID, p.AtespaceUID).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrGitLabDenied
	}
	return nil
}

// PutGitLabBinding creates (expected=0) or rotates a binding with revision CAS.
// Only a current active administrator can configure it. Any revision invalidates
// old grants and approvals, even when configuration values are unchanged.
func (s *Store) PutGitLabBinding(ctx context.Context, human string, b GitLabBinding, expected int64) (GitLabBinding, error) {
	if b.Repository == "" || !storedOrigin(b.Repository) || b.APIOrigin != "https://"+gitorigin.Host(b.Repository) || b.GitLabProjectID <= 0 || !gitlabBranch(b.TargetBranch) || !gitlabReference.MatchString(b.CredentialRef) || expected < 0 {
		return GitLabBinding{}, ErrGitLabDenied
	}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		var admin bool
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=? AND disabled=0 AND is_admin=1)`, human).Scan(&admin); err != nil {
			return err
		}
		if !admin {
			return ErrGitLabDenied
		}
		repository, err := s.ProjectRepository(ctx, b.ProjectID)
		if err != nil {
			return err
		}
		// Disable должен переживать дрейф repository, иначе старые grants оживут.
		if b.Enabled && repository != b.Repository {
			return ErrGitLabDenied
		}
		var revision int64
		err = s.conn(ctx).QueryRowContext(ctx, `SELECT revision FROM gitlab_bindings WHERE project_id=?`, b.ProjectID).Scan(&revision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if revision != expected {
			return ErrGitLabConflict
		}
		b.Revision = expected + 1
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO gitlab_bindings(project_id,repository,api_origin,gitlab_project_id,target_branch,credential_ref,enabled,revision) VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(project_id) DO UPDATE SET repository=excluded.repository,api_origin=excluded.api_origin,gitlab_project_id=excluded.gitlab_project_id,target_branch=excluded.target_branch,credential_ref=excluded.credential_ref,enabled=excluded.enabled,revision=excluded.revision`, b.ProjectID, b.Repository, b.APIOrigin, b.GitLabProjectID, b.TargetBranch, b.CredentialRef, b.Enabled, b.Revision)
		return err
	})
	if err != nil {
		return GitLabBinding{}, err
	}
	return b, nil
}

// GetGitLabBinding returns configuration for trusted server orchestration, not authorization.
func (s *Store) GetGitLabBinding(ctx context.Context, project string) (GitLabBinding, error) {
	var b GitLabBinding
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT project_id,repository,api_origin,gitlab_project_id,target_branch,credential_ref,enabled,revision FROM gitlab_bindings WHERE project_id=?`, project).Scan(&b.ProjectID, &b.Repository, &b.APIOrigin, &b.GitLabProjectID, &b.TargetBranch, &b.CredentialRef, &b.Enabled, &b.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return b, err
}
func (s *Store) gitlabCurrentBinding(ctx context.Context, project string) (GitLabBinding, error) {
	b, err := s.GetGitLabBinding(ctx, project)
	if err != nil {
		return b, err
	}
	repo, err := s.ProjectRepository(ctx, project)
	if err != nil {
		return b, err
	}
	if !b.Enabled || repo != b.Repository {
		return b, ErrGitLabDenied
	}
	return b, nil
}

// CreateGitLabGrant grants an exact current actor incarnation; no role inheritance.
// The granting human must currently own the swarm or be an administrator.
// A supplied ID is a stable, globally unique request identity (1-128 ASCII
// letters, digits, underscores, or hyphens). Exact active retries recheck current
// authority and return the original record. Changed payloads and revoked retries
// conflict. Empty IDs retain one-shot creation for trusted internal callers.
// Grants cannot be extended or edited: revoke and explicitly create another ID.
func (s *Store) CreateGitLabGrant(ctx context.Context, human string, g GitLabGrant) (GitLabGrant, error) {
	if g.ID == "" {
		g.ID = NewID()
	} else if len(g.ID) > 128 || strings.Trim(g.ID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") != "" {
		return GitLabGrant{}, ErrGitLabDenied
	}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		old, err := s.GetGitLabGrant(ctx, g.ID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		exists := err == nil
		if exists && (old.Principal != g.Principal || old.HumanID != human || old.BindingRevision != g.BindingRevision || old.Read != g.Read || old.Create != g.Create || old.Propose != g.Propose || !old.ExpiresAt.Equal(g.ExpiresAt) || old.Revoked) {
			return ErrGitLabConflict
		}
		if !g.ExpiresAt.After(time.Now()) || g.ExpiresAt.Year() > 2261 || (!g.Read && !g.Create && !g.Propose) {
			return ErrGitLabDenied
		}
		if err := s.gitlabHuman(ctx, human, g.Principal.ProjectID, g.Principal.SwarmID, true); err != nil {
			return err
		}
		if err := s.gitlabIdentity(ctx, g.Principal); err != nil {
			return err
		}
		b, err := s.gitlabCurrentBinding(ctx, g.Principal.ProjectID)
		if err != nil {
			return err
		}
		if b.Revision != g.BindingRevision {
			return ErrGitLabDenied
		}
		if exists {
			g = old
			return nil
		}
		g.HumanID = human
		g.Revoked = false
		g.Revision = 1
		g.ExpiresAt = g.ExpiresAt.UTC()
		p := g.Principal
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO gitlab_grants(id,project_id,swarm_id,atespace_uid,actor_uid,incarnation,human_id,binding_revision,can_read,can_create,can_propose,expires_at,revoked,revision) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,0,1)`, g.ID, p.ProjectID, p.SwarmID, p.AtespaceUID, p.ActorUID, p.Incarnation, g.HumanID, g.BindingRevision, g.Read, g.Create, g.Propose, g.ExpiresAt.UnixNano())
		return err
	})
	if err != nil {
		return GitLabGrant{}, err
	}
	return g, nil
}

func scanGitLabGrant(row *sql.Row) (GitLabGrant, error) {
	var g GitLabGrant
	var expires int64
	p := &g.Principal
	err := row.Scan(&g.ID, &p.ProjectID, &p.SwarmID, &p.AtespaceUID, &p.ActorUID, &p.Incarnation, &g.HumanID, &g.BindingRevision, &g.Read, &g.Create, &g.Propose, &expires, &g.Revoked, &g.Revision)
	g.ExpiresAt = time.Unix(0, expires).UTC()
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return g, err
}

const gitlabGrantColumns = `id,project_id,swarm_id,atespace_uid,actor_uid,incarnation,human_id,binding_revision,can_read,can_create,can_propose,expires_at,revoked,revision`

// GetGitLabGrant returns an internal historical record, not authorization.
func (s *Store) GetGitLabGrant(ctx context.Context, id string) (GitLabGrant, error) {
	return scanGitLabGrant(s.conn(ctx).QueryRowContext(ctx, `SELECT `+gitlabGrantColumns+` FROM gitlab_grants WHERE id=?`, id))
}

// RevokeGitLabGrant retains the immutable grant and all operation history.
func (s *Store) RevokeGitLabGrant(ctx context.Context, human, id string, revision int64) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		g, err := s.GetGitLabGrant(ctx, id)
		if err != nil {
			return err
		}
		if err := s.gitlabHumanState(ctx, human, g.Principal.ProjectID, g.Principal.SwarmID, true, false); err != nil {
			return err
		}
		return gitlabCAS(s.conn(ctx).ExecContext(ctx, `UPDATE gitlab_grants SET revoked=1,revision=revision+1 WHERE id=? AND revision=? AND revoked=0`, id, revision))
	})
}

func (s *Store) gitlabCheckGrant(ctx context.Context, p GitLabPrincipal, kind, id string) (GitLabGrant, error) {
	if kind != "read" && kind != "create" && kind != "propose" {
		return GitLabGrant{}, ErrGitLabDenied
	}
	if err := s.gitlabIdentity(ctx, p); err != nil {
		return GitLabGrant{}, err
	}
	b, err := s.gitlabCurrentBinding(ctx, p.ProjectID)
	if err != nil {
		return GitLabGrant{}, err
	}
	// Новый grant не должен оживлять прежнее намерение.
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT id FROM gitlab_grants WHERE project_id=? AND swarm_id=? AND atespace_uid=? AND actor_uid=? AND incarnation=? AND binding_revision=? AND revoked=0 AND expires_at>? AND (?='' OR id=?) AND ((?='read' AND can_read=1) OR (?='create' AND can_create=1) OR (?='propose' AND can_propose=1)) ORDER BY id`, p.ProjectID, p.SwarmID, p.AtespaceUID, p.ActorUID, p.Incarnation, b.Revision, time.Now().UnixNano(), id, id, kind, kind, kind)
	if err != nil {
		return GitLabGrant{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return GitLabGrant{}, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return GitLabGrant{}, err
	}
	for _, id := range ids {
		g, err := s.GetGitLabGrant(ctx, id)
		if err != nil {
			return GitLabGrant{}, err
		}
		if err := s.gitlabHuman(ctx, g.HumanID, p.ProjectID, p.SwarmID, false); err == nil {
			return g, nil
		} else if !errors.Is(err, ErrGitLabDenied) {
			return GitLabGrant{}, err
		}
	}
	return GitLabGrant{}, ErrGitLabDenied
}

// CheckGitLabAccess checks current durable authority in one transaction.
// A server must recheck after external work and before releasing private data.
func (s *Store) CheckGitLabAccess(ctx context.Context, p GitLabPrincipal, kind string) (GitLabGrant, error) {
	var g GitLabGrant
	err := s.WithTx(ctx, func(ctx context.Context) error {
		var err error
		g, err = s.gitlabCheckGrant(ctx, p, kind, "")
		return err
	})
	if err != nil {
		return GitLabGrant{}, err
	}
	return g, nil
}

func gitlabCAS(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrGitLabConflict
	}
	return nil
}
func gitlabHash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// ReserveGitLabMutation records MR creation intent before any external write.
// Only create is accepted; merge execution is intentionally absent.
func (s *Store) ReserveGitLabMutation(ctx context.Context, p GitLabPrincipal, kind, request string, payload []byte) (GitLabOperation, error) {
	if kind != "create" || len(payload) == 0 || len(payload) > 65536 {
		return GitLabOperation{}, ErrGitLabDenied
	}
	return s.reserveGitLabOperation(ctx, p, kind, request, gitlabHash(payload), GitLabMergeSnapshot{})
}

// ReserveGitLabProposal records a typed merge proposal without granting approval.
func (s *Store) ReserveGitLabProposal(ctx context.Context, p GitLabPrincipal, request string, snapshot GitLabMergeSnapshot) (GitLabOperation, error) {
	if snapshot.Requester != p || snapshot.MRIID <= 0 || snapshot.PipelineID <= 0 || !gitlabSHA.MatchString(snapshot.SourceSHA) || snapshot.PipelineSHA != snapshot.SourceSHA || !gitlabBranch(snapshot.SourceBranch) || snapshot.SourceBranch == snapshot.TargetBranch {
		return GitLabOperation{}, ErrGitLabDenied
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return GitLabOperation{}, err
	}
	return s.reserveGitLabOperation(ctx, p, "propose", request, gitlabHash(payload), snapshot)
}
func (s *Store) reserveGitLabOperation(ctx context.Context, p GitLabPrincipal, kind, request, hash string, snapshot GitLabMergeSnapshot) (GitLabOperation, error) {
	if request == "" || len(request) > 128 || strings.TrimSpace(request) != request {
		return GitLabOperation{}, ErrGitLabDenied
	}
	var op GitLabOperation
	err := s.WithTx(ctx, func(ctx context.Context) error {
		g, err := s.gitlabCheckGrant(ctx, p, kind, "")
		if err != nil {
			return err
		}
		b, err := s.gitlabCurrentBinding(ctx, p.ProjectID)
		if err != nil {
			return err
		}
		if kind == "propose" && (snapshot.BindingRevision != b.Revision || snapshot.GitLabProjectID != b.GitLabProjectID || snapshot.TargetBranch != b.TargetBranch) {
			return ErrGitLabDenied
		}
		var id string
		err = s.conn(ctx).QueryRowContext(ctx, `SELECT id FROM gitlab_operations WHERE project_id=? AND actor_uid=? AND incarnation=? AND kind=? AND request_id=?`, p.ProjectID, p.ActorUID, p.Incarnation, kind, request).Scan(&id)
		if err == nil {
			op, err = s.GetGitLabOperation(ctx, id)
			if err != nil {
				return err
			}
			if op.Principal != p || op.PayloadHash != hash {
				return ErrGitLabConflict
			}
			_, err = s.gitlabCheckGrant(ctx, p, kind, op.GrantID)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		op = GitLabOperation{ID: NewID(), Principal: p, Kind: kind, RequestID: request, GrantID: g.ID, BindingRevision: b.Revision, Binding: b, PayloadHash: hash, Snapshot: snapshot, State: "reserved", Revision: 1, CreatedAt: nowRFC3339(), UpdatedAt: nowRFC3339()}
		data := ""
		if kind == "propose" {
			encoded, _ := json.Marshal(snapshot)
			data = string(encoded)
		}
		bindingData, _ := json.Marshal(b)
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO gitlab_operations(id,project_id,swarm_id,atespace_uid,actor_uid,incarnation,kind,request_id,grant_id,binding_revision,binding_snapshot,payload_hash,snapshot,state,revision,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, op.ID, p.ProjectID, p.SwarmID, p.AtespaceUID, p.ActorUID, p.Incarnation, kind, request, g.ID, b.Revision, string(bindingData), hash, data, op.State, op.Revision, op.CreatedAt, op.UpdatedAt)
		return err
	})
	if err != nil {
		return GitLabOperation{}, err
	}
	return op, nil
}

// GetGitLabOperation reads internal history even after revocation. It must not
// be exposed to a guest without current authorization and principal checks.
func (s *Store) GetGitLabOperation(ctx context.Context, id string) (GitLabOperation, error) {
	var op GitLabOperation
	var snapshot, bindingData string
	p := &op.Principal
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT id,project_id,swarm_id,atespace_uid,actor_uid,incarnation,kind,request_id,grant_id,binding_revision,binding_snapshot,payload_hash,snapshot,state,revision,created_at,updated_at FROM gitlab_operations WHERE id=?`, id).Scan(&op.ID, &p.ProjectID, &p.SwarmID, &p.AtespaceUID, &p.ActorUID, &p.Incarnation, &op.Kind, &op.RequestID, &op.GrantID, &op.BindingRevision, &bindingData, &op.PayloadHash, &snapshot, &op.State, &op.Revision, &op.CreatedAt, &op.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return GitLabOperation{}, err
	}
	if err := json.Unmarshal([]byte(bindingData), &op.Binding); err != nil {
		return GitLabOperation{}, err
	}
	if snapshot != "" {
		if err := json.Unmarshal([]byte(snapshot), &op.Snapshot); err != nil {
			return GitLabOperation{}, err
		}
	}
	return op, nil
}

// TransitionGitLabOperation uses revision CAS. Only reserved->sending authorizes
// one outbound attempt, and rechecks the original grant. All post-send states
// are terminal and cannot automatically replay. Outcome recording intentionally
// survives loss of authority: it reports an already sent effect, not a new one.
func (s *Store) TransitionGitLabOperation(ctx context.Context, id string, revision int64, state string) (GitLabOperation, error) {
	var op GitLabOperation
	err := s.WithTx(ctx, func(ctx context.Context) error {
		var err error
		op, err = s.GetGitLabOperation(ctx, id)
		if err != nil {
			return err
		}
		if op.Revision != revision {
			return ErrGitLabConflict
		}
		allowed := op.State == "reserved" && (state == "sending" || state == "refused") || op.State == "sending" && (state == "completed" || state == "refused" || state == "unknown")
		if !allowed {
			return ErrGitLabConflict
		}
		if state == "sending" {
			if _, err := s.gitlabCheckGrant(ctx, op.Principal, op.Kind, op.GrantID); err != nil {
				return err
			}
		}
		op.State = state
		op.Revision++
		op.UpdatedAt = nowRFC3339()
		return gitlabCAS(s.conn(ctx).ExecContext(ctx, `UPDATE gitlab_operations SET state=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, state, op.UpdatedAt, id, revision))
	})
	if err != nil {
		return GitLabOperation{}, err
	}
	return op, nil
}

// RecoverGitLabOperations conservatively marks abandoned sending as unknown.
// Call only at exclusive startup after the preceding server has stopped. Never replay.
func (s *Store) RecoverGitLabOperations(ctx context.Context) error {
	_, err := s.conn(ctx).ExecContext(ctx, `UPDATE gitlab_operations SET state='unknown',revision=revision+1,updated_at=? WHERE state='sending'`, nowRFC3339())
	return err
}

func (s *Store) gitlabApprovalIdentity(ctx context.Context, op GitLabOperation, session string) (string, error) {
	current, err := s.GetGitLabOperation(ctx, op.ID)
	if err != nil {
		return "", err
	}
	if current != op || op.Kind != "propose" || op.State != "reserved" {
		return "", ErrGitLabDenied
	}
	if _, err := s.gitlabCheckGrant(ctx, op.Principal, "propose", op.GrantID); err != nil {
		return "", err
	}
	sess, err := s.GetSession(ctx, session)
	if err != nil {
		return "", err
	}
	if sess.ExpiresAt != nil && !sess.ExpiresAt.After(time.Now()) {
		return "", ErrGitLabDenied
	}
	if err := s.gitlabHuman(ctx, sess.UserID, op.Principal.ProjectID, op.Principal.SwarmID, false); err != nil {
		return "", err
	}
	return sess.UserID, nil
}

func (s *Store) gitlabApprovalAuthority(ctx context.Context, op GitLabOperation, session string, policy GitLabApproverPolicy) (string, error) {
	if policy == nil {
		return "", ErrGitLabDenied
	}
	human, err := s.gitlabApprovalIdentity(ctx, op, session)
	if err != nil {
		return "", err
	}
	if err := policy(ctx, GitLabApprovalContext{HumanID: human, SessionHash: session, Snapshot: op.Snapshot}); err != nil {
		return "", fmt.Errorf("%w: approver policy", ErrGitLabDenied)
	}
	current, err := s.gitlabApprovalIdentity(ctx, op, session)
	if err != nil {
		return "", err
	}
	if current != human {
		return "", ErrGitLabDenied
	}
	return human, nil
}

// ApproveGitLabMerge creates an immutable approval on an existing typed proposal.
// It requires current web-session authority and a separate explicit policy.
func (s *Store) ApproveGitLabMerge(ctx context.Context, id, session string, expires time.Time, policy GitLabApproverPolicy) (GitLabApproval, error) {
	var a GitLabApproval
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if !expires.After(time.Now()) || expires.Year() > 2261 {
			return ErrGitLabDenied
		}
		op, err := s.GetGitLabOperation(ctx, id)
		if err != nil {
			return err
		}
		if _, err := s.GetGitLabApproval(ctx, id); err == nil {
			return ErrGitLabConflict
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		human, err := s.gitlabApprovalAuthority(ctx, op, session, policy)
		if err != nil {
			return err
		}
		if !expires.After(time.Now()) {
			return ErrGitLabDenied
		}
		a = GitLabApproval{OperationID: id, HumanID: human, SessionHash: session, ExpiresAt: expires.UTC(), State: "approved", Revision: 1}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO gitlab_approvals(operation_id,human_id,session_hash,expires_at,state,revision) VALUES (?,?,?,?,'approved',1)`, id, human, session, expires.UnixNano())
		if IsUniqueViolation(err) {
			return ErrGitLabConflict
		}
		return err
	})
	if err != nil {
		return GitLabApproval{}, err
	}
	return a, nil
}

// GetGitLabApproval returns internal history, never current authorization.
func (s *Store) GetGitLabApproval(ctx context.Context, id string) (GitLabApproval, error) {
	var a GitLabApproval
	var expires int64
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT operation_id,human_id,session_hash,expires_at,state,revision FROM gitlab_approvals WHERE operation_id=?`, id).Scan(&a.OperationID, &a.HumanID, &a.SessionHash, &expires, &a.State, &a.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	a.ExpiresAt = time.Unix(0, expires).UTC()
	return a, err
}

// ClaimGitLabApproval atomically consumes exactly the approved snapshot. This is
// local single-use authority only, not proof of live GitLab checks or a merge.
// GitLab has no atomic expected-pipeline-ID precondition; execution stays absent.
func (s *Store) ClaimGitLabApproval(ctx context.Context, id string, revision int64, snapshot GitLabMergeSnapshot, policy GitLabApproverPolicy) (GitLabApproval, error) {
	var a GitLabApproval
	err := s.WithTx(ctx, func(ctx context.Context) error {
		var err error
		a, err = s.GetGitLabApproval(ctx, id)
		if err != nil {
			return err
		}
		op, err := s.GetGitLabOperation(ctx, id)
		if err != nil {
			return err
		}
		if a.Revision != revision || a.State != "approved" || snapshot != op.Snapshot {
			return ErrGitLabConflict
		}
		if !a.ExpiresAt.After(time.Now()) {
			return ErrGitLabDenied
		}
		human, err := s.gitlabApprovalAuthority(ctx, op, a.SessionHash, policy)
		if err != nil {
			return err
		}
		if human != a.HumanID || !a.ExpiresAt.After(time.Now()) {
			return ErrGitLabDenied
		}
		a.State = "claimed"
		a.Revision++
		return gitlabCAS(s.conn(ctx).ExecContext(ctx, `UPDATE gitlab_approvals SET state='claimed',revision=revision+1 WHERE operation_id=? AND revision=? AND state='approved'`, id, revision))
	})
	if err != nil {
		return GitLabApproval{}, err
	}
	return a, nil
}

// RevokeGitLabApproval preserves the decision and cannot reopen a consumed claim.
func (s *Store) RevokeGitLabApproval(ctx context.Context, human, id string, revision int64) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		op, err := s.GetGitLabOperation(ctx, id)
		if err != nil {
			return err
		}
		if err := s.gitlabHuman(ctx, human, op.Principal.ProjectID, op.Principal.SwarmID, true); err != nil {
			return err
		}
		return gitlabCAS(s.conn(ctx).ExecContext(ctx, `UPDATE gitlab_approvals SET state='revoked',revision=revision+1 WHERE operation_id=? AND revision=? AND state='approved'`, id, revision))
	})
}
