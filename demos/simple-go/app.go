// demo_applications/app.go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	pb "github.com/SimonWaldherr/HARP/harp"
	"google.golang.org/grpc"
)

var (
	proxyAddress = flag.String("proxy", "localhost:50051", "Address of the HARP proxy")
)

func main() {
	flag.Parse()

	// Connect to the HARP proxy's gRPC server.
	conn, err := grpc.Dial(*proxyAddress, grpc.WithInsecure())
	if err != nil {
		log.Fatalf("Failed to connect to proxy: %v", err)
	}
	defer conn.Close()

	client := pb.NewHarpServiceClient(conn)
	ctx := context.Background()
	stream, err := client.Proxy(ctx)
	if err != nil {
		log.Fatalf("Failed to create gRPC stream: %v", err)
	}

	// Send registration message.
	reg := &pb.Registration{
		Name:   "DemoBackend",
		Domain: ".*",
		Key:    "123",
		Routes: []*pb.Route{
			{
				Name:   "TestRoute",
				Path:   "/test",
				Port:   8080,
				Domain: ".*",
			},
		},
	}
	regMsg := &pb.ClientMessage{
		Payload: &pb.ClientMessage_Registration{
			Registration: reg,
		},
	}
	if err := stream.Send(regMsg); err != nil {
		log.Fatalf("Failed to send registration: %v", err)
	}
	log.Println("Registration sent. Waiting for HTTP requests...")

	// Listen for HTTP requests from the proxy.
	for {
		serverMsg, err := stream.Recv()
		if err != nil {
			log.Fatalf("Error receiving from proxy: %v", err)
		}
		httpReq := serverMsg.GetHttpRequest()
		if httpReq == nil {
			continue
		}
		log.Printf("Received HTTP request: %s %s", httpReq.Method, httpReq.Url)

		// Simulate processing delay.
		time.Sleep(2 * time.Second)

		// Create an HTTP response.
		httpResp := &pb.HTTPResponse{
			Status:    200,
			Headers:   map[string]string{"Content-Type": "text/plain"},
			Body:      fmt.Sprintf("Hello from DemoBackend! Processed %s", httpReq.Url),
			RequestId: httpReq.RequestId,
			Timestamp: time.Now().UnixNano(),
			Cacheable: true,
			Latency:   int64(2 * time.Second),
		}
		respMsg := &pb.ClientMessage{
			Payload: &pb.ClientMessage_HttpResponse{
				HttpResponse: httpResp,
			},
		}
		if err := stream.Send(respMsg); err != nil {
			log.Printf("Error sending response: %v", err)
		}
	}
}
