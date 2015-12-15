package main

/*
 * websocket/pty proxy server:
 * This program wires a websocket to a pty master.
 *
 * Usage:
 * go build -o ws-pty-proxy server.go
 * ./websocket-terminal -cmd /bin/bash -addr :9000 -static $HOME/src/websocket-terminal
 * ./websocket-terminal -cmd /bin/bash -- -i
 *
 * TODO:
 *  * make more things configurable
 *  * switch back to binary encoding after fixing term.js (see index.html)
 *  * make errors return proper codes to the web client
 *
 * Copyright 2014 Al Tobey tobert@gmail.com
 * MIT License, see the LICENSE file
 */

import (
	"flag"
	"github.com/codenvy/websocket"
	"github.com/codenvy/pty"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"encoding/json"
	"bufio"
	"bytes"
	"unicode/utf8"
)

// The address to run this http server on.
// May be either "IP:PORT" or just ":PORT" default is ":9000".
// It is set by "addr" command line argument.
var addressFlag string

// The command to execute on slave side of the pty.
// The default value is "/bin/bash".
// It is set by "cmd" command line argument.
var commandFlag string

// The path to the static content.
// The default value is path to the current directory.
// It is set by "static" command line argument.
var staticContentPathFlag string

// environment variable TERM=xterm
// used as environment for executing commandFlag command
const DefaultTerm = "xterm";

// default size of the pty
const DefaultTermRows = 60;
const DefaultTermCols = 200;

// default buffer size for reading from pty file
const defaultPtyBufferSize = 8192;


type WsPty struct {
	// pty builds on os.exec
	command *exec.Cmd

	// a pty is simply an os.File
	ptyFile *os.File
}

// Executes command + starts pty
// Command output is written to the certain file managed by pty.
func startPty() (wsPty *WsPty, err error) {
	// create command, from command flag and left arguments
	command := exec.Command(commandFlag, flag.Args()...)

	// set command environment
	osEnv := os.Environ()
	osEnv = append(osEnv, "TERM=" + DefaultTerm)
	command.Env = osEnv;

	// start pty
	ptyFile, err := pty.Start(command)
	if (err != nil) {
		return nil, err
	}

	// adjust pty
	pty.Setsize(ptyFile, DefaultTermRows, DefaultTermCols);

	return &WsPty {
		command,
		ptyFile,
	}, err
}

// FIXME close in the true way
func (wp *WsPty) Stop() {
	wp.ptyFile.Close()
	wp.command.Wait()
}

// Sets websocket limitations
// TODO: Should check if user has access to the terminal
var upgrader = websocket.Upgrader {

	// Limits the size of the input message to the 1 byte
	ReadBufferSize:  1,

	// Limits the size of the output message to the 1 byte
	WriteBufferSize: 1,

	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Copy everything from the pty master to the websocket using UTF-8 encoding.
// This function contains forever cycle which may stop in the following reasons:
// * When any error occurs during reading from pty file
// * When any error occurs during runes reading(from pty buffer)
// * When any error occurs during writing to the websocket channel
func transferPtyFileContentToWebsocket(conn *websocket.Conn, ptyFile *os.File) {
	// buffer which keeps ptyFile bytes
	ptyBuf        := make([]byte, defaultPtyBufferSize)
	ptyFileReader := bufio.NewReader(ptyFile)
	tmpBuf        := new(bytes.Buffer);
	// TODO: more graceful exit on socket close / process exit
	for {
		ptyBytesRead, err := ptyFileReader.Read(ptyBuf);
		if err != nil {
			log.Printf("Failed to read from pty master: %s", err)
			return
		}
		runeReader := bufio.NewReader(bytes.NewReader(append(tmpBuf.Bytes()[:], ptyBuf[:ptyBytesRead]...)))
		tmpBuf.Reset()
		// read byte array as Unicode code points (rune in go)
		// runes explained https://blog.golang.org/strings
		i := 0;
		for i < ptyBytesRead {
			runeChar, runeLen, err := runeReader.ReadRune()
			if err != nil {
				log.Printf("Failed to read rune from the pty rune buffer: %s", err)
				return
			}
			if runeChar == utf8.RuneError {
				runeReader.UnreadRune()
				break
			}
			tmpBuf.WriteRune(runeChar)
			i += runeLen;
		}
		// At this point of time tmp buffer may contain [0, bytesRead) bytes
		// which are going to be written into the websocket channel
		err = conn.WriteMessage(websocket.TextMessage, tmpBuf.Bytes())
		if err != nil {
			log.Printf("Failed to write UTF-8 character to the webscoket channel: %s", err)
			return
		}
		// appending all bytes which were not processed from the pty buffer
		// to the tmp buffer that allows to read runes in the next iteration
		tmpBuf.Reset();
		if i < ptyBytesRead {
			tmpBuf.Write(ptyBuf[i:ptyBytesRead])
		}
	}
}

// Handles /pty requests, starts os process and transfers its output
// to the dedicated websocket channel
func ptyHandler(httpWriter http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(httpWriter, r, nil)
	if err != nil {
		log.Printf("Websocket upgrade failed: %s\n", err)
		// TODO not always unauthorized
		httpWriter.WriteHeader(http.StatusUnauthorized)
		return
	}
	defer conn.Close();

	wsPty, err := startPty();
	if err != nil {
		httpWriter.WriteHeader(http.StatusInternalServerError)
		return
	}

	go transferPtyFileContentToWebsocket(conn, wsPty.ptyFile)

	type Message struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}

	// read from the web socket, copying to the pty master
	// messages are expected to be text and base64 encoded
	for {
		mt, payload, err := conn.ReadMessage()
		if err != nil && err != io.EOF {
			log.Printf("conn.ReadMessage failed: %s\n", err)
			return
		}
		var msg Message;
		switch mt {
		case websocket.BinaryMessage:
			log.Printf("Ignoring binary message: %q\n", payload)
		case websocket.TextMessage:
			err := json.Unmarshal(payload, &msg);
			if err != nil {
				log.Printf("Invalid message %s\n", err);
				continue
			}
			switch msg.Type {
			case "resize" :
				var size []float64;
				err := json.Unmarshal(msg.Data, &size)
				if err != nil {
					log.Printf("Invalid resize message: %s\n", err);
				} else {
					pty.Setsize(wsPty.ptyFile, uint16(size[1]), uint16(size[0]));
				}
			case "data" :
				var dat string;
				err := json.Unmarshal(msg.Data, &dat);
				if err != nil {
					log.Printf("Invalid data message %s\n", err);
				} else {
					wsPty.ptyFile.Write([]byte(dat));
				}
			default:
				log.Printf("Invalid message type %d\n", mt)
				return
			}

		default:
			log.Printf("Invalid message type %d\n", mt)
			return
		}
	}
	wsPty.Stop()
}

func init() {
	cwd, _ := os.Getwd()
	flag.StringVar(&addressFlag,           "addr",   ":9000",     "IP:PORT or :PORT address to listen on")
	flag.StringVar(&commandFlag,           "cmd",    "/bin/bash", "command to execute on slave side of the pty")
	flag.StringVar(&staticContentPathFlag, "static", cwd,         "path to static content")
	// TODO: make sure paths exist and have correct permissions
}

func main() {
	flag.Parse()

	http.HandleFunc("/pty", ptyHandler)

	// serve html & javascript
	http.Handle("/", http.FileServer(http.Dir(staticContentPathFlag)))

	err := http.ListenAndServe(addressFlag, nil)
	if (err != nil ) {
		log.Fatalf("net.http could not listen on address '%s': %s\n", addressFlag, err)
	}
}
