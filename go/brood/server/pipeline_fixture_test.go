package server

import (
	"context"
	"database/sql"

	"brood.dev/go/brood/store"
)

// insertSwarmGraph привязывает рою снимок графа напрямую, минуя
// производственную привязку AttachSwarmGraph с её обязательным источником:
// HTTP-тесты провода проверяют читателей и гейт, а не fail-closed
// источник. Снимок уходит вместе с роем (#206), ID запуска — это рой.
func insertSwarmGraph(ctx context.Context, ts *testServer, swarmID string, graph []byte, execution string) error {
	db, err := sql.Open("sqlite", ts.dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys=ON; PRAGMA busy_timeout=10000`); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `UPDATE swarms SET graph=?,execution=?,definition_name='test',definition_version=1,definition_revision='rev',updated_at='now' WHERE id=?`, graph, execution, swarmID)
	return err
}

// insertSwarmStep пишет шаг роя напрямую, минуя AppendPipelineVisit с его
// проверкой привязки роли (#208): тесты провода читают и такие строки,
// которые производственный путь больше не создаёт, — шаг с актором чужого
// роя или с нативным именем. Пустые поля пишутся как NULL.
func insertSwarmStep(ctx context.Context, ts *testServer, swarmID string, v store.PipelineVisit, nativeName string) error {
	db, err := sql.Open("sqlite", ts.dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys=ON; PRAGMA busy_timeout=10000`); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO swarm_steps(swarm_id,sequence,node_id,state,started_at,actor_uid,actor_incarnation,request_id,native_actor_name) VALUES (?,?,?,?,'now',NULLIF(?,''),NULLIF(?,0),NULLIF(?,''),NULLIF(?,''))`,
		swarmID, v.Sequence, v.NodeID, v.State, v.ActorUID, v.ActorIncarnation, v.RequestID, nativeName)
	return err
}
