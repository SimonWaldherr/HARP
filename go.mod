module github.com/SimonWaldherr/HARP

go 1.25.0

require (
	github.com/SimonWaldherr/HARP/harp v1.6.0
	github.com/SimonWaldherr/HARP/harpserver v1.1.7
	github.com/google/uuid v1.6.0
	github.com/gorilla/mux v1.8.1
	github.com/quic-go/quic-go v0.59.1
	google.golang.org/grpc v1.81.1
)

require (
	github.com/quic-go/qpack v0.6.0 // indirect
	go.uber.org/mock v0.6.0 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260523011958-0a33c5d7ca68 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace (
	github.com/SimonWaldherr/HARP/harp => ./harp
	github.com/SimonWaldherr/HARP/harpserver => ./harpserver
)
