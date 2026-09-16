# ixgo Program Integration

`testdata` is copied verbatim from `github.com/goplus/ixgo@v1.1.6/testdata`, with the upstream BSD license included. `TestUpstreamPrograms` runs all 18 distinct programs selected by upstream `TestTestdataFiles`; upstream lists `static.go` twice. This is the program corpus, not every ixgo unit test or every standard-library patch test.

The host creates and initializes the interpreter, obtains its existing `main` callback and passes that callback through `sandbox.Run`. The original program's assertions run in Sentry, followed by a completion value written back to the host.

`roundtrip_test.go` also carries the former transport tests for shared environments, returned closures, reflected captures, class state, multiple interpreters, source dependencies, local generic types and external bindings. It checks both the guest result and subsequent calls through the original host callbacks. Standard stream references, reflected interface values and native `reflect.MakeFunc` callbacks use the same public entry.

From this directory, run on Linux amd64 or arm64:

```sh
GLIBC_TUNABLES=glibc.pthread.rseq=0 SANDBOX_TEST_LIBRARY=/absolute/path/sentrylib.so go test -ldflags='-checklinkname=0 -s=false -w=false' -v .
```
