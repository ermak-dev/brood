package actormodel

// FEATURE: Claude Code models of managed actors set by the installation (#263).
// SPEC: A model name is 1..MaxName bytes of printable ASCII without spaces
// (0x21..0x7e); the zero Models means "Claude Code chooses", and each field is
// optional on its own. Validate names the violated setting and never echoes
// the value.

import (
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	for _, good := range []string{"glm-5.3", "glm-5.3-flash", "claude-opus-4-6[1m]", "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/abc", "openrouter/z-ai/glm-5.3", "!~", strings.Repeat("m", MaxName)} {
		if !ValidName(good) {
			t.Errorf("%q rejected", good)
		}
	}
	for name, bad := range map[string]string{
		"empty":          "",
		"space":          "glm 5.3",
		"leading space":  " glm-5.3",
		"tab":            "glm-5.3\t",
		"newline":        "glm-5.3\nANTHROPIC_API_KEY=x",
		"nul":            "glm\x00",
		"delete":         "glm\x7f",
		"non-ASCII":      "glm-5.3-флеш",
		"too long":       strings.Repeat("m", MaxName+1),
		"high byte only": "\xff",
	} {
		if ValidName(bad) {
			t.Errorf("%s: %q accepted", name, bad)
		}
	}
}

func TestModelsValidate(t *testing.T) {
	for _, good := range []Models{{}, {Model: "glm-5.3"}, {Small: "glm-5.3-flash"}, {Model: "glm-5.3", Small: "glm-5.3-flash"}} {
		if err := good.Validate(); err != nil {
			t.Errorf("%+v: %v", good, err)
		}
	}
	if !(Models{}).IsZero() || (Models{Small: "glm-5.3-flash"}).IsZero() {
		t.Fatal("zero value classification")
	}
	for setting, bad := range map[string]Models{
		"BROOD_ACTOR_MODEL":       {Model: "private model", Small: "glm-5.3-flash"},
		"BROOD_ACTOR_SMALL_MODEL": {Model: "glm-5.3", Small: "private\nmodel"},
	} {
		err := bad.Validate()
		if err == nil || !strings.Contains(err.Error(), setting) || strings.Contains(err.Error(), "private") {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}
