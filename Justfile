import "Justfile.vars.just"

export GOEXPERIMENT := "jsonv2"

_default:
    @just --list

# Build the manager binary, generators and checks included
build: generate manifests fmt vet binary

# CGO is disabled here only, not globally: the image needs a static binary,
# while `just test` runs with -race, which requires cgo.
#
# Build the binary without running the generators
binary:
    @echo "GOOS=$(go env GOOS) GOARCH=$(go env GOARCH)"
    CGO_ENABLED=0 go build -o {{ bin_filename }}

# Run tests
test: manifests generate
    go test ./... -race -coverprofile cover.tmp.out
    grep -v -e "zz_generated.deepcopy.go" -e "/applyconfiguration/" cover.tmp.out > cover.out

# Bump direct dependencies to their latest version, then run the tests
bump: && test
    go get $(go list -m -f '{{{{if not (or .Indirect .Main)}}{{{{.Path}}@latest{{{{end}}' all)
    go mod tidy

# Generate ClusterRole, CustomResourceDefinition and webhook objects
manifests:
    {{ CONTROLLER_GEN }} rbac:roleName=manager-role crd:generateEmbeddedObjectMeta=true webhook paths="./..." output:crd:artifacts:config=config/crd/bases output:webhook:artifacts:config=config/webhook

# Generate deepcopy functions, apply configurations and manifests
generate: manifests
    go generate ./...
    {{ CONTROLLER_GEN }} object paths="./..."
    {{ CONTROLLER_GEN }} applyconfiguration paths="./api/..."

# Build the docs as they appear on docs.helmetica.io; fails on warnings
docs:
    GOPROXY=direct GOTOOLCHAIN=auto GOEXPERIMENT=jsonv2 go run github.com/helmetica-framework/rosarium@main preview .

# Build the docs and serve them on http://localhost:2020
docs-serve:
    GOPROXY=direct GOTOOLCHAIN=auto GOEXPERIMENT=jsonv2 go run github.com/helmetica-framework/rosarium@main preview . --serve :2020

# Run go fmt against code
fmt:
    go fmt ./...

# Run go vet against code
vet:
    go vet ./...

# All-in-one linting
lint: fmt vet generate manifests docs
    @echo 'Checking kustomize build ...'
    {{ KUSTOMIZE }} build config/crd -o /dev/null
    {{ KUSTOMIZE }} build config/default -o /dev/null
    @echo 'Check for uncommitted changes ...'
    git diff --exit-code

# Build the docker image
build-docker: binary
    docker build . --tag {{ GHCR_IMG }}

# Run the controller from your host.
run: manifests generate fmt vet
    go run main.go controller --enable-webhooks=false

# Clean up the generated resources
clean:
    rm -rf contrib/completion dist/ cover.out cover.tmp.out {{ bin_filename }} || true
