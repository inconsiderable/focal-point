# Client

## Building the Client

To build the latest mind binaries from master, simply invoke the Go toolchain like so:

```
$ export GO111MODULE=on
$ go get -v github.com/inconsiderable/focal-point/client
$ go install -v github.com/inconsiderable/focal-point/client
```

The focalpoint bins should now be available in your go-managed `$GOPATH/bin` (which is hopefully also on your `$PATH`). You can test this by running `client -h` to print the help screen.

## CLI Options

Client help is available via the `client -h` command:

```
$ client -h
Usage of /home/focalpoint/go/bin/client:
  -compress
        Compress views on disk with lz4
  -datadir string
        Path to a directory to save focal point data
  -dnsseed
        Run a DNS server to allow others to find peers
  -inlimit int
        Limit for the number of inbound peer connections. (default 128)
  -keyfile string
        Path to a file containing public keys to use when rendering
  -memo string
        A memo to include in newly rendered views
  -noaccept
        Disable inbound peer connections
  -noirc
        Disable use of IRC for peer discovery
  -numrenderers int
        Number of renderers to run (default 1)
  -peer string
        Address of a peer to connect to
  -port int
        Port to listen for incoming peer connections (default 8832)
  -prune
        Prune consideration and public key consideration indices
  -pubkey string
        A public key which receives newly rendered view points
  -tlscert string
        Path to a file containing a PEM-encoded X.509 certificate to use with TLS
  -tlskey string
        Path to a file containing a PEM-encoded private key to use with TLS
  -upnp
        Attempt to forward the focalpoint port on your router with UPnP
```

## Running the Client

The client requires a data dir for storage of the focal-point and general metadata as well as one or more public keys to send view points to upon rendering. Otherwise, running the client is as simple as:

```
$ client -datadir view-data -keyfile keys.txt -numrenderers 2
```

### Configuring Peer Discovery

The client supports two modes of peer discovery: DNS with IRC as fallback.

If you want to run a DNS server to help enable peer discovery, you can pass the `-dnsseed` flag.

If you wish to disable IRC discovery, that can be disabled via the `-noirc` flag.

If you wish to enable UPnP port forwarding for the client node, use the `-upnp` flag.

### Configuring Renderers

In order to effectively render, you'll typically want to run one renderer per CPU core on your machine. This is configured via the `-numrenderers` param, like so:

```
$ client ... -numrenderers 4
```

To run a renderer-less node, you can pass `0` as the number of renderers like so:

```
$ client ... -numrenderers 0
```

### Configuring Keys

The client supports two modes of view point considerations for rendering: single key and key list targets.

To distribute view points to a single key, use the `-pubkey` flag to pass the target public key in the CLI command.

To distribute view points to multiple keys, use the `-keyfile` flag with a text file of the public keys (one per line).

> NOTE: The mind components `dumpkeys` command will generate a `keys.txt` for you as part of mind setup.

## Terminating the client

The client runs synchronously in the current window, so to exit simply hit control-c for a graceful shutdown.
