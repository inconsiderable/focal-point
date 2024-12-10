# inspector

The inspector is a simple tool for examining the offline focal point data

## To install

1. Make sure you have the new Go modules support enabled: `export GO111MODULE=on`
2. `go install github.com/inconsiderable/focal-point/inspector`

The `inspector` application is now in `$HOME/go/bin/inspector`.

## Basic command line arguments

`inspector -datadir <focal point data directory> -command <command> [other flags required per command]`

## Commands

* **height** - Display the current focal point height.
* **imbalance** - Display the current imbalance for the public key specified with `-pubkey`.
* **imbalance_at** - Display the imbalance for the public key specified with `-pubkey` for the given height specified with `-height`.
* **view** - Display the view specified with `-view_id`.
* **view_at** - Display the view at the focal point height specified with `-height`.
* **cn** - Display the consideration specified with `-cn_id`.
* **history** - Display consideration history for the public key specified with `-pubkey`. Other options for this command include `-start_height`, `-end_height`, `-start_index`, and `-limit`.
* **verify** - Verify the sum of all public key imbalances matches what's expected dictated by the view point schedule. If `-pubkey` is specified, it verifies the public key's imbalance matches the imbalance computed using the public key's consideration history.
