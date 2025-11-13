foo-component.wasm: foo-with-wit.wasm wasi_snapshot_preview1.reactor.wasm
	wasm-tools component new  --adapt wasi_snapshot_preview1.reactor.wasm $< --output $@

foo-with-wit.wasm: foo.wasm wit/test.wit
	wasm-tools component embed wit $< --output $@

foo.wasm: foo.go go/bin/go
	GOOS=wasip1 GOARCH=wasm ./go/bin/go build -o $@ -buildmode=c-shared -ldflags=-checklinkname=0 $<

wasi_snapshot_preview1.reactor.wasm:
	curl -OL https://github.com/bytecodealliance/wasmtime/releases/download/v38.0.4/wasi_snapshot_preview1.reactor.wasm

go/bin/go:
	git submodule update --init --recursive
	(cd go/src && bash make.bash)

.PHONY: test
test: foo-component.wasm
	TEST_COMPONENT=$$(pwd)/$< cargo test --release --manifest-path test/Cargo.toml
