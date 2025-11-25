GIT_VERSION = $(shell git describe --tags --always)
FLAGS = -ldflags "\
  -X github.com/holacloud/flowtest_workers/config.VERSION=$(GIT_VERSION) \
"

run:
	go run $(FLAGS) .

build:
	go build $(FLAGS) -o bin/ .

test:
	go test -count=1 ./...

dep:
	go mod tidy
	go mod vendor

cloc:
	cloc --exclude-dir=vendor,data .

version:
	@echo "${GIT_VERSION}"

# dist

APP_NAME := ft_worker
DIST_DIR := dist
BIN_DIR := bin
GOOSARCH := \
	linux/amd64 \
	linux/arm64 \
	darwin/amd64 \
	darwin/arm64 \
	windows/amd64

.PHONY: all clean build dist

all: dist

clean:
	rm -rf $(DIST_DIR) $(BIN_DIR)


build-dist:
	@$(foreach target, $(GOOSARCH), \
		GOOS=$(word 1,$(subst /, ,$(target))) \
		GOARCH=$(word 2,$(subst /, ,$(target))) \
		; \
		BIN=$(APP_NAME)-$(GIT_VERSION)-$${GOOS}-$${GOARCH}; \
		if [ "$${GOOS}" = "windows" ]; then BIN="$(APP_NAME)-$(GIT_VERSION)-$${GOOS}-$${GOARCH}.exe"; fi; \
		echo "  → Building $${GOOS}/$${GOARCH}"; \
		GOOS=$${GOOS} GOARCH=$${GOARCH} go build $(FLAGS) -o $(BIN_DIR)/$${BIN}; \
	)

dist: clean build-dist
	mkdir -p $(DIST_DIR)

	@$(foreach target, $(GOOSARCH), \
		GOOS=$(word 1,$(subst /, ,$(target))) \
		GOARCH=$(word 2,$(subst /, ,$(target))) \
		; \
		if [ "$${GOOS}" = "windows" ]; then \
			zip -j $(DIST_DIR)/$(APP_NAME)-$(GIT_VERSION)-$${GOOS}-$${GOARCH}.zip $(BIN_DIR)/$(APP_NAME)-$(GIT_VERSION)-$${GOOS}-$${GOARCH}.exe; \
		else \
			zip -j $(DIST_DIR)/$(APP_NAME)-$(GIT_VERSION)-$${GOOS}-$${GOARCH}.zip $(BIN_DIR)/$(APP_NAME)-$(GIT_VERSION)-$${GOOS}-$${GOARCH}; \
		fi; \
	)

	@echo "▶ Packaging source code…"

	tar --exclude=$(DIST_DIR) \
	    --exclude=.git \
		--exclude=vendor \
		--exclude=.idea \
		--exclude=bin \
		--exclude=dist \
		--exclude=Makefile \
	    -czf $(DIST_DIR)/$(APP_NAME)-$(GIT_VERSION)-sources.tar.gz .

	@rm -rf $(BIN_DIR)
	@echo "▶ Done!"
	@ls -1 $(DIST_DIR)

	@echo "▶ Generating SHA256 checksums…"
	@cd $(DIST_DIR) && \
	for f in *.tar.gz; do \
		sha256sum "$$f" > "$$f.sha256"; \
	done && \
	for f in *.zip; do \
		sha256sum "$$f" > "$$f.sha256"; \
	done


