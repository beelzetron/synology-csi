# Repository Guidelines

## Project Structure & Module Organization

- `main.go` starts the CSI driver and wires DSM config, host execution, and gRPC servers.
- `pkg/driver/` contains CSI controller, node, identity, iSCSI, NFS/SMB, multipath, and mount logic.
- `pkg/dsm/service/` translates CSI operations into DSM volume, share, snapshot, and clone workflows.
- `pkg/dsm/webapi/` contains the DSM HTTP API client.
- `pkg/models/`, `pkg/utils/`, and `pkg/interfaces/` hold shared types and contracts.
- `synocli/` provides a DSM debugging/admin CLI.
- `deploy/helm/`, `deploy/kubernetes/`, and `config/` contain manifests and examples.
- Tests live beside code as `*_test.go`; sanity tests live under `test/sanity/`.

## Build, Test, and Development Commands

- `make build`: builds `bin/synology-csi-driver` and `bin/synocli` for Linux.
- `make test`: clears the Go test cache and runs `go test -count=1 -short -v ./...`.
- `make test-sanity`: runs CSI sanity tests in `test/...`.
- `make docker-build`: builds the container image tagged from `Makefile` variables.
- `make clean`: removes generated binaries under `bin/`.

Prefer containerized builds/tests so the toolchain matches the repository:
`podman run --rm -v "$PWD:/workspace:Z" -w /workspace golang:1.26.2-alpine sh -c "apk add --no-cache make alpine-sdk && make build && make test"`.
Use `docker run` with the same arguments if Docker is your local runtime.

## Coding Style & Naming Conventions

Use `gofmt` on changed Go files. Keep package names short and lowercase. Public identifiers use `CamelCase`; private helpers use `camelCase`. Test files should use the same package unless black-box testing is intentional.

Keep changes surgical. Match the existing package boundaries: CSI RPC behavior belongs in `pkg/driver`, DSM orchestration in `pkg/dsm/service`, and raw HTTP details in `pkg/dsm/webapi`.

## Testing Guidelines

Use Go’s built-in `testing` package. Name tests `TestXxx` and keep table tests local to the behavior they cover. Prefer focused unit tests for parsing, idempotency, retry, and cleanup paths. For CSI changes, update `pkg/driver/*_test.go` and run `make test`; use `make test-sanity` for broader contract checks.

## Commit & Pull Request Guidelines

Recent history uses concise Conventional Commit-style messages, for example `fix(dsm): ...`, `perf: ...`, `refactor: ...`, and `chore: ...`. Follow that style when possible.

Pull requests should include a summary, linked issue when applicable, deployment or compatibility notes, and the exact verification run, such as `make test` or `make build`.

## Security & Configuration Tips

Never commit real DSM credentials. Use `config/client-info-template.yml` as the example and keep local secrets in `config/client-info.yml` or Kubernetes secrets. Be careful with `--debug`; DSM API query logging may expose sensitive request data.
