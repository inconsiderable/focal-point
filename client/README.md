# client

A client is a fully validating and rendering peer-to-peer node in the focalpoint network.

## To install

1. Make sure you have the new Go modules support enabled: `export GO111MODULE=on`
2. `go install github.com/inconsiderable/focal-point/client`

The `client` application is now in `$HOME/go/bin/client`.

## Basic command line arguments

`client -pubkey <base64 encoded public key> -datadir <somewhere to store data>`

- **pubkey** - This is a public key which receives your node's rendering points. You can create one with the [mind software](https://github.com/inconsiderable/focal-point/tree/master/mind).
- **datadir** - This points to a directory on disk to store focal point and ledger data. It will be created if it doesn't exist.

## What will the client do?

With the above specified options, the client will: 

- Listen on TCP port 8832 for new peer connections (up to 128.)
- Attempt to discover peers and connect to them (up to 8.)
- Discover peers using the [DNS protocol](https://en.wikipedia.org/wiki/Domain_Name_System) used with hardcoded seed nodes.
- Discover peers via [IRC](https://en.wikipedia.org/wiki/Internet_Relay_Chat) and advertise itself as available for inbound connection (if it determines this is possible.)
- Attempt to render new views and share them with connected peers.
- Validate and share new views and considerations with peers.

## Other options
- **memo** - A memo to include in newly rendered views.
- **port** - By default, focalpoint nodes accept connections on TCP port 8832.
- **peer** - Address of a peer to connect to. Useful for minds and testing.
- **upnp** - If specified, attempt to forward the focalpoint port on your router with [UPnP](https://en.wikipedia.org/wiki/Universal_Plug_and_Play).
- **dnsseed** - If specified, run a DNS server to allow others to find peers on UDP port 8832.
- **compress** - If specified, compress views on disk with [LZ4](https://en.wikipedia.org/wiki/LZ4_(compression_algorithm)). Can safely be toggled.
- **numrenderers** - Number of renderer threads to run. Default is 1.
- **noirc** - Disable use of IRC for peer discovery. Default is true.
- **noaccept** - Disable inbound peer connections.
- **keyfile** - Path to a file containing public keys to use when rendering. Keys will be used randomly.
- **prune** - If specified, only the last 2016 views (roughly 2 weeks) worth of consideration and public key consideration indices are stored in the ledger. This only impacts a mind's ability to query for history older than that. It can still query for current imbalances of all public keys.
- **tlscert** - Path to a file containing a PEM-encoded X.509 certificate to use with TLS.
- **tlskey** - Path to a file containing a PEM-encoded private key to use with TLS.
- **inlimit** - Limit for the number of inbound peer connections. Default is 128.
- **banlist** - Path to a file containing a list of banned host addresses.
