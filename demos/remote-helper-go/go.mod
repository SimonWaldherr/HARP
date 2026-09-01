module remote-helper-demo

go 1.25.0

require github.com/SimonWaldherr/HARP/harpserver v1.1.8-0.20260523160642-59b86864ee1e

require (
	github.com/SimonWaldherr/HARP/harp v1.1.8-0.20260523160642-59b86864ee1e // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260831171406-18b4a7587f8a // indirect
	google.golang.org/grpc v1.83.2 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace (
	github.com/SimonWaldherr/HARP/harp => ../../harp
	github.com/SimonWaldherr/HARP/harpserver => ../../harpserver
)
