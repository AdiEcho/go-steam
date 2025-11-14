# Generator for steamlang and protobuf

We generate Go code from SteamKit protocol descriptors, namely `steamlang` files and protocol buffer files.

## Dependencies
1. Update SteamKit/Protobufs submodule: `git submodule update --init --force --remote`.

2. Install [`protoc`](https://developers.google.com/protocol-buffers/docs/downloads), the protocol buffer compiler.

    ```
    ✗ protoc --version
    libprotoc 33.1
    ```

3. Install `protoc-gen-go`: `go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.31.0`

    ```
    ✗ protoc-gen-go --version
    protoc-gen-go v1.31.0
    ```

IMPORTANT: You MUST install ~v1.31.0 or dare face issues parsing the file descriptions at runtime.

4. Install the .NET Core SDK (3.1 or later).

## Execute generator

Execute `go run generator.go clean proto steamlang` to clean build files, then build protocol buffer files and then build steamlang files.

IMPORTANT: Since the generator relies upon relative files, you MUST use `go run` in its directory instead of `go build`

NOTE: You will commonly find missing proto files in the generation when Valve changes proto imports (ie. `unable to determine Go import path for <.....proto>`). 
You can remedy this by adding the relevant proto file to the corresponding generator map (ie. `clientProtoFiles` or `csgoProtoFiles`)
