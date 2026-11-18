package store

import "strings"

// Historical V58 DDL remains unchanged. A failed member of any/quorum may belong
// to a successful aggregate join; Store scope still requires its exact applied
// outcome/report, no open members, retained failure and current authority.
func ephemeralChildLifecycleV60(sql string) string {
	return strings.ReplaceAll(ephemeralChildLifecycleV58(sql), "v.outcome='fail'", "v.outcome IN ('success','fail')")
}

func ephemeralChildMixedCleanupMigration() string {
	statements := []string{"DROP TRIGGER ephemeral_child_lifecycle_insert_guard"}
	for _, object := range ephemeralChildLifecycleObjects {
		if object.name == "ephemeral_child_lifecycle_insert_guard" {
			statements = append(statements, ephemeralChildLifecycleV60(object.sql))
		}
	}
	return strings.Join(statements, ";")
}
