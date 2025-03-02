module github.com/SimonWaldherr/HARP/demos/simple-go

go 1.23

replace github.com/SimonWaldherr/HARP/harp => ../../harp

require (
	github.com/SimonWaldherr/HARP/harp v0.0.0-00010101000000-000000000000
	google.golang.org/grpc v1.70.0
)

require (
	golang.org/x/net v0.32.0 // indirect
	golang.org/x/sys v0.28.0 // indirect
	golang.org/x/text v0.21.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20241202173237-19429a94021a // indirect
	google.golang.org/protobuf v1.36.5 // indirect
)
