package egress

// FEATURE: Версионированный bootstrap не допускает неоднозначных policy (#302).
// SPEC: Ключи точные, дубликаты/неизвестные поля и oversized JSON отвергаются;
// явный offline отличается от отсутствия legacy-настройки.

import (
	"bytes"
	"strings"
	"testing"
)

func TestPolicyCodecRoundTrip(t *testing.T) {
	p, err := Compile([]Rule{{Protocol: TLS, Hostnames: []string{"Tool.Example."}, Ports: []int{8443, 443}}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"rules":[{"protocol":"tls","hostnames":["tool.example"],"ports":[443,8443]}]}`
	if string(raw) != want {
		t.Fatalf("wire format: %s", raw)
	}
	restored, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		raw[i] = 'x'
	}
	if !restored.Allows(TLS, "tool.example", 443) || restored.Allows(HTTP, "tool.example", 443) {
		t.Fatal("wire policy changed protocol or retained mutable input")
	}
	encoded, err := restored.Encode()
	if err != nil || string(encoded) != want {
		t.Fatalf("round trip: %q %v", encoded, err)
	}
	offline, err := Decode([]byte(`{"version":1,"rules":[]}`))
	if err != nil || offline.AllowsDNS("tool.example") {
		t.Fatalf("offline decode: %v", err)
	}
	encoded, err = offline.Encode()
	if err != nil || string(encoded) != `{"version":1,"rules":[]}` {
		t.Fatalf("offline encode: %q %v", encoded, err)
	}
}

func TestPolicyCodecRejectsAmbiguityWithoutEcho(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `{}`, `{"version":1}`, `{"rules":[]}`, `{"version":2,"rules":[]}`,
		`{"version":1.0,"rules":[]}`, `{"version":null,"rules":[]}`, `{"version":1,"rules":null}`,
		`{"version":1,"Version":2,"rules":[]}`, `{"version":1,"version":2,"rules":[]}`,
		`{"version":1,"rules":[],"rules":[]}`, `{"version":1,"Rules":[]}`,
		`{"version":1,"rules":[],"private-value":true}`,
		`{"version":1,"rules":[null]}`,
		`{"version":1,"rules":[{"protocol":"http","protocol":"tls","hostnames":["tool.example"],"ports":[443]}]}`,
		`{"version":1,"rules":[{"Protocol":"tls","hostnames":["tool.example"],"ports":[443]}]}`,
		`{"version":1,"rules":[{"protocol":"tls","hostnames":["tool.example"],"ports":[443],"private-value":true}]}`,
		`{"version":1,"rules":[{"protocol":"tls","hostnames":null,"ports":[443]}]}`,
		`{"version":1,"rules":[{"protocol":"tls","hostnames":["tool.example"],"ports":[443.0]}]}`,
		`{"version":1,"rules":[]} {"version":1,"rules":[]}`,
	} {
		if p, err := Decode([]byte(raw)); err == nil || p != nil || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("unsafe decode of %q: policy=%v error=%v", raw, p, err)
		}
	}
	if _, err := Decode(bytes.Repeat([]byte{' '}, (128<<10)+1)); err == nil {
		t.Fatal("oversized bootstrap accepted")
	}
	raw := append([]byte(`{"version":1,"rules":[],"`), 0xff)
	raw = append(raw, []byte(`":true}`)...)
	if _, err := Decode(raw); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}
