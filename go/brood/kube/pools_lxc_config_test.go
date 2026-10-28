package kube

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// BUG: Новый LXC pool полагался на отсутствующий либо чужой class-default (#77).
// HYPOTHESIS: Установочный lxc-image должен быть явным configRef только при create.
func TestLXCCreateUsesExplicitInstallationConfig(t *testing.T) {
	lxcImage := "registry/ateworker-lxc@sha256:" + strings.Repeat("d", 64)
	for _, config := range []string{"", "custom-trusted-lxc"} {
		t.Run(config, func(t *testing.T) {
			api := startAPI(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(w, r.Body)
			})
			service, err := NewPools(api.server.URL, api.tokenFile, api.caFile,
				mustJSON(map[string]string{"gvisor": image, "lxc": lxcImage}), "")
			if err != nil {
				t.Fatal(err)
			}
			in := pool()
			spec := in["spec"].(map[string]any)
			class := map[string]any{"name": "lxc"}
			if config != "" {
				class["configRef"] = map[string]any{"name": config}
			}
			spec["workerImage"], spec["sandboxClasses"] = lxcImage, []any{class}
			saved, err := service.Create(context.Background(), body(t, in))
			if err != nil {
				t.Fatal(err)
			}
			wanted := config
			if wanted == "" {
				wanted = "lxc-image"
			}
			actual := saved["spec"].(map[string]any)["sandboxClasses"].([]any)[0].(map[string]any)
			if mustJSON(actual["configRef"]) != mustJSON(map[string]any{"name": wanted}) {
				t.Fatalf("configRef = %v, want %s", actual["configRef"], wanted)
			}
		})
	}
}

func TestLXCUpdateDoesNotRebindAnExistingDefaultPool(t *testing.T) {
	lxcImage := "registry/ateworker-lxc@sha256:" + strings.Repeat("d", 64)
	loaded := pool()
	spec := loaded["spec"].(map[string]any)
	spec["workerImage"], spec["sandboxClasses"] = lxcImage, []any{map[string]any{"name": "lxc"}}
	api := startAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write(body(t, loaded))
			return
		}
		_, _ = io.Copy(w, r.Body)
	})
	service, err := NewPools(api.server.URL, api.tokenFile, api.caFile,
		mustJSON(map[string]string{"gvisor": image, "lxc": lxcImage}), "")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := service.Update(context.Background(), "agents", body(t, loaded))
	if err != nil {
		t.Fatal(err)
	}
	class := saved["spec"].(map[string]any)["sandboxClasses"].([]any)[0].(map[string]any)
	if _, present := class["configRef"]; present {
		t.Fatal("ordinary update silently rebound an existing LXC pool")
	}
}
