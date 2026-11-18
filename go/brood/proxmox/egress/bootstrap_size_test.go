package egress

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func bootstrapSizeFixture(t *testing.T, size int) ([]Rule, []byte) {
	t.Helper()
	hosts := make([]string, 40)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("h%03d.%s.%s.%s.example.com", i, strings.Repeat("a", 60), strings.Repeat("b", 60), strings.Repeat("c", 60))
	}
	rules := []Rule{{Protocol: TLS, Hostnames: hosts, Ports: []int{443}}}
	marshal := func() []byte {
		raw, err := json.Marshal(struct {
			Version int    `json:"version"`
			Rules   []Rule `json:"rules"`
		}{1, rules})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	padding := size - len(marshal())
	if padding < 0 || padding > 59 {
		t.Fatalf("invalid size fixture padding: %d", padding)
	}
	hosts[0] = strings.Repeat("d", padding) + hosts[0]
	raw := marshal()
	if len(raw) != size {
		t.Fatalf("fixture size=%d, want %d", len(raw), size)
	}
	return rules, raw
}

func TestPolicyFitsBootstrapEnvironmentValue(t *testing.T) {
	for _, size := range []int{8192, 8193} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			rules, raw := bootstrapSizeFixture(t, size)
			policy, err := Compile(rules)
			if err != nil {
				t.Fatal(err)
			}
			encoded, encodeErr := policy.Encode()
			decoded, decodeErr := Decode(raw)
			if size == 8192 {
				if encodeErr != nil || len(encoded) != size || decodeErr != nil || decoded == nil {
					t.Fatalf("transport-sized policy refused: encode=%v bytes=%d decode=%v", encodeErr, len(encoded), decodeErr)
				}
			} else {
				if encodeErr == nil || encoded != nil {
					t.Errorf("encoded %d-byte policy exceeds the 8192-byte bootstrap value limit", len(encoded))
				}
				if decodeErr == nil || decoded != nil {
					t.Error("accepted policy exceeding the 8192-byte bootstrap value limit")
				}
			}
		})
	}
}
