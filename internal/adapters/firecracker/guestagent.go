package firecracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
)

// guestConfigFile is the configuration written into the VM overlay and read by
// the guest agent at boot.
type guestConfigFile struct {
	Services []guestService `json:"services"`
}

// guestService is one service of a task microVM.
type guestService struct {
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	Command []string          `json:"command,omitempty"`
	// Entrypoint replaces the default binary of the rootfs, which compose
	// semantics express through entrypoint rather than command.
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Port      int               `json:"port,omitempty"`
}

// postJSON writes one JSON request to the guest agent.
func postJSON(conn net.Conn, path string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	request := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: guest-agent\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", path, len(payload))
	if _, err := conn.Write([]byte(request)); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	_, err = conn.Write(payload)
	return err
}

// readJSON reads one JSON response from the guest agent.
func readJSON(conn net.Conn, target any) error {
	buffer := make([]byte, 1<<20)
	total := 0
	for {
		if total == len(buffer) {
			return errors.New("guest agent response too large")
		}
		read, err := conn.Read(buffer[total:])
		if read > 0 {
			total += read
			// The guest agent answers with a single small JSON document, so
			// the first complete chunk is the whole response.
			if body, ok := extractJSON(buffer[:total]); ok {
				if err := json.Unmarshal(body, target); err != nil {
					return fmt.Errorf("decode guest agent response: %w", err)
				}
				return nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && total > 0 {
				return errors.New("guest agent closed the connection without a JSON body")
			}
			if errors.Is(err, io.EOF) {
				return io.EOF
			}
			return err
		}
	}
}

// extractJSON pulls a balanced JSON object out of an HTTP response body.
func extractJSON(raw []byte) ([]byte, bool) {
	start := -1
	for i, b := range raw {
		if b == '{' {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, false
	}
	depth := 0
	for i := start; i < len(raw); i++ {
		switch raw[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return raw[start : i+1], true
			}
		}
	}
	return nil, false
}
