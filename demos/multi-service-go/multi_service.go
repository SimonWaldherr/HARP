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

var (
	proxyAddress = flag.String("proxy", "simonwaldherr.de:50051", "Address of the HARP proxy")
)

// Predefined jokes for the /joke route
var jokes = []string{
	"Why don't programmers like nature? It has too many bugs.",
	"Why do Java developers wear glasses? Because they don't C#.",
	"Why was the developer unhappy at their job? They wanted arrays.",
}

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

	// Register multiple routes
	reg := &pb.Registration{
		Name:   "MultiService",
		Domain: ".*",
		Key:    "123",
		Routes: []*pb.Route{
			{Name: "Add", Path: "/math/add", Port: 8083, Domain: ".*"},
			{Name: "Multiply", Path: "/math/multiply", Port: 8083, Domain: ".*"},
			{Name: "Hello", Path: "/hello/", Port: 8083, Domain: ".*"},
			{Name: "Joke", Path: "/joke", Port: 8083, Domain: ".*"},
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
	log.Println("MultiService registered. Waiting for HTTP requests...")

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

		// Handle different routes
		var response string
		var contentType = "text/plain"

		switch {
		case strings.HasPrefix(httpReq.Url, "/math/add"):
			response, contentType = handleMath(httpReq, "add")
		case strings.HasPrefix(httpReq.Url, "/math/multiply"):
			response, contentType = handleMath(httpReq, "multiply")
		case strings.HasPrefix(httpReq.Url, "/hello/"):
			response, contentType = handleHello(httpReq)
		case httpReq.Url == "/joke":
			response, contentType = handleJoke()
		default:
			response = "404 - Not Found"
		}

		// Send the response
		httpResp := &pb.HTTPResponse{
			Status:    200,
			Headers:   map[string]string{"Content-Type": contentType},
			Body:      response,
			RequestId: httpReq.RequestId,
			Timestamp: time.Now().UnixNano(),
			Cacheable: false,
			Latency:   int64(500 * time.Millisecond),
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

// Handle math operations (add, multiply)
func handleMath(req *pb.HTTPRequest, operation string) (string, string) {
	parsedURL, _ := url.Parse(req.Url)
	params := parsedURL.Query()
	a, _ := strconv.Atoi(params.Get("a"))
	b, _ := strconv.Atoi(params.Get("b"))

	var result int
	switch operation {
	case "add":
		result = a + b
	case "multiply":
		result = a * b
	}

	respData := map[string]interface{}{
		"operation": operation,
		"a":         a,
		"b":         b,
		"result":    result,
	}
	respJSON, _ := json.Marshal(respData)
	return string(respJSON), "application/json"
}

// Handle /hello/{name} requests
func handleHello(req *pb.HTTPRequest) (string, string) {
	parts := strings.Split(strings.TrimPrefix(req.Url, "/hello/"), "/")
	name := "Guest"
	if len(parts) > 0 && parts[0] != "" {
		name = parts[0]
	}
	return fmt.Sprintf("Hello, %s! Welcome to MultiService.", name), "text/plain"
}

// Handle /joke requests
func handleJoke() (string, string) {
	rand.Seed(time.Now().UnixNano())
	joke := jokes[rand.Intn(len(jokes))]
	return joke, "text/plain"
}
