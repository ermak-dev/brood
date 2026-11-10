// Рои: личные atespace-области пользователей внутри проектов.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Swarm — строка таблицы swarms. State — состояние инфраструктуры
// (provisioning/active/deleting/error/deleted), Execution — состояние
// исполнения графа (idle/running/waiting/succeeded/failed/stopped);
// это отдельные поля и отдельные понятия (#206): остановка графа не
// меняет состояние инфраструктуры и наоборот. Graph — неизменяемый
// после привязки снимок документа, личность определения записана
// значениями, определение можно удалить, снимок — нет.
type Swarm struct {
	ID           string
	ProjectID    string
	OwnerID      string
	Name         string
	AtespaceName string
	AtespaceUID  *string
	State        string
	Retry        string
	CreatedAt    *string
	UpdatedAt    *string
	Graph        []byte
	Execution    string
	// DefinitionName, DefinitionVersion и DefinitionRevision — личность
	// определения, из которого снят граф: значения, не внешний ключ.
	DefinitionName     string
	DefinitionVersion  int64
	DefinitionRevision string
	// Source — снимок источника запуска, заполняется чтением.
	Source *PipelineSource
}

// swarmCols — список колонок swarms для SELECT.
const swarmCols = `id, project_id, owner_id, name, atespace_name, atespace_uid, state, created_at, updated_at, retry, graph, execution, definition_name, definition_version, definition_revision`

// scanSwarm читает строку swarms в фиксированном порядке колонок.
func scanSwarm(row interface{ Scan(...any) error }) (Swarm, error) {
	var w Swarm
	var createdAt, updatedAt string
	var graph []byte
	var defName, defRevision sql.NullString
	var defVersion sql.NullInt64
	err := row.Scan(&w.ID, &w.ProjectID, &w.OwnerID, &w.Name, &w.AtespaceName,
		&w.AtespaceUID, &w.State, &createdAt, &updatedAt, &w.Retry,
		&graph, &w.Execution, &defName, &defVersion, &defRevision)
	if err != nil {
		return w, err
	}
	w.CreatedAt = &createdAt
	w.UpdatedAt = &updatedAt
	w.Graph = graph
	w.DefinitionName, w.DefinitionRevision = defName.String, defRevision.String
	w.DefinitionVersion = defVersion.Int64
	return w, nil
}

// CreateSwarm вставляет рой; store проставляет created_at, updated_at
// и дефолтное состояние provisioning. Граф не передаётся: снимок
// привязывается отдельно (AttachSwarmGraph) и неизменяем после.
// Отсутствующие проект или владелец — нарушение внешнего ключа,
// занятый atespace_name — нарушение уникальности.
func (s *Store) CreateSwarm(ctx context.Context, w Swarm) error {
	if w.State == "" {
		w.State = "provisioning"
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		_, err := s.conn(ctx).ExecContext(ctx, `
			INSERT INTO swarms (id, project_id, owner_id, name, atespace_name, atespace_uid, state, created_at, updated_at, retry)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			w.ID, w.ProjectID, w.OwnerID, w.Name, w.AtespaceName, w.AtespaceUID,
			w.State, nowRFC3339(), nowRFC3339(), w.Retry)
		if err != nil {
			return wrapUnique(err, fmt.Sprintf("swarm %q or atespace %q", w.Name, w.AtespaceName))
		}
		_, err = s.conn(ctx).ExecContext(ctx,
			`INSERT INTO swarm_members (swarm_id, user_id, role) VALUES (?, ?, 'owner')`, w.ID, w.OwnerID)
		return err
	})
}

// GetSwarm возвращает рой по ID, вместе со снимком источника, если он есть.
func (s *Store) GetSwarm(ctx context.Context, id string) (Swarm, error) {
	w, err := scanSwarm(s.conn(ctx).QueryRowContext(ctx,
		`SELECT `+swarmCols+` FROM swarms WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return w, fmt.Errorf("%w: swarm %q", ErrNotFound, id)
	}
	if err != nil {
		return w, err
	}
	w.Source = s.swarmSource(ctx, id)
	return w, nil
}

// swarmSource читает снимок источника роя; отсутствие — не ошибка.
func (s *Store) swarmSource(ctx context.Context, id string) *PipelineSource {
	var origin, commit string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT origin,commit_sha FROM pipeline_sources WHERE swarm_id=?`, id).Scan(&origin, &commit)
	if err != nil {
		return nil
	}
	return &PipelineSource{Origin: origin, Commit: commit}
}

// GetSwarmByAtespace возвращает рой по имени его atespace.
func (s *Store) GetSwarmByAtespace(ctx context.Context, atespaceName string) (Swarm, error) {
	w, err := scanSwarm(s.conn(ctx).QueryRowContext(ctx,
		`SELECT `+swarmCols+` FROM swarms WHERE atespace_name = ?`, atespaceName))
	if err == sql.ErrNoRows {
		return w, fmt.Errorf("%w: atespace %q", ErrNotFound, atespaceName)
	}
	return w, err
}

// listSwarms выполняет выборку по фильтру в порядке создания.
func (s *Store) listSwarms(ctx context.Context, where, arg string) ([]Swarm, error) {
	rows, err := s.conn(ctx).QueryContext(ctx,
		`SELECT `+swarmCols+` FROM swarms WHERE `+where+` ORDER BY created_at, id`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var swarms []Swarm
	for rows.Next() {
		w, err := scanSwarm(rows)
		if err != nil {
			return nil, err
		}
		swarms = append(swarms, w)
	}
	return swarms, rows.Err()
}

// ListSwarmsByProject возвращает рои проекта в порядке создания.
func (s *Store) ListSwarmsByProject(ctx context.Context, projectID string) ([]Swarm, error) {
	swarms, err := s.listSwarms(ctx, `project_id = ?`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list swarms of project %s: %w", projectID, err)
	}
	return swarms, nil
}

// ListSwarmsByOwner возвращает рои владельца в порядке создания.
func (s *Store) ListSwarmsByOwner(ctx context.Context, ownerID string) ([]Swarm, error) {
	swarms, err := s.listSwarms(ctx, `owner_id = ?`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list swarms of owner %s: %w", ownerID, err)
	}
	return swarms, nil
}

// SetSwarmState меняет состояние роя (provisioning/active/deleting/error).
func (s *Store) SetSwarmState(ctx context.Context, id, state string) error {
	return s.updateSwarm(ctx, id, state, `state = ?, updated_at = ?`)
}

// SetSwarmProgress сохраняет состояние вместе с намерением повтора атомарно.
func (s *Store) SetSwarmProgress(ctx context.Context, id, state, retry string) error {
	result, err := s.conn(ctx).ExecContext(ctx,
		`UPDATE swarms SET state = ?, retry = ?, updated_at = ? WHERE id = ?`, state, retry, nowRFC3339(), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("%w: swarm %q", ErrNotFound, id)
	}
	return nil
}

// SetSwarmAtespaceUID сохраняет UID созданного atespace; nil стирает.
func (s *Store) SetSwarmAtespaceUID(ctx context.Context, id string, uid *string) error {
	return s.updateSwarm(ctx, id, uid, `atespace_uid = ?, updated_at = ?`)
}

// SetSwarmOwner передаёт рой другому пользователю (административная
// операция с проверкой членства на прикладном слое).
func (s *Store) SetSwarmOwner(ctx context.Context, id, ownerID string) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		current, err := s.GetSwarm(ctx, id)
		if err != nil {
			return err
		}
		if err := s.SetSwarmMember(ctx, id, ownerID, "owner"); err != nil {
			return err
		}
		if current.OwnerID != ownerID {
			if err := s.RemoveSwarmMember(ctx, id, current.OwnerID); err != nil {
				return err
			}
		}
		return s.updateSwarm(ctx, id, ownerID, `owner_id = ?, updated_at = ?`)
	})
}

// updateSwarm выполняет UPDATE swarms с данным фрагментом SET.
func (s *Store) updateSwarm(ctx context.Context, id string, arg any, set string) error {
	res, err := s.conn(ctx).ExecContext(ctx,
		`UPDATE swarms SET `+set+` WHERE id = ?`, arg, nowRFC3339(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: swarm %q", ErrNotFound, id)
	}
	return nil
}

// AttachSwarmGraph привязывает к рою снимок графа вместе с личностью
// определения и обязательным снимком источника (#206). Снимок
// валидируется до записи и неизменяем после: повторная привязка,
// замена и правка личности — отказ. Авторизация принадлежит вызывающему.
func (s *Store) AttachSwarmGraph(ctx context.Context, swarmID string, graph []byte, defName string, defVer int64, defRev string, source *PipelineSource) error {
	if !validPipelineField(swarmID) || len(graph) == 0 || len(graph) > MaxPipelineGraphBytes {
		return errors.New("invalid swarm graph or swarm ID")
	}
	if !validPipelineField(defName) || defVer < 1 || len(defRev) > maxPipelineFieldBytes {
		return errors.New("invalid workflow definition identity")
	}
	if _, err := executableGraph(graph); err != nil {
		return err
	}
	canonical, err := canonicalPipelineSource(source)
	if err != nil {
		return err
	}
	return s.WithTx(ctx, func(ctx context.Context) (err error) {
		if _, err = s.conn(ctx).ExecContext(ctx, `SAVEPOINT swarm_graph_attach`); err != nil {
			return err
		}
		defer func() {
			if err != nil {
				cleanup := context.WithoutCancel(ctx)
				_, rollbackErr := s.conn(cleanup).ExecContext(cleanup, `ROLLBACK TO swarm_graph_attach`)
				_, releaseErr := s.conn(cleanup).ExecContext(cleanup, `RELEASE swarm_graph_attach`)
				err = errors.Join(err, rollbackErr, releaseErr)
			}
		}()
		var exists bool
		if err = s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM swarms WHERE id=?)`, swarmID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: swarm %q", ErrNotFound, swarmID)
		}
		if _, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_sources(swarm_id,origin,commit_sha) VALUES (?,?,?)`, swarmID, canonical.Origin, canonical.Commit); err != nil {
			return err
		}
		result, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarms SET graph=?,execution='idle',definition_name=?,definition_version=?,definition_revision=?,updated_at=? WHERE id=? AND graph IS NULL`, graph, defName, defVer, defRev, nowRFC3339(), swarmID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// Рой уже несёт снимок: замена запрещена, повторный запуск идёт на нём же.
			return errors.New("swarm graph is already attached")
		}
		_, err = s.conn(ctx).ExecContext(ctx, `RELEASE swarm_graph_attach`)
		return err
	})
}

// SetSwarmExecution меняет состояние исполнения роя (#206):
// idle/running/waiting/succeeded/failed/stopped. Состояние
// инфраструктуры (state) не затрагивается.
func (s *Store) SetSwarmExecution(ctx context.Context, id, execution string) error {
	switch execution {
	case "idle", "running", "waiting", "succeeded", "failed", "stopped":
	default:
		return errors.New("invalid swarm execution")
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		// Возобновить граф, ждущий отменённого шлюза, нельзя: триггер
		// swarms_gate_resume держит это и при прямой записи.
		if activeExecution(execution) {
			var blocked bool
			if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM swarms w WHERE w.id=? AND w.execution NOT IN ('running','waiting') AND EXISTS (SELECT 1 FROM swarm_steps s WHERE s.swarm_id=w.id AND s.state='waiting' AND NOT EXISTS (SELECT 1 FROM swarm_requests q WHERE q.swarm_id=s.swarm_id AND q.sequence=s.sequence AND q.origin='node' AND q.state='open')))`, id).Scan(&blocked); err != nil {
				return err
			}
			if blocked {
				return fmt.Errorf("%w: the graph waits on a cancelled gate", ErrPipelineVisitConflict)
			}
		}
		res, err := s.conn(ctx).ExecContext(ctx,
			`UPDATE swarms SET execution = ?, updated_at = ? WHERE id = ?`, execution, nowRFC3339(), id)
		if err != nil {
			return err
		}
		if count, _ := res.RowsAffected(); count == 0 {
			return fmt.Errorf("%w: swarm %q", ErrNotFound, id)
		}
		return nil
	})
}
