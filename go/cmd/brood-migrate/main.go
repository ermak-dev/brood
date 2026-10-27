// Command brood-migrate переносит копии данных Django в Go-сервер.
package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `usage:
  brood-migrate users (--django-db PATH | --users-json PATH) --db PATH
    --source-id ID --bootstrap-admin NAME --out-report NEW_PATH
    [--existing-target yes]
  brood-migrate swarms --mapping PATH --atespaces SNAPSHOT --db PATH
    --existing-target yes

Offline copies only: stop the server, back up both databases, and never pass
production database paths. No cluster calls or live PostgreSQL connections are made.
A new destination is created privately; an existing compatible Go schema requires
explicit approval. Each command is one transaction. Discard failed new targets;
existing targets roll back on failure. Keep the original backups for cutover rollback.

users:
  SQLite input is read-only auth_user (id, username, is_superuser, is_active).
  For PostgreSQL, export those four columns to a JSON array on a trusted machine:
  [{"id":1,"username":"alice","is_superuser":false,"is_active":true}]
  This is an explicit column export, not Django dumpdata. No credentials are needed.
  source-id is a permanent unique installation identifier: reuse it on every rerun,
  including when switching between a SQLite copy and a JSON export of that source.
  Source IDs must be positive integers; ID/name conflicts are rejected, not merged.
  All imported users are disabled with random undisclosed Argon2id passwords.
  The private report contains REQUIRED ADMIN PASSWORD RESET instructions, not tokens.
  Existing exact imported users retain their current passwords and flags.
  bootstrap-admin must be a separate name, absent from the source. If absent in the
  target, create it with brood bootstrap-admin configured for the target copy, then
  sign in and POST /api/admin/users/{username}/password-reset with new_password.
  Explicitly approve each account with POST /api/admin/users/{username}/enable.
  A disabled same-name account cannot be recovered by server bootstrap: choose an
  unused recovery name instead. Source-inactive users must remain disabled.
  out-report must not exist. A report write failure rolls back the import.

swarms:
  Provision projects in the target first. Mapping is a nonempty JSON object:
  {"space":{"project":"main","owner":"alice","uid":"verified-uid"}}
  atespaces is a trusted JSON list snapshot from the intended installation:
  {"items":[{"metadata":{"name":"space","uid":"verified-uid"}}]}
  Every UID is mandatory and must match that snapshot. The operator must verify
  snapshot origin and freshness; this offline tool cannot attest live identity.
  Owners may be disabled; exact project membership is added without enabling them.
  Existing links must match owner, project, atespace name and UID. No moves occur.

Both commands delete all target sessions, including on rerun. Django sessions and
password hashes are never copied. Keep the Go server stopped throughout migration.
Run --help (or users --help / swarms --help) to show these instructions.
`

func main() { os.Exit(run(os.Args[1:], os.Stderr)) }

func run(args []string, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 1
	}
	if args[0] == "--help" || args[0] == "-h" || (len(args) == 2 && (args[0] == "users" || args[0] == "swarms") && (args[1] == "--help" || args[1] == "-h")) {
		fmt.Fprint(stderr, usage)
		return 0
	}
	var err error
	switch args[0] {
	case "users":
		err = cmdUsers(args[1:])
	case "swarms":
		err = cmdSwarms(args[1:])
	default:
		err = fmt.Errorf("unknown command %q; use --help", args[0])
	}
	if err != nil {
		fmt.Fprintf(stderr, "brood-migrate: %s\n", err)
		return 1
	}
	fmt.Fprintln(stderr, "Migration committed. Verify the database copy before cutover.")
	return 0
}
