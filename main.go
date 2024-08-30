package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	msginterfaces "github.com/deepgram/deepgram-go-sdk/pkg/api/speak/v1/websocket/interfaces"
	clientinterfaces "github.com/deepgram/deepgram-go-sdk/pkg/client/interfaces"
	client "github.com/deepgram/deepgram-go-sdk/pkg/client/speak"
)

type MyHandler struct {
	binaryChan  chan *[]byte
	openChan    chan *msginterfaces.OpenResponse
	flushedChan chan *msginterfaces.FlushedResponse
	closeChan   chan *msginterfaces.CloseResponse
	errorChan   chan *msginterfaces.ErrorResponse

	wsUI *websocket.Conn
}

func NewMyHandler(uiWebsocket *websocket.Conn) MyHandler {
	handler := MyHandler{
		binaryChan:  make(chan *[]byte),
		openChan:    make(chan *msginterfaces.OpenResponse),
		flushedChan: make(chan *msginterfaces.FlushedResponse),
		closeChan:   make(chan *msginterfaces.CloseResponse),
		errorChan:   make(chan *msginterfaces.ErrorResponse),
		wsUI:        uiWebsocket,
	}

	go func() {
		handler.Run()
	}()

	return handler
}

// GetUnhandled returns the binary event channels
func (dch MyHandler) GetBinary() []*chan *[]byte {
	return []*chan *[]byte{&dch.binaryChan}
}

// GetOpen returns the open channels
func (dch MyHandler) GetOpen() []*chan *msginterfaces.OpenResponse {
	return []*chan *msginterfaces.OpenResponse{&dch.openChan}
}

// GetMetadata returns the metadata channels
func (dch MyHandler) GetMetadata() []*chan *msginterfaces.MetadataResponse {
	return []*chan *msginterfaces.MetadataResponse{}
}

// GetFlushed returns the flush channels
func (dch MyHandler) GetFlush() []*chan *msginterfaces.FlushedResponse {
	return []*chan *msginterfaces.FlushedResponse{&dch.flushedChan}
}

// GetClose returns the close channels
func (dch MyHandler) GetClose() []*chan *msginterfaces.CloseResponse {
	return []*chan *msginterfaces.CloseResponse{&dch.closeChan}
}

// GetWarning returns the warning channels
func (dch MyHandler) GetWarning() []*chan *msginterfaces.WarningResponse {
	return []*chan *msginterfaces.WarningResponse{}
}

// GetError returns the error channels
func (dch MyHandler) GetError() []*chan *msginterfaces.ErrorResponse {
	return []*chan *msginterfaces.ErrorResponse{&dch.errorChan}
}

// GetUnhandled returns the unhandled event channels
func (dch MyHandler) GetUnhandled() []*chan *[]byte {
	return []*chan *[]byte{}
}

// Open is the callback for when the connection opens
// golintci: funlen
func (dch MyHandler) Run() error {
	wgReceivers := sync.WaitGroup{}

	// open channel
	wgReceivers.Add(1)
	go func() {
		defer wgReceivers.Done()

		for or := range dch.openChan {
			fmt.Printf("------------ [OPEN] Deepgram WebSocket connection opened\n")

			// Send metadata to the UI
			openJSON, err := json.Marshal(or)
			if err != nil {
				log.Println("Failed to marshal open to JSON:", err)
				continue
			}

			fmt.Printf("Open JSON: %s\n", openJSON)
			dch.wsUI.WriteMessage(websocket.TextMessage, openJSON)
		}
	}()

	// flushed channel
	wgReceivers.Add(1)
	go func() {
		defer wgReceivers.Done()

		for fr := range dch.flushedChan {
			fmt.Printf("------------ [FLUSHED] Final Binary\n")

			// Send metadata to the UI
			flushedJSON, err := json.Marshal(fr)
			if err != nil {
				log.Println("Failed to marshal flushed to JSON:", err)
				continue
			}

			fmt.Printf("Flushed JSON: %s\n", flushedJSON)
			dch.wsUI.WriteMessage(websocket.TextMessage, flushedJSON)
		}
	}()

	// binary channel
	wgReceivers.Add(1)
	go func() {
		defer wgReceivers.Done()

		lastTime := time.Now().Add(-5 * time.Second)

		for br := range dch.binaryChan {
			if time.Since(lastTime) > 3*time.Second {
				fmt.Printf("------------ [Binary Data] Attach header.\n")

				// Add a wav audio container header to the file if you want to play the audio
				// using a media player like VLC, Media Player, or Apple Music
				header := []byte{
					0x52, 0x49, 0x46, 0x46, // "RIFF"
					0x00, 0x00, 0x00, 0x00, // Placeholder for file size
					0x57, 0x41, 0x56, 0x45, // "WAVE"
					0x66, 0x6d, 0x74, 0x20, // "fmt "
					0x10, 0x00, 0x00, 0x00, // Chunk size (16)
					0x01, 0x00, // Audio format (1 for PCM)
					0x01, 0x00, // Number of channels (1)
					0x80, 0xbb, 0x00, 0x00, // Sample rate (48000)
					0x00, 0xee, 0x02, 0x00, // Byte rate (48000 * 2)
					0x02, 0x00, // Block align (2)
					0x10, 0x00, // Bits per sample (16)
					0x64, 0x61, 0x74, 0x61, // "data"
					0x00, 0x00, 0x00, 0x00, // Placeholder for data size
				}

				dch.wsUI.WriteMessage(websocket.BinaryMessage, header)
				lastTime = time.Now()
			}

			fmt.Printf("------------ [Binary Data] (len: %d)\n", len(*br))
			dch.wsUI.WriteMessage(websocket.BinaryMessage, *br)
		}
	}()

	// close channel
	wgReceivers.Add(1)
	go func() {
		defer wgReceivers.Done()

		for cr := range dch.closeChan {
			fmt.Printf("------------ [Close] Deepgram WebSocket connection closed\n")

			// Send metadata to the UI
			closeJSON, err := json.Marshal(cr)
			if err != nil {
				log.Println("Failed to marshal close to JSON:", err)
				continue
			}

			fmt.Printf("Close JSON: %s\n", closeJSON)
			dch.wsUI.WriteMessage(websocket.TextMessage, closeJSON)
		}
	}()

	// error channel
	wgReceivers.Add(1)
	go func() {
		defer wgReceivers.Done()

		for er := range dch.errorChan {
			fmt.Printf("------------ [Error] ErrCode: %s\n", er.ErrCode)
			fmt.Printf("ErrMsg: %s\n", er.ErrMsg)
			fmt.Printf("Description: %s\n", er.Description)

			// Send metadata to the UI
			errorJSON, err := json.Marshal(er)
			if err != nil {
				log.Println("Failed to marshal error to JSON:", err)
				continue
			}

			fmt.Printf("Error JSON: %s\n", errorJSON)
			dch.wsUI.WriteMessage(websocket.TextMessage, errorJSON)
		}
	}()

	// wait for all receivers to finish
	wgReceivers.Wait()

	return nil
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var requestData struct {
	Text string `json:"text"`
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// get the model from the query string
	model := r.URL.Query().Get("model")

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Failed to upgrade connection to WebSocket:", err)
		return
	}
	defer conn.Close()

	if model == "" {
		fmt.Println("No model specified, using default model")
		model = "aura-asteria-en"
	}

	// context
	ctx := context.Background()

	// client options, if needed
	cOptions := clientinterfaces.ClientOptions{}

	// Create a new Deepgram WebSocket client
	sOptions := clientinterfaces.WSSpeakOptions{
		Model:      model,
		Encoding:   "linear16",
		SampleRate: 48000,
	}
	callback := NewMyHandler(conn)

	wsClient, err := client.NewWSUsingChan(ctx, "", &cOptions, &sOptions, callback)
	if err != nil {
		log.Fatalf("Failed to create WebSocket client: %v", err)
		return
	}

	// Wait for the connection to be established
	isConnected := wsClient.Connect()
	if !isConnected {
		log.Fatalf("Failed to connect to Deepgram")
		return
	}

	for {
		// Read message from the WebSocket connection
		msgType, message, err := conn.ReadMessage()
		if err != nil {
			log.Println("Failed to read message from WebSocket:", err)
			break
		}

		if msgType == websocket.TextMessage {
			err = json.Unmarshal(message, &requestData)
			if err != nil {
				log.Println("Failed to unmarshal JSON:", err)
				continue
			}

			if requestData.Text == "" {
				log.Println("Text is required in the request")
				continue
			}

			log.Printf("Text: %s\n", requestData.Text)

			err = wsClient.SpeakWithText(requestData.Text)
			if err != nil {
				log.Println("Failed to send text to Deepgram:", err)
			}

			err = wsClient.Flush()
			if err != nil {
				fmt.Printf("Error flushing: %v\n", err)
				return
			}
		}
	}
}

func main() {
	client.Init(client.InitLib{
		LogLevel: client.LogLevelDefault, // LogLevelDefault, LogLevelFull, LogLevelDebug, LogLevelTrace
	})

	fs := http.FileServer(http.Dir("./public"))
	http.Handle("/", fs)
	http.HandleFunc("/ws", handleWebSocket)

	fmt.Printf("Open the UI at http://localhost:3000\n")
	log.Fatal(http.ListenAndServe(":3000", nil))
}
