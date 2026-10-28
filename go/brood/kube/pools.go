// WorkerPool установки: REST к Kubernetes с ограниченным набором полей.
// Отзеркаливает brood/server/projects/pools.py: WorkerPool порождает
// привилегированные поды, поэтому даже namespaced RBAC не позволяет принимать
// произвольные workerImage, ServiceAccount и pod-аннотации.
package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// collection — путь коллекции WorkerPool в namespace brood.
const collection = "/apis/ate.dev/v1/namespaces/" + Namespace + "/workerpools"

// nameRe — DNS-метка; имя пула — DNS-subdomain из таких меток.
var nameRe = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)

// digestRe — образ с sha256-digest, без :latest и mutable-тегов.
var digestRe = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

// allowedClasses — допустимые классы песочницы.
const classGvisor = "gvisor"

// ParseImages читает карту «класс песочницы — образ» из настроек: карта
// WORKER_POOL_IMAGES обязательна, WORKER_POOL_IMAGE — прежняя одиночная
// настройка gvisor-образа. Отказ 503 называет настройку.
func ParseImages(imagesRaw, singleRaw string) (map[string]string, error) {
	setting := "WORKER_POOL_IMAGES"
	if imagesRaw == "" {
		setting = "WORKER_POOL_IMAGE"
	}
	values := map[string]string{}
	if imagesRaw == "" {
		if singleRaw == "" {
			return nil, imageRefusal(setting)
		}
		if !digestImage(singleRaw) {
			return nil, imageRefusal(setting)
		}
		values[classGvisor] = singleRaw
		return values, nil
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(imagesRaw), &parsed); err != nil {
		return nil, imageRefusal(setting)
	}
	if !classSetValid(parsed) {
		return nil, imageRefusal(setting)
	}
	return parsed, nil
}

// imageRefusal — общий отказ настроек образов.
func imageRefusal(setting string) *KubeError {
	return &KubeError{Status: 503,
		Detail: setting + " must name deployed digest-pinned worker images by sandbox class"}
}

// classSetValid — карта обязательна, gvisor обязателен, классы только из
// списка, все образы — digest-pinned.
func classSetValid(parsed map[string]string) bool {
	if _, ok := parsed[classGvisor]; !ok {
		return false
	}
	for class, image := range parsed {
		if class != classGvisor && class != "microvm" && class != "lxc" {
			return false
		}
		if !digestImage(image) {
			return false
		}
	}
	return true
}

// digestImage — образ с sha256-digest.
func digestImage(value string) bool {
	return digestRe.MatchString(value)
}

// validName проверяет имя DNS-subdomain: до 253 символов, метки до 63.
func validName(name string) (string, error) {
	if len(name) > 253 {
		return "", nameRefusal()
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) > 63 || !nameRe.MatchString(label) {
			return "", nameRefusal()
		}
	}
	return name, nil
}

// nameRefusal — отказ по имени пула.
func nameRefusal() *KubeError {
	return &KubeError{Status: 400,
		Detail: "pool name must be a lowercase DNS subdomain up to 253 characters, with labels up to 63 characters"}
}

// poolsError переводит KubeError пакета в отказ сервиса; неожидаемые ошибки —
// 500 с причиной.
func poolsError(err error) error {
	if kubeErr, ok := err.(*KubeError); ok {
		return kubeErr
	}
	return &KubeError{Status: 500, Detail: err.Error()}
}

// Pools — сервис WorkerPool над REST-клиентом: настройка образов и правила
// полей выполняются до запроса.
type Pools struct {
	client *Client
	images map[string]string
}

// NewPools собирает сервис; отказы настроек — 503 с именем настройки.
func NewPools(endpoint, tokenFile, caFile, imagesRaw, singleRaw string) (*Pools, error) {
	rest, err := NewClient(endpoint, tokenFile, caFile)
	if err != nil {
		return nil, err
	}
	images, err := ParseImages(imagesRaw, singleRaw)
	if err != nil {
		return nil, err
	}
	return &Pools{client: rest, images: images}, nil
}

// Defaults — ответ страницы дефолтов: gvisor-образ и карта классов.
func (p *Pools) Defaults() map[string]any {
	return map[string]any{"workerImage": p.images[classGvisor], "workerImages": p.images}
}

// List обходит страницы WorkerPool.
func (p *Pools) List(ctx context.Context) ([]map[string]any, error) {
	var out []map[string]any
	for token := ""; ; {
		query := url.Values{"limit": []string{fmt.Sprint(pageSize)}}
		if token != "" {
			query.Set("continue", token)
		}
		response, err := p.client.request(ctx, "GET", collection, query.Encode(), nil)
		if err != nil {
			return nil, poolsError(err)
		}
		items, ok := response["items"].([]any)
		if !ok {
			return nil, &KubeError{Status: 502,
				Detail: "Kubernetes returned an invalid WorkerPool list"}
		}
		for _, item := range items {
			object, ok := item.(map[string]any)
			if !ok {
				return nil, &KubeError{Status: 502,
					Detail: "Kubernetes returned an invalid WorkerPool list"}
			}
			out = append(out, object)
		}
		metadata, _ := response["metadata"].(map[string]any)
		next, _ := metadata["continue"].(string)
		if next == "" {
			return out, nil
		}
		token = next
	}
}

// Get возвращает пул по имени.
func (p *Pools) Get(ctx context.Context, name string) (map[string]any, error) {
	item, err := validName(name)
	if err != nil {
		return nil, err
	}
	response, err := p.client.request(ctx, "GET", collection+"/"+item, "", nil)
	if err != nil {
		return nil, poolsError(err)
	}
	return response, nil
}

// Create проверяет тело и заводит пул; идентичность подменяется своей.
func (p *Pools) Create(ctx context.Context, body []byte) (map[string]any, error) {
	data, err := p.body(body, nil, nil)
	if err != nil {
		return nil, err
	}
	response, err := p.client.request(ctx, "POST", collection, "", data)
	if err != nil {
		return nil, poolsError(err)
	}
	return response, nil
} // Update проверяет предусловия из тела, сверяет их с текущим пулом (409 при
// расхождении), повторно валидирует спецификацию против существующей и
// заменяет пул. Имя — из пути, не из тела.
func (p *Pools) Update(ctx context.Context, name string, body []byte) (map[string]any, error) {
	item, err := validName(name)
	if err != nil {
		return nil, err
	}
	conditions, err := preconditions(body)
	if err != nil {
		return nil, err
	}
	current, err := p.Get(ctx, item)
	if err != nil {
		return nil, err
	}
	metadata, _ := current["metadata"].(map[string]any)
	for _, key := range []string{"uid", "resourceVersion"} {
		if fmt.Sprint(metadata[key]) != conditions[key] {
			return nil, &KubeError{Status: 409,
				Detail: "the worker pool has been modified; reload before retrying"}
		}
	}
	existing, _ := current["spec"].(map[string]any)
	data, err := p.body(body, &item, existing)
	if err != nil {
		return nil, err
	}
	var sent map[string]any
	if err := json.Unmarshal(data, &sent); err != nil {
		return nil, &KubeError{Status: 500, Detail: "worker pool body could not be assembled"}
	}
	sentMetadata, _ := sent["metadata"].(map[string]any)
	for _, key := range []string{"uid", "resourceVersion"} {
		sentMetadata[key] = conditions[key]
	}
	response, err := p.client.request(ctx, "PUT", collection+"/"+item, "", mustMarshal(sent))
	if err != nil {
		return nil, poolsError(err)
	}
	return response, nil
}

// Delete удаляет пул с предусловиями и Foreground-распространением.
func (p *Pools) Delete(ctx context.Context, name string, body []byte) error {
	item, err := validName(name)
	if err != nil {
		return err
	}
	conditions, err := preconditions(body)
	if err != nil {
		return err
	}
	payload := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions",
		"preconditions": conditions, "propagationPolicy": "Foreground"}
	_, err = p.client.request(ctx, "DELETE", collection+"/"+item, "", mustMarshal(payload))
	if err != nil {
		return poolsError(err)
	}
	return nil
}

// preconditions вынимает обязательные uid/resourceVersion из metadata тела.
func preconditions(body []byte) (map[string]string, error) {
	data, err := object(body)
	if err != nil {
		return nil, err
	}
	metadata, err := objectAt(data["metadata"], "metadata")
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for _, key := range []string{"uid", "resourceVersion"} {
		value, ok := metadata[key].(string)
		if !ok || strings.TrimSpace(value) == "" {
			return nil, &KubeError{Status: 400,
				Detail: "metadata." + key + " from the loaded pool is required; reload before retrying"}
		}
		result[key] = value
	}
	return result, nil
}

// object разбирает тело в объект-JSON.
func object(body []byte) (map[string]any, error) {
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil || data == nil {
		return nil, &KubeError{Status: 400, Detail: "the body must be a JSON object"}
	}
	return data, nil
}

// objectAt требует объект в поле.
func objectAt(value any, field string) (map[string]any, error) {
	data, ok := value.(map[string]any)
	if !ok {
		return nil, &KubeError{Status: 400, Detail: field + " must be a JSON object"}
	}
	return data, nil
}

// fields отказывается от полей вне списка, отсортированных по имени.
func fields(value map[string]any, allowed []string, field string) error {
	allowedSet := map[string]bool{}
	for _, name := range allowed {
		allowedSet[name] = true
	}
	var extra []string
	for name := range value {
		if !allowedSet[name] {
			extra = append(extra, name)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return &KubeError{Status: 400,
			Detail: field + ": unsupported fields: " + strings.Join(extra, ", ")}
	}
	return nil
}

// stringMap требует строковых ключей и значений.
func stringMap(value any, field string) error {
	data, err := objectAt(value, field)
	if err != nil {
		return err
	}
	for _, item := range data {
		if _, ok := item.(string); !ok {
			return &KubeError{Status: 400,
				Detail: field + " must contain string keys and values"}
		}
	}
	return nil
}

// validateSpec проверяет спецификацию: точный набор полей, класс песочницы из
// установленных, digest-pinned образ класса (прежний digest — только тем же
// пулом), ограниченный шаблон пода.
func (p *Pools) validateSpec(spec map[string]any, existing map[string]any) error {
	if err := fields(spec, []string{"replicas", "workerImage", "sandboxClasses", "template"}, "spec"); err != nil {
		return err
	}
	replicas, ok := spec["replicas"].(float64)
	if !ok || replicas != float64(int64(replicas)) || replicas < 0 || replicas > 2147483647 {
		return &KubeError{Status: 400,
			Detail: "spec.replicas must be an integer between 0 and 2147483647"}
	}
	classes, ok := spec["sandboxClasses"].([]any)
	if !ok || len(classes) != 1 {
		return &KubeError{Status: 400,
			Detail: "spec.sandboxClasses must contain exactly one installed sandbox class"}
	}
	class, err := objectAt(classes[0], "spec.sandboxClasses[0]")
	if err != nil {
		return err
	}
	if err := fields(class, []string{"name", "configRef"}, "spec.sandboxClasses[0]"); err != nil {
		return err
	}
	runtime, ok := class["name"].(string)
	if !ok || p.images[runtime] == "" {
		return &KubeError{Status: 400,
			Detail: "sandbox class must be one of: " + strings.Join(sortedClasses(p.images), ", ")}
	}
	// Смена класса существующего пула запрещена: класс выбирает образ и
	// конфигурацию воркера.
	if existing != nil {
		existingClasses, _ := existing["sandboxClasses"].([]any)
		if len(existingClasses) != 1 {
			return &KubeError{Status: 400,
				Detail: "the loaded worker pool has an invalid sandbox class"}
		}
		existingClass, _ := existingClasses[0].(map[string]any)
		existingName, _ := existingClass["name"].(string)
		if existingName != runtime {
			return &KubeError{Status: 400,
				Detail: "the sandbox class of an existing worker pool is immutable"}
		}
	}
	value, _ := spec["workerImage"].(string)
	unchanged := existing != nil && value == fmt.Sprint(existing["workerImage"]) && digestImage(value)
	if value != p.images[runtime] && !unchanged {
		return &KubeError{Status: 400,
			Detail: "spec.workerImage must match the deployed image for its sandbox class"}
	}
	if ref, ok := class["configRef"]; ok {
		configRef, err := objectAt(ref, "configRef")
		if err != nil {
			return err
		}
		if err := fields(configRef, []string{"name"}, "configRef"); err != nil {
			return err
		}
		name, ok := configRef["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			return &KubeError{Status: 400, Detail: "configRef.name is required"}
		}
	}
	template, err := objectAt(defaulted(spec["template"]), "spec.template")
	if err != nil {
		return err
	}
	if err := fields(template, []string{"resources", "labels", "nodeSelector"}, "spec.template"); err != nil {
		return err
	}
	for _, key := range []string{"labels", "nodeSelector"} {
		if _, ok := template[key]; ok {
			if err := stringMap(template[key], "spec.template."+key); err != nil {
				return err
			}
		}
	}
	resources, err := objectAt(defaulted(template["resources"]), "spec.template.resources")
	if err != nil {
		return err
	}
	if err := fields(resources, []string{"requests", "limits"}, "spec.template.resources"); err != nil {
		return err
	}
	for _, key := range []string{"requests", "limits"} {
		quantities, err := objectAt(defaulted(resources[key]), "resources."+key)
		if err != nil {
			return err
		}
		if err := fields(quantities, []string{"cpu", "memory"}, "resources."+key); err != nil {
			return err
		}
		for _, quantity := range quantities {
			text, ok := quantity.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return &KubeError{Status: 400,
					Detail: "resources." + key + " quantities must be nonempty strings"}
			}
		}
	}
	return nil
}

// defaulted заменяет nil пустым объектом: отсутствующее — пустое.
func defaulted(value any) any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

// sortedClasses — имена классов по алфавиту для причин отказов.
func sortedClasses(images map[string]string) []string {
	classes := make([]string, 0, len(images))
	for class := range images {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	return classes
}

// body разбирает тело, проверяет метаданные и спецификацию и собирает
// канонический объект WorkerPool: имя — из пути или тела, namespace — свой,
// серверные поля отбрасываются. existing — спецификация текущего пула при
// изменении.
func (p *Pools) body(body []byte, name *string, existing map[string]any) ([]byte, error) {
	data, err := object(body)
	if err != nil {
		return nil, err
	}
	metadata, err := objectAt(defaulted(data["metadata"]), "metadata")
	if err != nil {
		return nil, err
	}
	for _, unsafe := range []string{"ownerReferences", "finalizers"} {
		if _, ok := metadata[unsafe]; ok {
			return nil, &KubeError{Status: 400,
				Detail: "metadata." + unsafe + " cannot be changed through this API"}
		}
	}
	poolName := ""
	if name != nil {
		poolName = *name
	} else if value, ok := metadata["name"].(string); ok {
		poolName = value
	}
	poolName, err = validName(poolName)
	if err != nil {
		return nil, err
	}
	identity := map[string]any{"name": poolName, "namespace": Namespace}
	for _, key := range []string{"labels", "annotations"} {
		if value, ok := metadata[key]; ok {
			if err := stringMap(value, "metadata."+key); err != nil {
				return nil, err
			}
			identity[key] = value
		}
	}
	spec, err := objectAt(data["spec"], "spec")
	if err != nil {
		return nil, err
	}
	if err := p.validateSpec(spec, existing); err != nil {
		return nil, err
	}
	if name == nil {
		class := spec["sandboxClasses"].([]any)[0].(map[string]any)
		if _, present := class["configRef"]; class["name"] == "lxc" && !present {
			class["configRef"] = map[string]any{"name": "lxc-image"}
		}
	}
	return mustMarshal(map[string]any{"apiVersion": "ate.dev/v1", "kind": "WorkerPool",
		"metadata": identity, "spec": spec}), nil
}

// mustMarshal — JSON без отказа: значение собрано из проверенных данных.
func mustMarshal(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		return []byte("{}")
	}
	return data
}
