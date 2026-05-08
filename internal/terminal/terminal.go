package terminal

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Message struct {
	Type string `json:"type"` // "input" | "resize" | "output" | "error" | "exit"
	Data string `json:"data"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// HandleWS handles a terminal WebSocket connection.
// Each connection spawns a /bin/sh shell session.
func HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("terminal ws upgrade: %v", err)
		return
	}
	defer conn.Close()

	// Start shell
	cmd := exec.Command("/bin/sh")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		sendMsg(conn, "error", "failed to get stdin: "+err.Error())
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		sendMsg(conn, "error", "failed to get stdout: "+err.Error())
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		sendMsg(conn, "error", "failed to get stderr: "+err.Error())
		return
	}

	if err := cmd.Start(); err != nil {
		sendMsg(conn, "error", "failed to start shell: "+err.Error())
		return
	}

	var mu sync.Mutex
	closed := false

	safeSend := func(msgType, data string) {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		sendMsg(conn, msgType, data)
	}

	// Stream stdout
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			safeSend("output", scanner.Text()+"\n")
		}
	}()

	// Stream stderr
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			safeSend("output", scanner.Text()+"\n")
		}
	}()

	// Wait for process exit
	go func() {
		err := cmd.Wait()
		exitMsg := "Process exited"
		if err != nil {
			exitMsg = "Process exited: " + err.Error()
		}
		time.Sleep(200 * time.Millisecond)
		safeSend("exit", exitMsg)
		mu.Lock()
		closed = true
		mu.Unlock()
		conn.Close()
	}()

	// Read from WebSocket -> write to stdin
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("terminal ws read: %v", err)
			}
			break
		}

		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "input":
			if _, err := io.WriteString(stdin, msg.Data); err != nil {
				safeSend("error", "stdin write failed: "+err.Error())
			}
		case "ping":
			safeSend("pong", "")
		}
	}

	// Kill the process when connection closes
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func sendMsg(conn *websocket.Conn, msgType, data string) {
	msg := Message{Type: msgType, Data: data}
	b, _ := json.Marshal(msg)
	_ = conn.WriteMessage(websocket.TextMessage, b)
}
