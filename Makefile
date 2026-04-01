
# Build clipboard-over-ssh for multiple architectures.
# All builds are fully static (CGO_ENABLED=0).

BINARY = clipboard-over-ssh
# Auto-detect GOROOT from the go binary if not set
GOROOT ?= $(shell go env GOROOT 2>/dev/null || echo /usr/lib/go-1.22)
GO = GOROOT=$(GOROOT) go

PLATFORMS = linux/amd64 linux/arm64 linux/arm
OUTDIR = dist

.PHONY: all clean $(PLATFORMS)

all: $(PLATFORMS)

$(PLATFORMS):
	$(eval GOOS := $(word 1,$(subst /, ,$@)))
	$(eval GOARCH := $(word 2,$(subst /, ,$@)))
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build \
		-ldflags="-s -w" \
		-o $(OUTDIR)/$(BINARY)-$(GOOS)-$(GOARCH) .

# Build for the current platform only
build:
	CGO_ENABLED=0 $(GO) build -ldflags="-s -w" -o $(BINARY) .

clean:
	rm -rf $(OUTDIR) $(BINARY)
