package kube

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// FEATURE: Явно установленный experimental LXC доступен в API пулов (#72).
// SPEC: Только закреплённый образ из настроек, gvisor остаётся default;
// отсутствие класса, подмена образа и смена runtime существующего пула запрещены.
func TestInstalledLXCImageAndPool(t *testing.T) {
	lxcImage := "registry/ateworker-lxc@sha256:" + strings.Repeat("d", 64)
	approved := map[string]string{"gvisor": image, "microvm": microvmImg, "lxc": lxcImage}
	parsed, err := ParseImages(mustJSON(approved), "")
	if err != nil || parsed["lxc"] != lxcImage {
		t.Fatalf("installed LXC images = %v, %v", parsed, err)
	}
	api := startAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(w, r.Body)
	})
	service, err := NewPools(api.server.URL, api.tokenFile, api.caFile, mustJSON(approved), "")
	if err != nil {
		t.Fatal(err)
	}
	if service.Defaults()["workerImage"] != image {
		t.Fatal("LXC changed the default gvisor image")
	}
	in := pool()
	spec := in["spec"].(map[string]any)
	spec["workerImage"] = lxcImage
	spec["sandboxClasses"] = []any{map[string]any{"name": "lxc", "configRef": map[string]any{"name": "lxc-trusted"}}}
	saved, err := service.Create(context.Background(), body(t, in))
	if err != nil {
		t.Fatal(err)
	}
	if mustJSON(saved["spec"]) != mustJSON(spec) {
		t.Fatalf("LXC spec changed: %v", saved["spec"])
	}
}

func TestLXCSettingsRemainFailClosed(t *testing.T) {
	for _, raw := range []string{
		`{"lxc":"registry/worker@sha256:` + strings.Repeat("d", 64) + `"}`,
		`{"gvisor":"` + image + `","lxc":"worker:latest"}`,
		`{"gvisor":"` + image + `","fullvm":"` + microvmImg + `"}`,
	} {
		if _, err := ParseImages(raw, ""); asKube(t, err).Status != 503 {
			t.Fatalf("unsafe LXC installation accepted: %s", raw)
		}
	}
}

func TestLXCPoolCannotChangeRuntimeOrUseAnotherRuntimeImage(t *testing.T) {
	lxcImage := "registry/ateworker-lxc@sha256:" + strings.Repeat("d", 64)
	for _, change := range []string{"image", "runtime"} {
		t.Run(change, func(t *testing.T) {
			loaded := pool()
			loadedSpec := loaded["spec"].(map[string]any)
			loadedSpec["workerImage"] = lxcImage
			loadedSpec["sandboxClasses"] = []any{map[string]any{"name": "lxc"}}
			api := startAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(body(t, loaded))
			})
			service, err := NewPools(api.server.URL, api.tokenFile, api.caFile,
				mustJSON(map[string]string{"gvisor": image, "lxc": lxcImage}), "")
			if err != nil {
				t.Fatal(err)
			}
			in := pool()
			spec := in["spec"].(map[string]any)
			spec["sandboxClasses"] = []any{map[string]any{"name": "lxc"}}
			if change == "runtime" {
				spec["sandboxClasses"] = []any{map[string]any{"name": "gvisor"}}
			}
			_, err = service.Update(context.Background(), "agents", body(t, in))
			if asKube(t, err).Status != 400 {
				t.Fatal("LXC runtime/image change was accepted")
			}
			if len(api.sent) != 0 {
				t.Fatal("invalid change reached Kubernetes")
			}
		})
	}
}
