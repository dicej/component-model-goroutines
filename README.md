## component-model-goroutines

This is a rough proof-of-concept demonstrating how the [WebAssembly Component
Model](https://github.com/WebAssembly/component-model/tree/main)'s [concurrency
model](https://github.com/WebAssembly/component-model/blob/main/design/mvp/Concurrency.md)
can be used idiomatically in Go via goroutines.

[The example](./foo.go) exports an
[async-lifted](https://github.com/WebAssembly/component-model/blob/main/design/mvp/Concurrency.md#async-export-abi)
function which the host may call any number of times concurrently.  That
function, in turn, spawns three goroutines, each of which calls an
[async-lowered](https://github.com/WebAssembly/component-model/blob/main/design/mvp/Concurrency.md#async-import-abi)
import function.  The import function blocks asynchronously (sleeping for a few
milliseconds), creating component model subtasks and causing the goroutines to
suspend and thus the original export call to return control to the host until
one or more of the subtasks complete.  As each subtask completes, the respective
goroutines resume.  Once all subtasks have completed, the results are collected
and returned to the host.

Note that this example contains a lot of boilerplate which will eventually be
generated automatically from the
[WIT](https://github.com/WebAssembly/component-model/blob/main/design/mvp/WIT.md)
files or else packaged as part of a reusable library.  For now, everything is
handwritten.

### Build and test

Prequisites:

- Bash
- Make
- Rust and Cargo 1.90 or newer
- Go 1.25.0 or newer

The following will:

1. build [a slightly modified version of Go](https://github.com/dicej/go/tree/component-model-async), then
1. build the example with it, producing a Wasm module, then
1. download the [wasi-preview1-component-adapter](https://github.com/bytecodealliance/wasmtime/tree/main/crates/wasi-preview1-component-adapter), then
1. combine the Wasm module and adapter into a component, then
1. build a [Wasmtime](https://github.com/bytecodealliance/wasmtime)-based test harness, then
1. run the test!

The test calls the function exported by the guest three times concurrently with
different arguments then asserts that the results match the expected values.

```
make test
```
