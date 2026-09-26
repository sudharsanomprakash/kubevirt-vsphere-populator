# Release checklist

The project is **experimental**. No production release tag may be created
until every gate in section A has passed and its evidence is linked here.

## A. Production acceptance gates

| # | Gate | Status | Evidence |
|---|---|---|---|
| A1 | `diagnose --full-read` against at least one real vCenter and one standalone ESXi release; lease keys, response headers, Range behavior, format and manifest recorded in docs/design.md | **Not passed** (no vSphere lab available) | — |
| A2 | Cold migration of a disposable real VM (Filesystem and Block PVCs); source disk compared with the destination by an independent method (e.g. `sha256sum` of a flat copy of the source vs. the PVC) | **Not passed** | — |
| A3 | A KubeVirt VM boots from a populated PVC (Filesystem and Block) | **Not passed** (KubeVirt images unavailable in the development environment) | — |
| A4 | Multi-disk VM on real vSphere: every disk maps to the right PVC | **Not passed** | — |
| A5 | Real lease behaviors: renewal over a long transfer, abort on worker kill, ready timeout, manifest verification | **Not passed** | — |
| A6 | Single-stream benchmark against real vSphere with versions, datastore type, disk sizes, transport path, elapsed time, throughput, worker CPU and ESXi host impact | **Not passed** | — |
| A7 | Controller-level lease key convention confirmed for controllers whose number differs from their bus number (currently rejected) | **Not passed** | — |
| A8 | Security review of RBAC, Secret handling and the `--allowed-server-pattern` model by someone other than the author | **Not passed** | — |

## B. Engineering gates (must pass for any tag, including pre-releases)

| # | Gate | Command |
|---|---|---|
| B1 | Unit, integration and envtest suites | `make test` |
| B2 | Race detector | `make test-race` |
| B3 | Lint, vet, formatting | `make lint vet` and `gofmt -l .` |
| B4 | Generated code and CRD up to date | `make verify-generate` |
| B5 | Images build and start | `make images` |
| B6 | kind end-to-end test (fake vSphere) including the QEMU boot check | `make e2e-kind` |
| B7 | `THIRD_PARTY_NOTICES.md` regenerated after dependency changes | see the file header |
| B8 | No credentials, internal names or customer data in the repository | review, plus a secret scan |

## C. Release steps (pre-releases only while section A is open)

1. Update `CHANGELOG.md` with what is implemented, tested and measured.
2. Confirm B1–B8 on the commit to be tagged.
3. Tag `v0.x.y-alpha.N` (never a plain `vX.Y.Z` while A is open) and mark the
   GitHub release as a pre-release with the experimental notice.
4. Build and push images to a registry you control; record their digests in
   the release notes and pin them in `deploy/`.
