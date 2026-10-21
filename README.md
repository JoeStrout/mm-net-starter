# Mini Micro Networking Starter

A minimal multiplayer demo for [Mini Micro](https://miniscript.org/MiniMicro): click to move, type to chat. A small Go server keeps an event log for each game, and clients poll it over HTTP about once a second.

## Server

Requires Go 1.23 or later. To run it locally:

```sh
cd server
go run .          # listens on port 8737; use -addr to change
```

To build for a Linux server:

```sh
cd server
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o net-server-linux-amd64 .
```

Copy `net-server-linux-amd64` to the server and run it there. No Go installation is needed on the server. Make sure its firewall allows TCP port 8737.

## Client

Set `SERVER` at the top of `client/netClient.ms` to your server's address, then run it in Mini Micro.
