# Contributing

Thank you for your interest. This project is experimental; correctness and
honest reporting of what has and has not been verified matter more than
features.

## Development setup

* Go: `go.mod` pins the toolchain (Go 1.27.1); with `GOTOOLCHAIN=auto` the Go
  command downloads it.
* `qemu-img` (for example `apt-get install qemu-utils`).
* Docker, for images and the kind end-to-end test.

```sh
make build          # binaries in ./bin
make fixtures       # fetch pinned third-party test fixtures
make test           # full suite (qemu-img, fixtures, envtest required)
make test-race      # same with the race detector
make lint vet       # golangci-lint v2 and go vet
make generate       # deepcopy and CRD after API changes
make images         # controller and worker images
make e2e-kind       # kind end-to-end test (fake vSphere; see test/e2e/README.md)
```

Loop-device tests need root and are skipped otherwise; CI runs them.

## Pull requests

* Keep changes focused; add tests that fail without the change.
* Anything that talks to vSphere must be exercised against the test double in
  `test/fakevsphere`, including failure paths. If you have access to a real
  vSphere lab, attach a redacted `diagnose` report and say which versions you
  used.
* Do not add claims to the documentation that are not backed by a test or a
  recorded measurement. Mark untested behavior as such.
* Never commit credentials, internal host names or customer data. Examples
  must use reserved names such as `example.test`.
* By contributing you agree that your contribution is licensed under the
  Apache License 2.0.

## Reporting issues

Include the controller version, Kubernetes version, CSI driver, the redacted
`diagnose` output and the worker's termination message. Remove server names
if they are sensitive.
