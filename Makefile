UPSTREAM := https://raw.githubusercontent.com/gnachman/iTerm2
COMMIT   ?= fe8cbff71a07035ac317c0f6535c48aab77d5e76
PB_PKG   := github.com/schleppy/iterm2/internal/iterm2pb

.PHONY: all build test vet proto proto-fetch topology

all: vet test build

build:
	go build ./...

test:
	go test -race ./...

vet:
	go vet ./...

# Regenerate Go bindings from the vendored upstream schema. protoc-gen-go is
# pinned through the `tool` directive in go.mod.
proto:
	protoc \
		--plugin=protoc-gen-go="$$(go tool -n protoc-gen-go)" \
		-I proto \
		--go_out=internal/iterm2pb --go_opt=paths=source_relative \
		--go_opt=Miterm2/api.proto=$(PB_PKG) \
		iterm2/api.proto
	mv internal/iterm2pb/iterm2/api.pb.go internal/iterm2pb/api.pb.go
	rmdir internal/iterm2pb/iterm2

proto-fetch:
	curl -sSfL -o proto/iterm2/api.proto $(UPSTREAM)/$(COMMIT)/proto/api.proto
	shasum -a 256 proto/iterm2/api.proto

# Print the live topology of a running iTerm2.
topology:
	go run ./cmd/iterm2-topology
