// demos/multi-service-go/multi_service.go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"time"

	pb "github.com/SimonWaldherr/HARP/harp"
	"google.golang.org/grpc"
)

var proxyAddr = flag.String("proxy", "localhost:50051", "Address of the HARP proxy")

func main() {
	flag.Parse()

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

	// Register multiple routes using the master key.
	reg := &pb.Registration{
		Name:   "MultiServiceBackend",
		Domain: ".*",
		Key:    "master-key",
		Routes: []*pb.Route{
			{Name: "Add", Path: "/math/add", Port: 8083, Domain: ".*"},
			{Name: "Multiply", Path: "/math/multiply", Port: 8083, Domain: ".*"},
			{Name: "Hello", Path: "/hello/", Port: 8083, Domain: ".*"},
			{Name: "Joke", Path: "/joke", Port: 8083, Domain: ".*"},
		},
	}
	if err := stream.Send(&pb.ClientMessage{
		Payload: &pb.ClientMessage_Registration{Registration: reg},
	}); err != nil {
		log.Fatalf("Registration failed: %v", err)
	}
	log.Println("MultiServiceBackend registered. Waiting for requests...")

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

		var response string
		var contentType = "text/plain"
		switch {
		case strings.HasPrefix(req.Url, "/math/add"):
			response, contentType = handleMath(req, "add")
		case strings.HasPrefix(req.Url, "/math/multiply"):
			response, contentType = handleMath(req, "multiply")
		case strings.HasPrefix(req.Url, "/hello/"):
			response, contentType = handleHello(req)
		case req.Url == "/joke":
			response, contentType = handleJoke()
		default:
			response = "404 - Not Found"
		}
		resp := &pb.HTTPResponse{
			Status:    200,
			Headers:   map[string]string{"Content-Type": contentType},
			Body:      response,
			RequestId: req.RequestId,
			Timestamp: time.Now().UnixNano(),
			Cacheable: false,
			Latency:   int64(500 * time.Millisecond),
		}
		if err := stream.Send(&pb.ClientMessage{
			Payload: &pb.ClientMessage_HttpResponse{HttpResponse: resp},
		}); err != nil {
			log.Printf("Error sending response: %v", err)
		}
	}
}

func handleMath(req *pb.HTTPRequest, op string) (string, string) {
	parsedURL, _ := url.Parse(req.Url)
	params := parsedURL.Query()
	a, _ := strconv.Atoi(params.Get("a"))
	b, _ := strconv.Atoi(params.Get("b"))
	var result int
	if op == "add" {
		result = a + b
	} else {
		result = a * b
	}
	respData := map[string]interface{}{
		"operation": op,
		"a":         a,
		"b":         b,
		"result":    result,
	}
	data, _ := json.Marshal(respData)
	return string(data), "application/json"
}

func handleHello(req *pb.HTTPRequest) (string, string) {
	parts := strings.Split(strings.TrimPrefix(req.Url, "/hello/"), "/")
	name := "Guest"
	if len(parts) > 0 && parts[0] != "" {
		name = parts[0]
	}
	return fmt.Sprintf("Hello, %s! Welcome to MultiServiceBackend.", name), "text/plain"
}

func handleJoke() (string, string) {
	jokes := []string{
		"Why don't programmers like nature? It has too many bugs.",
		"Why do Java developers wear glasses? Because they don't C#.",
		"Why was the developer unhappy at their job? They wanted arrays.",
	}
	rand.Seed(time.Now().UnixNano())
	return jokes[rand.Intn(len(jokes))], "text/plain"
}
