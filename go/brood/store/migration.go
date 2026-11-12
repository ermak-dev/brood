package store

// Миграции схемы (#274): развёрнутая база версии N < schemaVersion
// доводится до текущей схемы упорядоченными шагами N→N+1→…, а не
// пересоздаётся. Реестр DDL (registry.go) остаётся единственным описанием
// текущей схемы: свежая база создаётся по нему сразу, а мигрированная
// сверяется с ним той же проверкой, что база текущей версии при открытии.
// Open и schema gate выкладки (VerifyDatabaseSchema) идут одним кодом.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
)

// schemaMigration: шаг from → from+1. SQL и, если шагу нужен перенос
// данных, Go-код в той же транзакции после SQL.
type schemaMigration struct {
	from int
	sql  string
	data func(ctx context.Context, tx *sql.Tx) error
}

// schemaMigrations: цепочка от самой старой поддержанной версии до
// schemaVersion, по шагу на версию, по возрастанию. Изменение схемы
// поднимает schemaVersion, меняет DDL реестра и добавляет сюда шаг от
// развёрнутой версии. Итог шага обязан совпасть со свежей базой: тест
// мигрирует замороженную базу testdata/schema-<from>.sql. Шаг идёт с
// выключенными внешними ключами, как велит SQLite для перестройки таблиц:
// ON DELETE CASCADE в шаге не срабатывает, целостность проверяет
// foreign_key_check до фиксации. Триггеры при записи в шаге действуют.
var schemaMigrations = []schemaMigration{
	// 27: индекс запросов роя для подсчётов сводки списка роёв (#266).
	{from: 26, sql: swarmRequestSwarm},
}

// migrationPlan: шаги от found до want или отказ до любой записи: база
// новее релиза (старый образ на новой базе), версия вне цепочки или разрыв
// в ней.
func migrationPlan(found, want int, steps []schemaMigration) ([]schemaMigration, error) {
	if found > want {
		return nil, fmt.Errorf("%w: found %d, want %d; the database is newer than this release",
			errIncompatibleSchema, found, want)
	}
	var plan []schemaMigration
	for version := found; version < want; version++ {
		i := slices.IndexFunc(steps, func(step schemaMigration) bool { return step.from == version })
		if i < 0 {
			return nil, fmt.Errorf("%w: found %d, want %d; no migration from version %d",
				errIncompatibleSchema, found, want, version)
		}
		plan = append(plan, steps[i])
	}
	return plan, nil
}

// prepareSchema приводит базу соединения к схеме версии want: пустую
// создаёт по реестру, текущую сверяет, старую мигрирует по шагам. Полный
// план проверяется до первого шага, поэтому отказ плана ничего не меняет.
// Каждый шаг идёт в своей транзакции и перечитывает версию под блокировкой
// записи: конкурентный Open, успевший раньше, не даёт применить шаг дважды.
func prepareSchema(ctx context.Context, conn *sql.Conn, want int, steps []schemaMigration) error {
	// Каждый круг применяет шаг или видит, что его применил другой Open.
	for range len(steps) + 2 {
		pending, err := settleSchema(ctx, conn, want, steps)
		if err != nil || !pending {
			return err
		}
		err = withoutForeignKeys(ctx, conn, func() error { return migrateStep(ctx, conn, want, steps) })
		if err != nil {
			return err
		}
	}
	return errors.New("database schema did not reach the current version")
}

// settleSchema: одна транзакция, в которой пустая база (версия 0 без объектов)
// создаётся целиком, база версии want сверяется с реестром. Для старой
// версии проверяется только план, без записи, и возвращается true.
func settleSchema(ctx context.Context, conn *sql.Conn, want int, steps []schemaMigration) (bool, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin schema initialization: %w", err)
	}
	defer tx.Rollback()
	version, objects, err := schemaState(ctx, tx)
	if err != nil {
		return false, err
	}
	switch {
	case version == 0 && objects == 0:
		for _, module := range schemaModules {
			if _, err := tx.ExecContext(ctx, module.create()); err != nil {
				return false, fmt.Errorf("initialize database schema: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version=%d`, want)); err != nil {
			return false, fmt.Errorf("initialize schema version: %w", err)
		}
	case version == want:
		if err := verifySchemaModules(ctx, tx); err != nil {
			return false, err
		}
	default:
		if _, err := migrationPlan(version, want, steps); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit schema initialization: %w", err)
	}
	return false, nil
}

// schemaState: версия схемы и число объектов базы.
func schemaState(ctx context.Context, tx *sql.Tx) (version, objects int, err error) {
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return 0, 0, fmt.Errorf("inspect schema version: %w", err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`).Scan(&objects); err != nil {
		return 0, 0, fmt.Errorf("inspect database schema: %w", err)
	}
	return version, objects, nil
}

// migrateStep применяет очередной шаг в своей транзакции. Версия
// перечитывается в ней же: если другой Open уже дошёл до want, шага нет,
// сверку сделает следующий settleSchema.
func migrateStep(ctx context.Context, conn *sql.Conn, want int, steps []schemaMigration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer tx.Rollback()
	version, _, err := schemaState(ctx, tx)
	if err != nil || version >= want {
		return err
	}
	plan, err := migrationPlan(version, want, steps)
	if err != nil {
		return err
	}
	step := plan[0]
	if err := applyMigration(ctx, tx, step, want); err != nil {
		return fmt.Errorf("%w: migration from version %d to %d failed: %w",
			errIncompatibleSchema, step.from, step.from+1, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: migration from version %d to %d failed: commit: %w",
			errIncompatibleSchema, step.from, step.from+1, err)
	}
	return nil
}

// applyMigration выполняет тело шага: SQL, перенос данных, проверку
// внешних ключей и запись новой версии. Последний шаг до фиксации сверяет схему с
// реестром: несовпадение откатывает и его.
func applyMigration(ctx context.Context, tx *sql.Tx, step schemaMigration, want int) error {
	if step.sql != "" {
		if _, err := tx.ExecContext(ctx, step.sql); err != nil {
			return err
		}
	}
	if step.data != nil {
		if err := step.data(ctx, tx); err != nil {
			return err
		}
	}
	if err := foreignKeyCheck(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version=%d`, step.from+1)); err != nil {
		return err
	}
	if step.from+1 == want {
		return verifySchemaModules(ctx, tx)
	}
	return nil
}

// foreignKeyCheck ищет нарушения внешних ключей, которые шаг с выключенными
// ключами мог оставить. Отказ называет таблицы, но не строки.
func foreignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check foreign keys: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return rows.Err()
	}
	var table, parent string
	var row sql.NullInt64
	var key int
	if err := rows.Scan(&table, &row, &parent, &key); err != nil {
		return fmt.Errorf("check foreign keys: %w", err)
	}
	return fmt.Errorf("foreign key violation in table %s referencing %s", table, parent)
}

// withoutForeignKeys выполняет fn с выключенными внешними ключами
// соединения и возвращает их прежнее значение и при отказе fn. Внутри
// транзакции прагма не действует, поэтому она стоит вокруг шага на
// закреплённом соединении. Соединение, которому значение не вернулось,
// выбрасывается из пула, а prepareSchema отказывает.
func withoutForeignKeys(ctx context.Context, conn *sql.Conn, fn func() error) (err error) {
	var enabled int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&enabled); err != nil {
		return fmt.Errorf("inspect foreign keys: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for migration: %w", err)
	}
	defer func() {
		restore := context.WithoutCancel(ctx)
		var now int
		_, failed := conn.ExecContext(restore, fmt.Sprintf(`PRAGMA foreign_keys=%d`, enabled))
		if failed == nil {
			failed = conn.QueryRowContext(restore, `PRAGMA foreign_keys`).Scan(&now)
		}
		if failed == nil && now != enabled {
			failed = fmt.Errorf("foreign_keys is %d, want %d", now, enabled)
		}
		if failed != nil {
			conn.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, fmt.Errorf("restore foreign keys after migration: %w", failed))
		}
	}()
	return fn()
}
