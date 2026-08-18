
GO_BUILD_ENV :=
GO_BUILD_FLAGS := -trimpath
MODULE_BINARY := bin/windows-diagnostics
VIAM_TARGET_OS := windows
HOST_OS := $(shell go env GOHOSTOS)

ifeq ($(VIAM_TARGET_OS), windows)
	GO_BUILD_ENV += GOOS=windows GOARCH=amd64
	GO_BUILD_FLAGS += -tags no_cgo
	MODULE_BINARY = bin/windows-diagnostics.exe
endif

$(MODULE_BINARY): Makefile go.mod components/*.go cmd/module/*.go
	GOOS=$(VIAM_BUILD_OS) GOARCH=$(VIAM_BUILD_ARCH) $(GO_BUILD_ENV) go build $(GO_BUILD_FLAGS) -o $(MODULE_BINARY) cmd/module/main.go

lint:
	gofmt -s -w .

update:
	go get go.viam.com/rdk@latest
	go mod tidy

# Every file in components/ is //go:build windows, so on any other host the package
# is empty and `go test ./...` fails to even load it. Cross-compiling a test binary
# would not help: it cannot be executed here. Type-check against the target instead,
# which is all `go test` provided anyway while the repo has no test files.
test:
ifeq ($(HOST_OS), windows)
	$(GO_BUILD_ENV) go test $(GO_BUILD_FLAGS) ./...
else
	@echo "host is $(HOST_OS): type-checking for windows/amd64 (tests only run on a windows host)"
	$(GO_BUILD_ENV) go build $(GO_BUILD_FLAGS) ./...
endif

module.tar.gz: meta.json $(MODULE_BINARY)
ifneq ($(VIAM_TARGET_OS), windows)
	strip $(MODULE_BINARY)
endif
	tar czf $@ meta.json $(MODULE_BINARY)

module: test module.tar.gz

all: test module.tar.gz

setup:
	GOOS=windows GOARCH=amd64 go mod tidy
