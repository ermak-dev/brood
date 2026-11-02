package main

import (
	"reflect"
	"testing"
)

// FEATURE: Explicit guest/operator command routing.
// SPEC: Globals work before/after commands, stop at --, never infer guest mode.
// IMPLEMENTATION: Parse globals independently of catalog positional arguments.
// RESULT: IMPLEMENTED — mode, scope, flag order and invalid combinations pass.
// STATUS: DONE
func TestParseExecution(t *testing.T) {
	for _, tc := range []struct {
		args        []string
		mode, swarm string
		guest       bool
		rest        []string
		bad         bool
	}{
		{args: []string{"--guest", "mcp"}, guest: true, rest: []string{"mcp"}},
		{args: []string{"mcp", "--guest"}, guest: true, rest: []string{"mcp"}},
		{args: []string{"agents", "--swarm", "s1"}, swarm: "s1", rest: []string{"agents"}},
		{args: []string{"--swarm=s2", "agents"}, swarm: "s2", rest: []string{"agents"}},
		{args: []string{"agents"}, mode: "guest", guest: true, rest: []string{"agents"}},
		{args: []string{"agents"}, rest: []string{"agents"}},
		{args: []string{"send", "peer", "--", "--guest"}, rest: []string{"send", "peer", "--", "--guest"}},
		{args: []string{"--swarm"}, bad: true},
		{args: []string{"--guest", "--swarm", "s1", "agents"}, bad: true},
		{args: []string{"agents"}, mode: "auto", bad: true},
	} {
		got, err := parseExecution(tc.args, tc.mode, "")
		if (err != nil) != tc.bad {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if tc.bad {
			continue
		}
		if got.guest != tc.guest || got.swarm != tc.swarm || !reflect.DeepEqual(got.args, tc.rest) {
			t.Fatalf("%v: %+v", tc.args, got)
		}
	}
}
