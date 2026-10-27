// Привязка роёв только по проверенному офлайн-снимку atespaces.
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"brood.dev/go/brood/store"
)

type swarmLink struct {
	Project string `json:"project"`
	Owner   string `json:"owner"`
	UID     string `json:"uid"`
}

type uidSource func(string) (string, error)

func cmdSwarms(argv []string) error {
	f, err := parseFlags(argv, "mapping", "db", "atespaces", "existing-target")
	if err != nil {
		return err
	}
	for _, name := range []string{"mapping", "db", "atespaces"} {
		if _, err := f.get(name); err != nil {
			return err
		}
	}
	allow, err := existingTargetFlag(f)
	if err != nil {
		return err
	}
	if !allow {
		return fmt.Errorf("swarms requires --existing-target yes for a stopped database copy")
	}
	return runSwarms(f.values["mapping"], f.values["db"], f.values["atespaces"])
}

func runSwarms(mappingPath, dbPath, snapshotPath string) error {
	source, err := snapshotUIDSource(snapshotPath)
	if err != nil {
		return err
	}
	return migrateSwarms(mappingPath, dbPath, source)
}

func migrateSwarms(mappingPath, dbPath string, verified uidSource) error {
	links, err := readMapping(mappingPath)
	if err != nil {
		return err
	}
	if verified == nil {
		return fmt.Errorf("verified atespace UID snapshot is required")
	}
	names := make([]string, 0, len(links))
	uids := map[string]bool{}
	for name, link := range links {
		if uids[link.UID] {
			return fmt.Errorf("atespace UID reused in mapping")
		}
		uids[link.UID] = true
		uid, err := verified(name)
		if err != nil {
			return err
		}
		if uid == "" || uid != link.UID {
			return fmt.Errorf("atespace %q UID does not match the verified snapshot", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return withTarget(dbPath, true, func(tx *sql.Tx) error {
		for _, name := range names {
			link := links[name]
			var projectID, ownerID string
			if err := tx.QueryRow(`SELECT id FROM projects WHERE name=?`, link.Project).Scan(&projectID); err != nil {
				return fmt.Errorf("project %q: %w", link.Project, err)
			}
			if err := tx.QueryRow(`SELECT id FROM users WHERE username=?`, link.Owner).Scan(&ownerID); err != nil {
				return fmt.Errorf("owner %q: %w", link.Owner, err)
			}
			var oldProject, oldOwner, oldName string
			var oldUID sql.NullString
			err := tx.QueryRow(`SELECT project_id,owner_id,atespace_name,atespace_uid FROM swarms WHERE atespace_name=? OR atespace_uid=?`, name, link.UID).Scan(&oldProject, &oldOwner, &oldName, &oldUID)
			exists := err == nil
			if exists {
				var count int
				if err := tx.QueryRow(`SELECT count(*) FROM swarms WHERE atespace_name=? OR atespace_uid=?`, name, link.UID).Scan(&count); err != nil {
					return err
				}
				if count != 1 || oldProject != projectID || oldOwner != ownerID || oldName != name || !oldUID.Valid || oldUID.String != link.UID {
					return fmt.Errorf("atespace %q is already linked with different owner, project, name or UID; refusing to move it", name)
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO project_members(project_id,user_id) VALUES(?,?) ON CONFLICT(project_id,user_id) DO NOTHING`, projectID, ownerID); err != nil {
				return err
			}
			if exists {
				continue
			}
			now := time.Now().UTC().Format(time.RFC3339)
			if _, err := tx.Exec(`INSERT INTO swarms(id,project_id,owner_id,name,atespace_name,atespace_uid,state,created_at,updated_at) VALUES(?,?,?,?,?,?,'active',?,?)`, store.NewID(), projectID, ownerID, name, name, link.UID, now, now); err != nil {
				return fmt.Errorf("link atespace %q: %w", name, err)
			}
		}
		_, err := tx.Exec(`DELETE FROM sessions`)
		return err
	})
}

func readMapping(path string) (map[string]swarmLink, error) {
	var links map[string]swarmLink
	if err := readJSON(path, &links); err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, fmt.Errorf("mapping must be a nonempty object")
	}
	for name, link := range links {
		for _, v := range []string{name, link.Project, link.Owner, link.UID} {
			if strings.TrimSpace(v) == "" || strings.TrimSpace(v) != v {
				return nil, fmt.Errorf("mapping needs nonempty atespace name, project, owner and UID without surrounding whitespace")
			}
		}
	}
	return links, nil
}

// snapshotUIDSource не запускает kubectl и не меняет кластер. Снимок должен
// быть получен оператором из правильной установки до остановки сервера.
func snapshotUIDSource(path string) (uidSource, error) {
	var envelope map[string]json.RawMessage
	if err := readJSON(path, &envelope); err != nil {
		return nil, err
	}
	var items []struct {
		Metadata struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(envelope["items"], &items); err != nil {
		return nil, fmt.Errorf("atespace snapshot needs an items array: %w", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("atespace snapshot is empty")
	}
	names, uids := map[string]string{}, map[string]bool{}
	for _, item := range items {
		m := item.Metadata
		if m.Name == "" || m.UID == "" || names[m.Name] != "" || uids[m.UID] {
			return nil, fmt.Errorf("snapshot needs unique nonempty atespace names and UIDs")
		}
		names[m.Name] = m.UID
		uids[m.UID] = true
	}
	return func(name string) (string, error) { return names[name], nil }, nil
}
