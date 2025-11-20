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
