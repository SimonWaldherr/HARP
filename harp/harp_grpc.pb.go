// Code generated by protoc-gen-go-grpc. DO NOT EDIT.
// versions:
// - protoc-gen-go-grpc v1.5.1
// - protoc             v5.29.3
// source: harp/harp.proto

package harp

import (
	context "context"
	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
// Requires gRPC-Go v1.64.0 or later.
const _ = grpc.SupportPackageIsVersion9

const (
	HarpService_Proxy_FullMethodName = "/harp.HarpService/Proxy"
)

// HarpServiceClient is the client API for HarpService service.
//
// For semantics around ctx use and closing/ending streaming RPCs, please refer to https://pkg.go.dev/google.golang.org/grpc/?tab=doc#ClientConn.NewStream.
//
// Bidirectional streaming RPC.
type HarpServiceClient interface {
	Proxy(ctx context.Context, opts ...grpc.CallOption) (grpc.BidiStreamingClient[ClientMessage, ServerMessage], error)
}

type harpServiceClient struct {
	cc grpc.ClientConnInterface
}

func NewHarpServiceClient(cc grpc.ClientConnInterface) HarpServiceClient {
	return &harpServiceClient{cc}
}

func (c *harpServiceClient) Proxy(ctx context.Context, opts ...grpc.CallOption) (grpc.BidiStreamingClient[ClientMessage, ServerMessage], error) {
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	stream, err := c.cc.NewStream(ctx, &HarpService_ServiceDesc.Streams[0], HarpService_Proxy_FullMethodName, cOpts...)
	if err != nil {
		return nil, err
	}
	x := &grpc.GenericClientStream[ClientMessage, ServerMessage]{ClientStream: stream}
	return x, nil
}

// This type alias is provided for backwards compatibility with existing code that references the prior non-generic stream type by name.
type HarpService_ProxyClient = grpc.BidiStreamingClient[ClientMessage, ServerMessage]

// HarpServiceServer is the server API for HarpService service.
// All implementations must embed UnimplementedHarpServiceServer
// for forward compatibility.
//
// Bidirectional streaming RPC.
type HarpServiceServer interface {
	Proxy(grpc.BidiStreamingServer[ClientMessage, ServerMessage]) error
	mustEmbedUnimplementedHarpServiceServer()
}

// UnimplementedHarpServiceServer must be embedded to have
// forward compatible implementations.
//
// NOTE: this should be embedded by value instead of pointer to avoid a nil
// pointer dereference when methods are called.
type UnimplementedHarpServiceServer struct{}

func (UnimplementedHarpServiceServer) Proxy(grpc.BidiStreamingServer[ClientMessage, ServerMessage]) error {
	return status.Errorf(codes.Unimplemented, "method Proxy not implemented")
}
func (UnimplementedHarpServiceServer) mustEmbedUnimplementedHarpServiceServer() {}
func (UnimplementedHarpServiceServer) testEmbeddedByValue()                     {}

// UnsafeHarpServiceServer may be embedded to opt out of forward compatibility for this service.
// Use of this interface is not recommended, as added methods to HarpServiceServer will
// result in compilation errors.
type UnsafeHarpServiceServer interface {
	mustEmbedUnimplementedHarpServiceServer()
}

func RegisterHarpServiceServer(s grpc.ServiceRegistrar, srv HarpServiceServer) {
	// If the following call pancis, it indicates UnimplementedHarpServiceServer was
	// embedded by pointer and is nil.  This will cause panics if an
	// unimplemented method is ever invoked, so we test this at initialization
	// time to prevent it from happening at runtime later due to I/O.
	if t, ok := srv.(interface{ testEmbeddedByValue() }); ok {
		t.testEmbeddedByValue()
	}
	s.RegisterService(&HarpService_ServiceDesc, srv)
}

func _HarpService_Proxy_Handler(srv interface{}, stream grpc.ServerStream) error {
	return srv.(HarpServiceServer).Proxy(&grpc.GenericServerStream[ClientMessage, ServerMessage]{ServerStream: stream})
}

// This type alias is provided for backwards compatibility with existing code that references the prior non-generic stream type by name.
type HarpService_ProxyServer = grpc.BidiStreamingServer[ClientMessage, ServerMessage]

// HarpService_ServiceDesc is the grpc.ServiceDesc for HarpService service.
// It's only intended for direct use with grpc.RegisterService,
// and not to be introspected or modified (even as a copy)
var HarpService_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "harp.HarpService",
	HandlerType: (*HarpServiceServer)(nil),
	Methods:     []grpc.MethodDesc{},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "Proxy",
			Handler:       _HarpService_Proxy_Handler,
			ServerStreams: true,
			ClientStreams: true,
		},
	},
	Metadata: "harp/harp.proto",
}
