BINARY  := club
PKG     := ./cmd/club
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
GO      ?= go

CONTROL_URL ?=

LDFLAGS := -s -w -X github.com/nwylynko/clubhouse-cli/cli.Version=$(VERSION)
ifneq ($(strip $(CONTROL_URL)),)
LDFLAGS += -X github.com/nwylynko/clubhouse-cli/controlplane.DefaultURL=$(CONTROL_URL)
endif
LDFLAGS += -X github.com/nwylynko/clubhouse-cli/controlplane.ClientVersion=$(VERSION)

export CGO_ENABLED := 0

HOSTBIN := dist/$(BINARY)-$(shell $(GO) env GOOS)-$(shell $(GO) env GOARCH)

.PHONY: build release test fetch-git fetch-git-all wire-snapshot next-version next-version-consume box-image clean

build: fetch-git
	@mkdir -p dist
	$(GO) build -ldflags '$(LDFLAGS)' -o $(HOSTBIN) $(PKG)
	@echo "built $(HOSTBIN) — $(VERSION)"

fetch-git:
	@bash scripts/fetch-bundled-git.sh

fetch-git-all:
	@bash scripts/fetch-bundled-git.sh all

PLATFORMS := darwin/arm64 darwin/amd64 linux/arm64 linux/amd64
release: fetch-git-all
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		out=dist/$(BINARY)-$$os-$$arch; \
		echo "building $$out"; \
		GOOS=$$os GOARCH=$$arch $(GO) build -ldflags '$(LDFLAGS)' -o $$out $(PKG) || exit 1; \
	done

test:
	$(GO) build ./... && $(GO) vet ./... && $(GO) test ./...

wire-snapshot:
	@$(GO) run ./cmd/clubwire .

next-version:
	@$(GO) run ./cmd/clubnextversion .

next-version-consume:
	@$(GO) run ./cmd/clubnextversion -consume .

BOX_IMAGE ?= ghcr.io/nwylynko/clubhouse-box
BOX_PLATFORMS ?= linux/amd64,linux/arm64
BOX_HOST_PLATFORM = linux/$(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
BOX_LATEST_TAG = $(if $(filter-out %-dirty %-dev,$(VERSION)),-t $(BOX_IMAGE):latest,)

box-image:
	@test -n "$(BOX_LATEST_TAG)" || echo "note: $(VERSION) is not a released version — tagging :$(VERSION) only, leaving :latest alone"
	docker buildx build \
		-f box/Dockerfile.ci \
		--build-arg VERSION=$(VERSION) \
		--build-arg REVISION=$(shell git rev-parse HEAD 2>/dev/null || echo unknown) \
		-t $(BOX_IMAGE):$(VERSION) $(BOX_LATEST_TAG) \
		$(if $(filter 1,$(PUSH)),--platform $(BOX_PLATFORMS) --provenance=true --push,--platform $(BOX_HOST_PLATFORM) --load) \
		.

clean:
	rm -rf dist
