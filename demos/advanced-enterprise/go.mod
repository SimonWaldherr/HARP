module advanced-enterprise

go 1.23

toolchain go1.24.5

require (
	github.com/SimonWaldherr/HARP/harpserver v1.1.5
	github.com/gorilla/mux v1.8.1
	github.com/gorilla/websocket v1.5.3
)

require (
	github.com/SimonWaldherr/HARP/harp v1.1.6 // indirect
	golang.org/x/net v0.32.0 // indirect
	golang.org/x/sys v0.28.0 // indirect
	golang.org/x/text v0.21.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20241202173237-19429a94021a // indirect
	google.golang.org/grpc v1.70.0 // indirect
	google.golang.org/protobuf v1.36.5 // indirect
)

replace github.com/SimonWaldherr/HARP => ../../
