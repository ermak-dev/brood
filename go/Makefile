# Сборка и проверки Go-части brood (docs/GO.md): бинарники в go/bin,
# кросс-компиляция под целевые платформы релиза. Toolchain закреплён в
# go.mod; команды -- те же, что гоняет CI (.ci/go-test.sh).
BINARY ?= brood
GO     ?= go
export CGO_ENABLED := 0

# Версия бинарников: git describe рабочей копии; в CI переменную задаёт
# пайплайн (конвейер не зависит от git в образе).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X=main.version=$(VERSION)

CMDS    := ./cmd/brood ./cmd/bro
TARGETS := linux/amd64 linux/arm64

.PHONY: all build release web web-test test vet fmt-check cross clean

all: build

# Release rebuilds source; ordinary build/test deliberately need no Bun.
web:
	bash ../.ci/web.sh build

web-test:
	bash ../.ci/web.sh verify

release: web
	$(MAKE) build cross

# Бинарники сервера и клиента -- в go/bin/, он в .gitignore.
build:
	$(GO) build -trimpath -ldflags="$(LDFLAGS)" -o bin/ $(CMDS)

test:
	$(GO) test ./... -count=1

vet:
	$(GO) vet ./...

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then \
		echo "gofmt needed:"; echo "$$out"; exit 1; fi

# Кросс-компиляция релиза: go/bin/<os>_<arch>/. CGO выключен: драйвер
# SQLite -- modernc.org/sqlite, внешнего runtime у бинарников нет.
cross:
	@for target in $(TARGETS); do \
		os="$${target%/*}"; arch="$${target#*/}"; \
		echo "== $$target"; \
		mkdir -p "bin/$${os}_$${arch}"; \
		for cmd in brood bro; do \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags="$(LDFLAGS)" \
				-o "bin/$${os}_$${arch}/$$cmd" "./cmd/$$cmd" || exit 1; \
		done; \
	done

clean:
	rm -rf bin
