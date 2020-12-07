package migrations

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/pachyderm/pachyderm/src/client/pkg/errors"
	"github.com/pachyderm/pachyderm/src/server/pkg/obj"
	"github.com/sirupsen/logrus"
)

type Env struct {
	// TODO: etcd
	ObjectClient obj.Client
	Tx           *sqlx.Tx
}

func MakeEnv(objC obj.Client) Env {
	return Env{
		ObjectClient: objC,
	}
}

type Func func(ctx context.Context, env Env) error

type State struct {
	n      int
	prev   *State
	change Func
	name   string
}

func (s State) Apply(name string, fn Func) State {
	return State{
		prev:   &s,
		change: fn,
		n:      s.n + 1,
		name:   strings.ToLower(name),
	}
}

func (s State) Name() string {
	return s.name
}

func (s State) Number() int {
	return s.n
}

func InitialState() State {
	return State{
		name: "init",
		change: func(ctx context.Context, env Env) error {
			_, err := env.Tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS migrations (
				id BIGINT PRIMARY KEY,
				NAME VARCHAR(250) NOT NULL,
				start_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				end_time TIMESTAMP
			);
			INSERT INTO migrations (id, name, start_time, end_time) VALUES (0, 'init', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP) ON CONFLICT DO NOTHING;
			`)
			return err
		},
	}
}

func ApplyMigrations(ctx context.Context, db *sqlx.DB, baseEnv Env, state State) error {
	if state.prev != nil {
		if err := ApplyMigrations(ctx, db, baseEnv, *state.prev); err != nil {
			return err
		}
	}
	tx, err := db.BeginTxx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	env := baseEnv
	env.Tx = tx
	if err := func() error {
		if state.n == 0 {
			if err := state.change(ctx, env); err != nil {
				panic(err)
			}
		}
		_, err := tx.ExecContext(ctx, `LOCK TABLE migrations IN EXCLUSIVE MODE NOWAIT`)
		if err != nil {
			return err
		}
		if finished, err := isFinished(ctx, tx, state); err != nil {
			return err
		} else if finished {
			// skip migration
			logrus.Infof("migration %d already applied", state.n)
			return nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO migrations (id, name, start_time) VALUES ($1, $2, CURRENT_TIMESTAMP)`, state.n, state.name); err != nil {
			return err
		}
		logrus.Infof("applying migration %d %s", state.n, state.name)
		if err := state.change(ctx, env); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE migrations SET end_time = CURRENT_TIMESTAMP WHERE id = $1`, state.n); err != nil {
			return err
		}
		logrus.Infof("successfully applied migration %d", state.n)
		return nil
	}(); err != nil {
		if err := tx.Rollback(); err != nil {
			logrus.Error(err)
		}
		return err
	}
	return tx.Commit()
}

func BlockUntil(ctx context.Context, db *sqlx.DB, state State) error {
	const (
		schemaName = "public"
		tableName  = "migrations"
	)
	// poll database until this state is registered
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var tableExists bool
		if err := db.GetContext(ctx, &tableExists, `SELECT EXISTS (
			SELECT FROM information_schema.tables
			WHERE table_schema = $1
			AND table_name = $2
		)`, schemaName, tableName); err != nil {
			return err
		}
		if tableExists {
			var latest int
			if err := db.GetContext(ctx, &latest, `SELECT COALESCE(MAX(id), 0) FROM migrations`); err != nil && err != sql.ErrNoRows {
				return err
			}
			if latest == state.n {
				return nil
			} else if latest > state.n {
				return errors.Errorf("database state is newer than application is expecting")
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func isFinished(ctx context.Context, tx *sqlx.Tx, state State) (bool, error) {
	var name string
	if err := tx.GetContext(ctx, &name, `
	SELECT name
	FROM migrations
	WHERE id = $1
	`, state.n); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	if name != state.name {
		return false, errors.Errorf("migration mismatch %d HAVE: %s WANT: %s", state.n, name, state.name)
	}
	return true, nil
}