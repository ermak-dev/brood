package gitidentity

// FEATURE: Git identity of a managed actor (#249).
// SPEC: The installation domain and the actor name derive the author and
// committer identity "brood <actor>" <actor@domain>. The domain is a lowercase
// DNS name; name and email are bounded, free of control characters and of the
// angle brackets Git uses to delimit the email. Errors never echo the value.

import (
	"strings"
	"testing"
)

func TestActorIdentity(t *testing.T) {
	id, err := Actor("worker2", "brood.corp.example")
	if err != nil || id != (Identity{Name: "brood worker2", Email: "worker2@brood.corp.example"}) {
		t.Fatalf("identity: %+v %v", id, err)
	}
	if id.IsZero() || (Identity{}).IsZero() != true {
		t.Fatal("zero value classification")
	}
	for name, tc := range map[string][2]string{
		"empty actor":      {"", "brood.corp.example"},
		"empty domain":     {"worker2", ""},
		"uppercase domain": {"worker2", "Brood.corp.example"},
		"IP domain":        {"worker2", "192.0.2.1"},
		"numeric label":    {"worker2", "example.123"},
		"trailing dot":     {"worker2", "brood.corp.example."},
		"hyphen edge":      {"worker2", "-brood.example"},
		"space in domain":  {"worker2", "brood example"},
		"actor with space": {"worker 2", "brood.corp.example"},
		"actor with at":    {"worker@2", "brood.corp.example"},
		"actor with angle": {"worker<2>", "brood.corp.example"},
		"control in actor": {"worker\n2", "brood.corp.example"},
		"long domain":      {"worker2", strings.Repeat("a", 64) + ".example"},
		"long actor":       {strings.Repeat("a", 250), "brood.corp.example"},
	} {
		t.Run(name, func(t *testing.T) {
			id, err := Actor(tc[0], tc[1])
			if err == nil || id != (Identity{}) || strings.Contains(err.Error(), tc[0]) && tc[0] != "" || strings.Contains(err.Error(), tc[1]) && tc[1] != "" {
				t.Fatalf("accepted or echoed: %+v %v", id, err)
			}
		})
	}
}

func TestValidDomain(t *testing.T) {
	for _, ok := range []string{"localhost", "brood.corp.example", "a-b.c1.example"} {
		if !ValidDomain(ok) {
			t.Errorf("rejected %q", ok)
		}
	}
	for _, bad := range []string{"", "Example.com", "1.2.3.4", "[2001:db8::1]", "a..b", "a_b.example", "example.com.", strings.Repeat("a.", 127) + "example"} {
		if ValidDomain(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestValidate(t *testing.T) {
	valid := Identity{Name: "brood worker2", Email: "worker2@brood.corp.example"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]Identity{
		"empty":               {},
		"name only":           {Name: "brood worker2"},
		"email only":          {Email: "worker2@brood.corp.example"},
		"name angle":          {Name: "brood <worker2>", Email: valid.Email},
		"name control":        {Name: "brood\tworker2", Email: valid.Email},
		"name newline":        {Name: "brood\nworker2", Email: valid.Email},
		"name padded":         {Name: " brood worker2", Email: valid.Email},
		"name long":           {Name: strings.Repeat("n", MaxName+1), Email: valid.Email},
		"name invalid UTF-8":  {Name: "brood \xff", Email: valid.Email},
		"email angle":         {Name: valid.Name, Email: "<worker2@brood.corp.example>"},
		"email space":         {Name: valid.Name, Email: "worker 2@brood.corp.example"},
		"email two at":        {Name: valid.Name, Email: "worker@2@brood.corp.example"},
		"email no at":         {Name: valid.Name, Email: "worker2.brood.corp.example"},
		"email empty local":   {Name: valid.Name, Email: "@brood.corp.example"},
		"email bad domain":    {Name: valid.Name, Email: "worker2@Brood.corp.example"},
		"email control":       {Name: valid.Name, Email: "worker2@brood.corp.example\n"},
		"email long":          {Name: valid.Name, Email: strings.Repeat("l", MaxEmail) + "@brood.corp.example"},
		"email local chars":   {Name: valid.Name, Email: "work(er)2@brood.corp.example"},
		"email non-ASCII":     {Name: valid.Name, Email: "wörker@brood.corp.example"},
		"name only non-ASCII": {Name: "брод worker2", Email: valid.Email},
	} {
		t.Run(name, func(t *testing.T) {
			err := id.Validate()
			if name == "name only non-ASCII" {
				if err != nil {
					t.Fatalf("UTF-8 name rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted")
			}
			if strings.Contains(err.Error(), "worker") || strings.Contains(err.Error(), "brood.corp") {
				t.Fatalf("echoed value: %v", err)
			}
		})
	}
}
