# HARP

[![Go Report Card](https://goreportcard.com/badge/github.com/simonwaldherr/harp)](https://goreportcard.com/report/github.com/simonwaldherr/harp) 
[![GPL License](https://img.shields.io/badge/license-GPL-blue)](https://github.com/SimonWaldherr/HARP/blob/main/LICENSE)  

![Harp Logo](GolangHarp.png)

HARP (HTTP Autoregister Reverse Proxy) is an inverting reverse proxy server which allows backend applications to register themselves and accept incoming requests from the internet without being directly exposed. The backend applications specify the domain and target URL, and the proxy server forwards incoming requests to the appropriate backend based on the domain in the request.

**HARP is currently WIP and not production ready.**

## how it works

* The proxy server listens for incoming HTTP requests.
* Backend applications register themselves by connecting via WebSocket and sending routing-information.
* When an incoming request is received, the proxy server checks if a backend is registered for the request's domain and path.
* If a matching backend is found, the proxy server forwards the request to the backend and returns the backend's response to the client.
* If no matching backend is found, the proxy server returns a 404 Not Found error.

## usage

Deploy the proxy server on a publicly accessible server.
Start your backend application (it even works on a non-publicly accessible server).

## use cases

the final project will help you for the following demands:

* Caching
* Dynamic Scaling and Load Balancing
* Service Migration
* Internal Tooling and Debugging
* Local Development
* Microservices Development
* Temporary Services

## sequence diagram

```mermaid
sequenceDiagram
    box Application
    participant B1 as Backend Service 1
    participant B2 as Backend Service 2
    end
    box HARP
    participant H as HARP
    participant CM as Cache Mechanism
    end
    
    box Client
    participant C as Client
    end
 
    B1->>H: WebSocket Registration (domain/path)
    B2->>H: WebSocket Registration (domain/path)
    Note over B1,H: the WebSocket connection stays open<br/>and is used for all further communication
 
    loop Every 5s
        H-->>B1: WebSocket Ping
        H-->>B2: WebSocket Ping
        B1-->>H: WebSocket Pong
        B2-->>H: WebSocket Pong
    end
 
    C->>H: HTTP Request (domain/path)
    H->>CM: Check for Cached Response
    alt Cached Response Exists
        CM->>H: Return Cached Response
        H->>C: Return Cached Response
    else No Cached Response
        CM->>H: No Cached Response
        alt Request Matches Backend 1
            H-->>B1: Forward Request to Backend 1
            B1-->>H: Response
            H->>CM: Cache the Response
            CM-->>H: Confirmation
            H->>C: Return Response
        else Request Matches Backend 2
            H-->>B2: Forward Request to Backend 2
            B2-->>H: Response
            H->>CM: Cache the Response
            CM-->>H: Confirmation
            H->>C: Return Response
        end
    end
```
