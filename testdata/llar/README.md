# LLAR Formula Integration

The five `_llar.gox` fixtures are copied without changes from `github.com/goplus/llar/internal/formula/testdata/formula` at commit `bd473ea21632d57a4ed21e54144a55d0ee6c1872`. The test uses that revision's real `internal/formula.LoadFS`, class registration and package exports. The module path permits importing LLAR's internal loader without adding LLAR to the sandbox library's dependencies.

Run in Linux amd64 or arm64 with Go 1.26.6, a matching Sentry shared library and `pkg-config` installed:

```sh
GLIBC_TUNABLES=glibc.pthread.rseq=0 SANDBOX_TEST_LIBRARY=/absolute/path/sentrylib.so go test -ldflags='-checklinkname=0 -s=false -w=false' -v .
```

Each formula is loaded on the host. Its callbacks execute through `sandbox.Run`; the host then checks completion, environment isolation, dependency writeback and generated pkg-config metadata. The fixtures use a writable temporary bind mount for output. These tests require a Linux environment capable of running the Sentry backend.
