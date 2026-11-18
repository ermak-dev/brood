package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// BUG: Pre-start role view eval читает сегодняшний каталог вместо pinned graph.
// HYPOTHESIS: При Graph=nil selection из immutable binding не участвует в
// swarmRoleGraph, и catalogue drift меняет набор ролей перед первым start.
func TestSwarmEvalRolesStayPinnedBeforeFirstStart(t *testing.T) {
	for _, drift := range []string{"changed", "removed"} {
		t.Run(drift, func(t *testing.T) {
			ctx := context.Background()
			s, in := evalFixture(t)
			graph := strings.Replace(startBuildFirst, "name: build-first", "name: approve", 1)
			publish(t, s, "two", map[string]string{"approve": graph})
			binding, err := s.CreateSwarmEvalBinding(ctx, in, evalAuthorize)
			mustNil(t, err)
			if drift == "changed" {
				publish(t, s, "two", map[string]string{"approve": strings.Replace(graph, "role: build", "role: review", 1)})
			} else {
				publish(t, s, "two", map[string]string{"other": strings.Replace(startApprove, "name: approve", "name: other", 1)})
			}
			before := databaseRows(t, s.db)
			for _, selected := range []string{"", binding.Definition} {
				view, err := s.ListSwarmRoles(ctx, in.CandidateSwarmID, selected)
				if err != nil || view.Graph == nil || !view.Graph.Pinned || view.Graph.Name != binding.Definition || view.Graph.Version != binding.DefinitionVersion || view.Graph.Revision != binding.DefinitionRevision || !reflect.DeepEqual(view.Graph.Roles, []string{"build"}) {
					t.Fatalf("%s catalogue changed pinned pre-start roles: %+v %v", drift, view, err)
				}
			}
			if _, err := s.ListSwarmRoles(ctx, in.CandidateSwarmID, "other"); !errors.Is(err, ErrSwarmGraphPinned) {
				t.Fatalf("eval role view accepted another definition: %v", err)
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("pinned role projection changed execution or binding history")
			}
			_, err = s.ReserveManagedActor(ctx, managedRequest("eval-role-actor", "two", 202))
			mustNil(t, err)
			setActor(t, s, "eval-role-actor", "phase='running',stage='done',operation='start'")
			view, err := s.BindSwarmRoleAs(ctx, SwarmRoleAssignment{SwarmID: in.CandidateSwarmID, UserID: in.LauncherID, Role: "build", ActorUID: "eval-role-actor", ActorIncarnation: 1}, allow)
			mustNil(t, err)
			if view.Graph == nil || !view.Graph.Pinned || len(view.Roles) != 1 || !view.Roles[0].InGraph || view.Roles[0].Binding == nil {
				t.Fatalf("binding did not use the selected immutable role graph: %+v", view)
			}
			if _, err := s.StartSwarmEval(ctx, in.CandidateSwarmID, in.LauncherID, evalAuthorize); err != nil {
				t.Fatalf("pinned roles could not start after %s catalogue drift: %v", drift, err)
			}
		})
	}
}
