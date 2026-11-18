package egress

// FEATURE: Точная managed egress policy сохраняет протокол, hostname и порт (#302).
// SPEC: HTTP и TLS не подменяют друг друга; offline допустим, wildcard/IP — нет;
// изменяемый вход не меняет скомпилированную policy, размер конфигурации ограничен.

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestExactProtocolHostPortPolicy(t *testing.T) {
	p, err := Compile([]Rule{
		{Protocol: HTTP, Hostnames: []string{"Tool.Example."}, Ports: []int{8080}},
		{Protocol: TLS, Hostnames: []string{"tool.example", "git.example"}, Ports: []int{443, 8443}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		protocol, host string
		port           int
		want           bool
	}{
		{HTTP, "tool.example", 8080, true}, {TLS, "TOOL.EXAMPLE.", 443, true},
		{TLS, "git.example", 8443, true}, {HTTP, "tool.example", 443, false},
		{TLS, "tool.example", 8080, false}, {HTTP, "git.example", 8080, false},
		{TLS, "sub.tool.example", 443, false}, {TLS, "tool.example", 80, false},
		{"tcp", "tool.example", 443, false}, {TLS, "127.0.0.1", 443, false},
		{TLS, "tool.example:443", 443, false}, {TLS, "tool.example", 0, false},
	} {
		if got := p.Allows(tc.protocol, tc.host, tc.port); got != tc.want {
			t.Errorf("%+v: got %v", tc, got)
		}
	}
}

func TestOfflinePolicyIsExplicitAndClosed(t *testing.T) {
	p, err := Compile(nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Allows(HTTP, "example.com", 80) || p.Allows(TLS, "example.com", 443) || p.AllowsDNS("example.com") {
		t.Fatal("offline policy permits traffic")
	}
	if got := p.Rules(); got == nil || len(got) != 0 {
		t.Fatalf("offline snapshot: %#v", got)
	}
}

func TestPolicyCanonicalizationAndOwnership(t *testing.T) {
	input := []Rule{{Protocol: TLS, Hostnames: []string{"Z.Example.", "a.example", "A.EXAMPLE"}, Ports: []int{8443, 443, 443}}}
	p, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	want := []Rule{{Protocol: TLS, Hostnames: []string{"a.example", "z.example"}, Ports: []int{443, 8443}}}
	if got := p.Rules(); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot: %#v", got)
	}
	if !p.AllowsDNS("Z.EXAMPLE.") || p.AllowsDNS("foreign.example") {
		t.Fatal("DNS approval differs from exact configured hostnames")
	}
	input[0].Hostnames[0] = "evil.example"
	input[0].Ports[0] = 1234
	snapshot := p.Rules()
	snapshot[0].Hostnames[0] = "evil.example"
	snapshot[0].Ports[0] = 1234
	if !reflect.DeepEqual(p.Rules(), want) || !p.Allows(TLS, "z.example", 8443) || p.Allows(TLS, "evil.example", 1234) {
		t.Fatal("caller rewrote compiled policy")
	}
}

func TestInvalidPolicyIsRefusedWithoutEchoingInput(t *testing.T) {
	for _, rule := range []Rule{
		{Protocol: "private-value", Hostnames: []string{"tool.example"}, Ports: []int{443}},
		{Protocol: TLS, Ports: []int{443}}, {Protocol: TLS, Hostnames: []string{"tool.example"}},
		{Protocol: TLS, Hostnames: []string{"*.private-value.example"}, Ports: []int{443}},
		{Protocol: TLS, Hostnames: []string{"127.0.0.1"}, Ports: []int{443}},
		{Protocol: TLS, Hostnames: []string{"[2001:db8::1]"}, Ports: []int{443}},
		{Protocol: TLS, Hostnames: []string{"https://private-value.example"}, Ports: []int{443}},
		{Protocol: TLS, Hostnames: []string{"private-value.example\n"}, Ports: []int{443}},
		{Protocol: TLS, Hostnames: []string{"localhost"}, Ports: []int{443}},
		{Protocol: TLS, Hostnames: []string{"a.123"}, Ports: []int{443}},
		{Protocol: TLS, Hostnames: []string{"a..example"}, Ports: []int{443}},
		{Protocol: TLS, Hostnames: []string{"-a.example"}, Ports: []int{443}},
		{Protocol: TLS, Hostnames: []string{"a-.example"}, Ports: []int{443}},
		{Protocol: TLS, Hostnames: []string{"K.example"}, Ports: []int{443}},
		{Protocol: TLS, Hostnames: []string{"tool.example"}, Ports: []int{0}},
		{Protocol: TLS, Hostnames: []string{"tool.example"}, Ports: []int{65536}},
	} {
		if p, err := Compile([]Rule{rule}); err == nil || p != nil || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("unsafe validation: policy=%v, error=%v", p, err)
		}
	}
}

func TestPolicyLimitsBoundCartesianExpansion(t *testing.T) {
	hosts := make([]string, 65)
	ports := make([]int, 65)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("h%d.example", i)
		ports[i] = i + 1
	}
	if p, err := Compile([]Rule{{Protocol: TLS, Hostnames: hosts, Ports: ports}}); err == nil || p != nil {
		t.Fatal("unbounded endpoint expansion accepted")
	}
	hosts = hosts[:64]
	ports = ports[:64]
	p, err := Compile([]Rule{{Protocol: TLS, Hostnames: hosts, Ports: ports}})
	if err != nil || !p.Allows(TLS, "h63.example", 64) {
		t.Fatalf("bounded policy refused: %v", err)
	}
	for _, input := range [][]Rule{
		make([]Rule, 65),
		{{Protocol: TLS, Hostnames: make([]string, 257), Ports: []int{443}}},
		{{Protocol: TLS, Hostnames: []string{"tool.example"}, Ports: make([]int, 257)}},
	} {
		if _, err := Compile(input); err == nil {
			t.Fatal("configuration collection limit ignored")
		}
	}
}
