// demos/simple-go/app.go
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

var proxyAddr = flag.String("proxy", "localhost:50054", "Address of the HARP proxy")

func main() {
	flag.Parse()

	// Connect to the proxy.
	conn, err := grpc.Dial(*proxyAddr, grpc.WithInsecure())
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

	// Register a test route "/enhanced-test" using the master key.
	reg := &pb.Registration{
		Name:   "EnhancedTestBackend",
		Domain: ".*",
		Key:    "master-key",
		Routes: []*pb.Route{
			{
				Name:   "EnhancedTestRoute",
				Path:   "/enhanced-test",
				Port:   8083,
				Domain: ".*",
			},
		},
	}
	if err := stream.Send(&pb.ClientMessage{
		Payload: &pb.ClientMessage_Registration{Registration: reg},
	}); err != nil {
		log.Fatalf("Failed to send registration: %v", err)
	}
	log.Println("EnhancedTestBackend registered. Waiting for requests...")

	// Listen for incoming HTTP requests.
	for {
		msg, err := stream.Recv()
		if err != nil {
			log.Fatalf("Error receiving from proxy: %v", err)
		}
		req := msg.GetHttpRequest()
		if req == nil {
			continue
		}
		log.Printf("Received request: %s %s", req.Method, req.Url)
		// Simulate processing.
		time.Sleep(1 * time.Second)
		response := fmt.Sprintf(`{"message": "Hello from Enhanced HARP Backend!", "url": "%s", "method": "%s", "features": {"caching": "enabled", "metrics": "enabled", "rate_limiting": "enabled"}}`, req.Url, req.Method)
		resp := &pb.HTTPResponse{
			Status: 200,
			Headers: map[string]string{
				"Content-Type": "application/json",
				"X-Backend":    "Enhanced-HARP-v2",
			},
			Body:      response,
			RequestId: req.RequestId,
			Timestamp: time.Now().UnixNano(),
			Cacheable: true,
			Latency:   int64(10 * time.Millisecond),
		}
		if err := stream.Send(&pb.ClientMessage{
			Payload: &pb.ClientMessage_HttpResponse{HttpResponse: resp},
		}); err != nil {
			log.Printf("Error sending response: %v", err)
		}
	}
}
