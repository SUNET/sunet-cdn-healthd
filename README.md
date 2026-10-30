# sunet-cdn-healthd

This health check daemon runs on `cache` machines and is queried by
[sunet-cdn-agent](https://github.com/SUNET/sunet-cdn-agent) running on
`l4lb` machines so they can decide if they should keep sending them traffic.

Currently checks only aim to verify whole-machine-availability, per-tenant
state is not inspected.

## Reasoning when adding new health checks
Since the CDN service is supposed to protect against DDoS traffic we
need to be careful when adding checks that could become flaky under load. An
overloaded machine is probably better kept in rotation than shedding its load
to remaining machines that would then struggle even more.

Ideally only judge health based on characteristics that should remain stable
even when a machine is struggling with incoming traffic. E.g. the initial
health check only verifies that the tunnel interfaces are present and up. This
is a check that is very unlikely to become unreliable even under load, and if we
dont have the interfaces or they are down we can be sure that the machine will
not be able handle any traffic at all.

The sunet-cdn-agent checks from `l4lb` nodes also look for connection problems,
so even if we have no machine health checks at all just the fact that we are
responding is used to decide if the machine is available.

## Development
### Formatting and linting
When working with this code at least the following tools are expected to be
run at the top level directory prior to commiting:

* `gofumpt -l -w .` (see [gofumpt](https://github.com/mvdan/gofumpt))
* `go vet ./...`
* `staticcheck ./...` (see [staticcheck](https://staticcheck.io))
* `gosec ./...` (see [gosec](https://github.com/securego/gosec))
* `golangci-lint run` (see [golangci-lint](https://golangci-lint.run))
* `go test -race ./...`
* `govulncheck ./...` (see [govulncheck](https://go.dev/doc/tutorial/govulncheck))
