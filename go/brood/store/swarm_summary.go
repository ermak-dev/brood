package store

// Сводки для строк списков (#234): список роёв проекта несёт исполнение,
// число открытых запросов и недоставленных ответов актору каждого роя
// (#266), список проектов — число роёв. Каждая сводка — одна выборка на
// весь набор: запрос на рой или проект делал бы список N+1, а именно так
// панель собирала сводку до #234 (#255).
// Набор ключей едет одним параметром JSON-массивом через json_each: число
// роёв не упирается в предел параметров SQLite, и текст запроса не зависит
// от размера набора.

import (
	"context"
	"encoding/json"
	"fmt"
)

// SwarmSummary is the execution of a swarm graph, the number of its open
// operator requests of either origin, node or actor, and the number of
// operator answers to actor requests whose delivery is failed or
// unconfirmed.
type SwarmSummary struct {
	Execution          string
	OpenRequests       int64
	UndeliveredAnswers int64
}

// SwarmSummaries returns the summary of each named swarm in one query. A
// swarm missing from the database is missing from the map. The caller
// names only swarms it may show: the summary does not check access.
func (s *Store) SwarmSummaries(ctx context.Context, ids []string) (map[string]SwarmSummary, error) {
	return swarmSummaries(ctx, s.conn(ctx), ids)
}

// swarmSummaries — SwarmSummaries на данном executor: тест считает им
// обращения к базе. Недоставленный ответ — тот, чью доставку панель
// предлагает повторить (#261): отвеченный запрос актора, ответ которого
// заведомо не ушёл (failed) или ушёл без квитанции (unconfirmed, #260);
// идущая (pending) и доставленная доставка оператора не ждут.
func swarmSummaries(ctx context.Context, q DB, ids []string) (map[string]SwarmSummary, error) {
	out := make(map[string]SwarmSummary, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	set, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `
		SELECT s.id, s.execution,
			(SELECT COUNT(*) FROM swarm_requests r WHERE r.swarm_id = s.id AND r.state = 'open'),
			(SELECT COUNT(*) FROM swarm_requests r WHERE r.swarm_id = s.id AND r.origin = 'actor'
				AND r.state = 'answered' AND r.delivery IN ('failed', 'unconfirmed'))
		FROM swarms s
		WHERE s.id IN (SELECT value FROM json_each(?))`, string(set))
	if err != nil {
		return nil, fmt.Errorf("swarm summaries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var summary SwarmSummary
		if err := rows.Scan(&id, &summary.Execution, &summary.OpenRequests, &summary.UndeliveredAnswers); err != nil {
			return nil, fmt.Errorf("swarm summaries: %w", err)
		}
		out[id] = summary
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("swarm summaries: %w", err)
	}
	return out, nil
}

// ProjectSwarmCounts counts, in one query, the swarms of each named project
// that the user's swarm list of that project shows: every swarm that is not
// deleted for an active administrator, otherwise the swarms the user holds a
// role in while a member of the project. A disabled or unknown user sees no
// swarm. Every named project is in the map, with zero if nothing is visible.
func (s *Store) ProjectSwarmCounts(ctx context.Context, userID string, projectIDs []string) (map[string]int64, error) {
	return projectSwarmCounts(ctx, s.conn(ctx), userID, projectIDs)
}

// projectSwarmCounts — ProjectSwarmCounts на данном executor. Правило
// видимости — то же, что у списка роёв проекта (app.ListSwarmsFor):
// администратору все неудалённые рои, участнику проекта — рои с его ролью.
// Список проверяет его в Go по рою, здесь оно — условие выборки, и
// расхождение двух мест ловит тест провода, сверяющий счёт с длиной списка.
func projectSwarmCounts(ctx context.Context, q DB, userID string, projectIDs []string) (map[string]int64, error) {
	out := make(map[string]int64, len(projectIDs))
	if len(projectIDs) == 0 {
		return out, nil
	}
	set, err := json.Marshal(projectIDs)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `
		SELECT p.value, COUNT(s.id)
		FROM json_each(?) p
		LEFT JOIN swarms s ON s.project_id = p.value AND s.state <> 'deleted'
			AND EXISTS (SELECT 1 FROM users u WHERE u.id = ? AND u.disabled = 0
				AND (u.is_admin = 1 OR (
					EXISTS (SELECT 1 FROM project_members pm WHERE pm.project_id = s.project_id AND pm.user_id = u.id)
					AND EXISTS (SELECT 1 FROM swarm_members m WHERE m.swarm_id = s.id AND m.user_id = u.id))))
		GROUP BY p.value`, string(set), userID)
	if err != nil {
		return nil, fmt.Errorf("project swarm counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var count int64
		if err := rows.Scan(&id, &count); err != nil {
			return nil, fmt.Errorf("project swarm counts: %w", err)
		}
		out[id] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("project swarm counts: %w", err)
	}
	return out, nil
}
