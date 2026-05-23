// demos/websocket-go/app.go
//
// This is a minimal WebSocket echo server implemented with the Go standard
// library. It is intentionally direct HTTP, not HARP-proxied: the current HARP
// transport supports HTTP request/response and server-to-client response
// streaming, but not full-duplex WebSocket upgrade tunneling yet.
package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

var addr = flag.String("addr", ":8091", "HTTP listen address")

func main() {
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/ws", websocketHandler)
	mux.HandleFunc("/harp-note", harpNoteHandler)

	log.Printf("WebSocket demo listening on http://localhost%s", *addr)
	log.Println("Open http://localhost:8091/ in a browser")
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html>
<html>
<head><meta charset="utf-8"><title>HARP WebSocket Demo</title></head>
<body>
<h1>WebSocket Echo Demo</h1>
<p>This endpoint is direct HTTP. HARP does not proxy full-duplex WebSocket upgrades yet.</p>
<pre id="log"></pre>
<script>
const log = document.getElementById("log");
const ws = new WebSocket("ws://" + location.host + "/ws");
ws.onopen = () => {
  log.textContent += "open\n";
  ws.send("hello from browser");
};
ws.onmessage = event => log.textContent += "message: " + event.data + "\n";
ws.onclose = () => log.textContent += "closed\n";
ws.onerror = () => log.textContent += "error\n";
</script>
</body>
</html>`)
}

func harpNoteHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusUpgradeRequired)
	fmt.Fprintln(w, "WebSocket requires full-duplex HTTP upgrade tunneling.")
	fmt.Fprintln(w, "The current HARP transport supports SSE and chunked response streaming, but not WebSocket proxying yet.")
}

func websocketHandler(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "expected WebSocket upgrade", http.StatusUpgradeRequired)
		return
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		log.Printf("hijack failed: %v", err)
		return
	}
	defer conn.Close()

	accept := websocketAccept(key)
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n")
	fmt.Fprintf(rw, "Upgrade: websocket\r\n")
	fmt.Fprintf(rw, "Connection: Upgrade\r\n")
	fmt.Fprintf(rw, "Sec-WebSocket-Accept: %s\r\n\r\n", accept)
	if err := rw.Flush(); err != nil {
		log.Printf("handshake flush failed: %v", err)
		return
	}

	reader := rw.Reader
	for {
		opcode, payload, err := readFrame(reader)
		if err != nil {
			if err != io.EOF {
				log.Printf("read frame failed: %v", err)
			}
			return
		}
		switch opcode {
		case 0x1:
			reply := fmt.Sprintf("echo at %s: %s", time.Now().Format(time.RFC3339), payload)
			if err := writeFrame(conn, 0x1, []byte(reply)); err != nil {
				log.Printf("write frame failed: %v", err)
				return
			}
		case 0x8:
			_ = writeFrame(conn, 0x8, nil)
			return
		case 0x9:
			if err := writeFrame(conn, 0xA, payload); err != nil {
				log.Printf("write pong failed: %v", err)
				return
			}
		}
	}
}

func websocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + websocketGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func readFrame(r *bufio.Reader) (byte, []byte, error) {
	first, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	second, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	opcode := first & 0x0F
	masked := second&0x80 != 0
	length := uint64(second & 0x7F)
	switch length {
	case 126:
		var buf [2]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(buf[:]))
	case 127:
		var buf [8]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(buf[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}

func writeFrame(conn net.Conn, opcode byte, payload []byte) error {
	header := []byte{0x80 | opcode}
	switch {
	case len(payload) < 126:
		header = append(header, byte(len(payload)))
	case len(payload) <= 65535:
		header = append(header, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		header = append(header, 127)
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(len(payload)))
		header = append(header, buf[:]...)
	}
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err := conn.Write(payload)
	return err
}
