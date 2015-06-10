gospdyquic, SPDY/QUIC support for Go
====================================

This is a work-in-progress SPDY/QUIC implementation for Go. This is based on
[goquic](https://github.com/devsisters/goquic) library. You can use this library
to add SPDY/QUIC support for your existing Go HTTP server.

QUIC is an experimental protocol aimed at reducing web latency over that of TCP.
On the surface, QUIC is very similar to TCP+TLS+SPDY implemented on UDP. Because
TCP is implement in operating system kernels, and middlebox firmware, making
significant changes to TCP is next to impossible. However, since QUIC is built
on top of UDP, it suffers from no such limitations.

Key features of QUIC over existing TCP+TLS+SPDY include

  * Dramatically reduced connection establishment time
  * Improved congestion control
  * Multiplexing without head of line blocking
  * Forward error correction
  * Connection migration

## Project Status

*This library is highly experimental.* Although `libquic` sources are from
Chromium (which are tested), the Go bindings are still highly pre-alpha state.

See [goquic](https://github.com/devsisters/goquic) for more details about
current project status.


Getting Started
===============

## Dependency

  * [goquic](https://github.com/devsisters/goquic)

## Build static library files

Although prebuilt static library files already exists in the repository for
convenience, it is always good practice to build library files from source. You
should not trust any unverifiable third-party binaries.

To build the library files for your architecture and OS:

```bash
go get github.com/devsisters/goquic
cd $GOPATH/github.com/devsisters/goquic
./build_libs.sh
```

Currently Linux and Mac OS X is supprted.

## How to build

Due to Go 1.4's cgo restrictions, use an environment variable like below to
build your projects. This restriction will be removed from Go 1.5.

```bash
CGO_CFLAGS="-I$GOPATH/src/github.com/devsisters/goquic/libquic/boringssl/include"
CGO_LDFLAGS="-L$GOPATH/src/github.com/devsisters/goquic/lib/$GOOS_$GOARCH"
```

For example, building gospdyquic example server in Mac:

```bash
CGO_CFLAGS="-I$GOPATH/src/github.com/devsisters/goquic/libquic/boringssl/include" CGO_LDFLAGS="-L$GOPATH/src/github.com/devsisters/goquic/lib/darwin_amd64" go build $GOPATH/github.com/devsisters/gospdyquic/example/server.go
```

In Linux:

```bash
CGO_CFLAGS="-I$GOPATH/src/github.com/devsisters/goquic/libquic/boringssl/include" CGO_LDFLAGS="-L$GOPATH/src/github.com/devsisters/goquic/lib/linux_amd64" go build $GOPATH/github.com/devsisters/gospdyquic/example/server.go
```

## How to use server

When running a HTTP server, do:

```go
gospdyquic.ListenAndServe(":8080", 1, nil)
```

instead of

```go
http.ListenAndServe(":8080", nil)
```

## How to use client

You need to create http.Client with Transport changed, do:

```go
client := &http.Client{
	Transport: gospdyquic.NewRoundTripper(false),
}
resp, err := client.Get("http://example.com/")
```

instead of

```go
resp, err := http.Get("http://example.com/")
```
