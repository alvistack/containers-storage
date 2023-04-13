export GO111MODULE=off
export GOPROXY=https://proxy.golang.org

.PHONY: \
	all \
	clean \
	default \
	docs \
	help \
	install.tools \
	local-binary \
	local-cross \
	local-gccgo \
	local-test \
	local-test-integration \
	local-test-unit \
	local-validate \
	lint \
	vendor

PACKAGE := github.com/containers/storage
GIT_BRANCH := $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null)
GIT_BRANCH_CLEAN := $(shell echo $(GIT_BRANCH) | sed -e "s/[^[:alnum:]]/-/g")
EPOCH_TEST_COMMIT := 0418ebf59f9e1f564831c0ba9378b7f8e40a1c73
NATIVETAGS :=
AUTOTAGS := $(shell ./hack/btrfs_tag.sh) $(shell ./hack/libdm_tag.sh) $(shell ./hack/libsubid_tag.sh)
BUILDFLAGS := -tags "$(AUTOTAGS) $(TAGS)" $(FLAGS)
GO ?= go
TESTFLAGS := $(shell $(GO) test -race $(BUILDFLAGS) ./pkg/stringutils 2>&1 > /dev/null && echo -race)

# Go module support: set `-mod=vendor` to use the vendored sources
ifeq ($(shell $(GO) help mod >/dev/null 2>&1 && echo true), true)
	GO:=GO111MODULE=on $(GO)
	MOD_VENDOR=-mod=vendor
endif

default all: local-binary docs local-validate local-cross ## validate all checks, build and cross-build\nbinaries and docs

clean: ## remove all built files
	$(RM) -f containers-storage containers-storage.* docs/*.1 docs/*.5

sources := $(wildcard *.go cmd/containers-storage/*.go drivers/*.go drivers/*/*.go internal/*/*.go pkg/*/*.go pkg/*/*/*.go types/*.go)
containers-storage: $(sources) ## build using gc on the host
	$(GO) build $(MOD_VENDOR) -compiler gc $(BUILDFLAGS) ./cmd/containers-storage

codespell:
	codespell -S Makefile,build,buildah,buildah.spec,imgtype,copy,AUTHORS,bin,vendor,.git,go.sum,CHANGELOG.md,changelog.txt,seccomp.json,.cirrus.yml,"*.xz,*.gz,*.tar,*.tgz,*ico,*.png,*.1,*.5,*.orig,*.rej" -L worl,flate,uint,iff,od,ERRO -w

binary local-binary: containers-storage

local-gccgo gccgo: ## build using gccgo on the host
	GCCGO=$(PWD)/hack/gccgo-wrapper.sh $(GO) build $(MOD_VENDOR) -compiler gccgo $(BUILDFLAGS) -o containers-storage.gccgo ./cmd/containers-storage

local-cross cross: ## cross build the binaries for arm, darwin, and freebsd
	@for target in linux/amd64 linux/386 linux/arm linux/arm64 linux/ppc64 linux/ppc64le linux/s390x linux/mips linux/mipsle linux/mips64 linux/mips64le darwin/amd64 windows/amd64 freebsd/amd64 freebsd/arm64 ; do \
		os=`echo $${target} | cut -f1 -d/` ; \
		arch=`echo $${target} | cut -f2 -d/` ; \
		suffix=$${os}.$${arch} ; \
		echo env CGO_ENABLED=0 GOOS=$${os} GOARCH=$${arch} $(GO) build $(MOD_VENDOR) -compiler gc -tags \"$(NATIVETAGS) $(TAGS)\" $(FLAGS) -o containers-storage.$${suffix} ./cmd/containers-storage ; \
		env CGO_ENABLED=0 GOOS=$${os} GOARCH=$${arch} $(GO) build $(MOD_VENDOR) -compiler gc -tags "$(NATIVETAGS) $(TAGS)" $(FLAGS) -o containers-storage.$${suffix} ./cmd/containers-storage || exit 1 ; \
	done

docs: install.tools ## build the docs on the host
	$(MAKE) -C docs docs

local-test: local-binary local-test-unit local-test-integration ## build the binaries and run the tests

local-test-unit test-unit: local-binary ## run the unit tests on the host (requires\nsuperuser privileges)
	@$(GO) test -count 1 $(MOD_VENDOR) $(BUILDFLAGS) $(TESTFLAGS) $(shell $(GO) list ./... | grep -v ^$(PACKAGE)/vendor)

local-test-integration test-integration: local-binary ## run the integration tests on the host (requires\nsuperuser privileges)
	@cd tests; ./test_runner.bash

local-validate validate: install.tools ## validate DCO and gofmt on the host
	@./hack/git-validation.sh
	@./hack/gofmt.sh

install.tools:
	$(MAKE) -C tests/tools

$(FFJSON):
	$(MAKE) -C tests/tools

install.docs: docs
	$(MAKE) -C docs install

install: install.docs

lint: install.tools
	tests/tools/build/golangci-lint run --build-tags="$(AUTOTAGS) $(TAGS)"

help: ## this help
	@awk 'BEGIN {FS = ":.*?## "} /^[a-z A-Z_-]+:.*?## / {gsub(" ",",",$$1);gsub("\\\\n",sprintf("\n%22c"," "), $$2);printf "\033[36m%-21s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

vendor-in-container:
	podman run --privileged --rm --env HOME=/root -v `pwd`:/src -w /src golang make vendor

vendor:
	$(GO) mod tidy
	$(GO) mod vendor
	$(GO) mod verify
