package harp

import (
	"log"

	"github.com/gorilla/websocket"
)

type HandlerFunc func(*HTTPRequest, func(*HTTPResponse) error)

type Server struct {
	routes []Route
}

func NewServer() *Server {
	return &Server{}
}

func (s *Server) HandleFunc(path string, handler HandlerFunc) {
	route := Route{
		Name:    "App-pages",
		Path:    path,
		RegExp:  "^" + path + "$",
		Port:    80,
		Handler: handler,
	}
	s.routes = append(s.routes, route)
}

func (s *Server) ListenAndServe(server string) error {
	var ws *websocket.Conn
	var err error
	ws, _, err = websocket.DefaultDialer.Dial(server, nil)
	if err != nil {
		log.Println("Error connecting to WebSocket server:", err)
		return err
	}
	defer ws.Close()

	reg := &Registration{
		Name:   "App",
		Domain: ".*",
		Key:    "123456",
		Routes: make([]Route, len(s.routes)),
	}

	for i, route := range s.routes {
		reg.Routes[i] = Route{
			Name:   route.Name,
			Path:   route.Path,
			RegExp: route.RegExp,
			Port:   route.Port,
		}
	}

	err = ws.WriteJSON(reg)
	if err != nil {
		log.Println("Error sending registration:", err)
		return err
	}

	sendResponse := func(res *HTTPResponse) error {
		err := ws.WriteJSON(res)
		if err != nil {
			log.Println("Error sending response:", err)
		}
		return err
	}

	for {
		req := &HTTPRequest{}

		err := ws.ReadJSON(req)
		if err != nil {
			log.Println("Error reading JSON:", err)
			break
		}

		for _, route := range s.routes {
			if route.Handler != nil && route.Path == req.URL {
				route.Handler(req, sendResponse)
				break
			}
		}
	}

	return nil
}
