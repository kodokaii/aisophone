package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	openairt "github.com/WqyJh/go-openai-realtime"
	"github.com/coder/websocket"
)

const (
	ADDR  = ":8787"
	MODEL = openairt.GPT4oMiniRealtimePreview20241217

	FRAME_TIME         = 20 * time.Millisecond
	FRAME_WAIT_DIVISOR = 4
	FRAME_SIZE_MAX     = 65500

	QUEUE_SIZE_MAX  = 1400
	QUEUE_SIZE_XOFF = 1000
	QUEUE_SIZE_XON  = 700
)

type Config struct {
	ApiKey string
}

type Asterisk struct {
	conn             *websocket.Conn
	pipe             *AudioPipe
	paused           atomic.Bool
	onPipeEmpty      chan func()
	resumeCond       *sync.Cond
	connectionID     string
	channel          string
	optimalFrameSize int
}

func NewAsterisk(conn *websocket.Conn) *Asterisk {
	asterisk := &Asterisk{
		conn:        conn,
		pipe:        NewAudioPipe(),
		resumeCond:  sync.NewCond(&sync.Mutex{}),
		onPipeEmpty: make(chan func(), 1),
	}
	return asterisk
}

func (asterisk *Asterisk) Close() {
	if asterisk.pipe != nil {
		asterisk.pipe.Close()
	}
	if asterisk.conn != nil {
		asterisk.conn.Close(websocket.StatusNormalClosure, "Connection closed")
	}
	asterisk.Resume()
}

func (asterisk *Asterisk) Pause() {
	asterisk.paused.Store(true)
}

func (asterisk *Asterisk) Resume() {
	if asterisk.paused.Load() {
		asterisk.paused.Store(false)
		asterisk.resumeCond.Signal()
	}
}

func (asterisk *Asterisk) WaitForResume() {
	asterisk.resumeCond.L.Lock()
	defer asterisk.resumeCond.L.Unlock()
	if asterisk.paused.Load() {
		asterisk.resumeCond.Wait()
	}
}

func (asterisk *Asterisk) SendText(ctx context.Context, msg string) {
	data := []byte(msg)
	err := asterisk.conn.Write(ctx, websocket.MessageText, data)
	if err != nil {
		log.Printf("Error sending text message to Asterisk: %v", err)
		return
	}
}

func (asterisk *Asterisk) SendAudio(base64Audio string) {
	audio, err := base64.StdEncoding.DecodeString(base64Audio)
	if err != nil {
		log.Printf("Failed to decode base64 audio data: %v", err)
		return
	}
	asterisk.pipe.Write(audio)
}

func (asterisk *Asterisk) ParseMediaStart(text string) error {
	fields := strings.Split(text, " ")
	for _, field := range fields[1:] {
		parts := strings.SplitN(field, ":", 2)
		if len(parts) == 2 {
			key, value := parts[0], parts[1]
			switch key {
			case "connection_id":
				asterisk.connectionID = value
			case "channel":
				asterisk.channel = value
			case "optimal_frame_size":
				size, err := strconv.Atoi(value)
				if err != nil {
					return fmt.Errorf("invalid optimal_frame_size: %v", err)
				}
				asterisk.optimalFrameSize = size
			}
		}
	}
	if asterisk.optimalFrameSize <= 0 {
		return fmt.Errorf("optimal_frame_size missing")
	}
	return nil
}

func (asterisk *Asterisk) AddOnPipeEmpty(f func()) error {
	select {
	case asterisk.onPipeEmpty <- f:
		return nil
	default:
		return fmt.Errorf("onPipeEmpty channel is full")
	}
}

func (asterisk *Asterisk) RunOnPipeEmpty() {
	for {
		select {
		case f := <-asterisk.onPipeEmpty:
			f()
		default:
			return
		}
	}
}

func (asterisk *Asterisk) ClearOnPipeEmpty() {
	for {
		select {
		case <-asterisk.onPipeEmpty:
		default:
			return
		}
	}
}

type OpenAI struct {
	conn *openairt.Conn
}

func NewOpenAI(conn *openairt.Conn) *OpenAI {
	openai := &OpenAI{
		conn: conn,
	}
	return openai
}

func (openai *OpenAI) Close() {
	if openai.conn != nil {
		openai.conn.Close()
	}
}

type Session struct {
	ctx      context.Context
	cancel   context.CancelFunc
	asterisk *Asterisk
	openai   *OpenAI
}

func NewSession(cfg Config, w http.ResponseWriter, r *http.Request) (*Session, error) {
	var err error
	session := &Session{}

	session.ctx, session.cancel = context.WithCancel(context.Background())

	asteriskConn, err := websocket.Accept(w, r, nil)
	if err != nil {
		session.Close()
		return nil, err
	}
	session.asterisk = NewAsterisk(asteriskConn)
	session.asterisk.conn.SetReadLimit(-1)

	clientOpenai := openairt.NewClient(cfg.ApiKey)
	openaiConn, err := clientOpenai.Connect(session.ctx, openairt.WithModel(MODEL))
	if err != nil {
		session.Close()
		return nil, err
	}
	session.openai = NewOpenAI(openaiConn)

	err = session.openai.conn.SendMessage(session.ctx, openairt.SessionUpdateEvent{
		Session: openairt.ClientSession{
			TurnDetection: &openairt.ClientTurnDetection{
				Type: openairt.ClientTurnDetectionTypeServerVad,
			},
		},
	})
	if err != nil {
		session.Close()
		return nil, err
	}

	return session, nil
}

func (session *Session) Close() {
	if session.cancel != nil {
		session.cancel()
	}
	if session.asterisk != nil {
		session.asterisk.Close()
	}
	if session.openai != nil {
		session.openai.Close()
	}
}

func (session *Session) StreamAsterisk(done chan struct{}) {
	defer func() { done <- struct{}{} }()

	lastPacketTime := time.Now()
	lastPacketSize := 0

	audio := make([]byte, FRAME_SIZE_MAX)
	for {
		n, err := session.asterisk.pipe.Read(audio)
		if err != nil {
			log.Printf("Error reading audio from pipe: %v", err)
			break
		}

		timeSinceLastPacket := time.Since(lastPacketTime)
		timeToWait := time.Duration(lastPacketSize)*FRAME_TIME/time.Duration(session.asterisk.optimalFrameSize*FRAME_WAIT_DIVISOR) - timeSinceLastPacket
		if 0 < timeToWait {
			time.Sleep(timeToWait)
		}

		session.asterisk.WaitForResume()

		err = session.asterisk.conn.Write(session.ctx, websocket.MessageBinary, audio[:n])
		if err != nil {
			log.Printf("Error sending audio to Asterisk: %v", err)
			break
		}

		session.asterisk.SendText(session.ctx, "GET_STATUS")

		if session.asterisk.pipe.Len() == 0 {
			session.asterisk.RunOnPipeEmpty()
		}

		lastPacketTime = time.Now()
		lastPacketSize = n
	}
}

func (session *Session) HandleAsterisk(done chan struct{}) {
	defer func() { done <- struct{}{} }()
	for {
		messageType, msg, err := session.asterisk.conn.Read(session.ctx)
		if err != nil {
			log.Printf("Error reading from Asterisk WebSocket: %v", err)
			break
		}

		switch messageType {
		case websocket.MessageText:
			text := string(msg)
			switch {
			case strings.HasPrefix(text, "MEDIA_START"):
				if err := session.asterisk.ParseMediaStart(text); err != nil {
					log.Printf("Error parsing MEDIA_START: %v", err)
					break
				}
				session.asterisk.SendText(session.ctx, "ANSWER")
			case strings.HasPrefix(text, "MEDIA_XOFF"):
				session.asterisk.Pause()
			case strings.HasPrefix(text, "MEDIA_XON"):
				session.asterisk.Resume()
			}

		case websocket.MessageBinary:
			base64Audio := base64.StdEncoding.EncodeToString(msg)
			session.openai.conn.SendMessage(session.ctx, &openairt.InputAudioBufferAppendEvent{
				Audio: base64Audio,
			})
		}
	}
	session.Close()
}

func (session *Session) HandleOpenAI(done chan struct{}) {
	defer func() { done <- struct{}{} }()

	var currentResponseID string

	for {
		msg, err := session.openai.conn.ReadMessage(session.ctx)
		if err != nil {
			log.Printf("Error reading from OpenAI WebSocket: %v", err)
			break
		}

		switch msg.ServerEventType() {
		case openairt.ServerEventTypeInputAudioBufferSpeechStarted:
			currentResponseID = ""
			session.asterisk.ClearOnPipeEmpty()
			session.asterisk.pipe.Clear()
			session.asterisk.SendText(session.ctx, "FLUSH_MEDIA")
			session.asterisk.Resume()
		case openairt.ServerEventTypeResponseContentPartAdded:
			msg := msg.(openairt.ResponseContentPartAddedEvent)
			if msg.Part.Type == openairt.MessageContentTypeAudio {
				currentResponseID = msg.ResponseID
				session.asterisk.SendText(session.ctx, "START_MEDIA_BUFFERING")
			}
		case openairt.ServerEventTypeResponseAudioDelta:
			msg := msg.(openairt.ResponseAudioDeltaEvent)
			if msg.ResponseID == currentResponseID {
				session.asterisk.SendAudio(msg.Delta)
			}
		case openairt.ServerEventTypeResponseAudioDone:
			msg := msg.(openairt.ResponseAudioDoneEvent)
			if msg.ResponseID == currentResponseID {
				err := session.asterisk.AddOnPipeEmpty(func() {
					session.asterisk.SendText(session.ctx, "STOP_MEDIA_BUFFERING")
				})
				if err != nil {
					log.Printf("Error adding onPipeEmpty callback: %v", err)
				}
			}
		default:
			//log.Printf("Unhandled OpenAI message type: %s", msg.ServerEventType())
		}
	}
}

func handleCall(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Println("New call received")

		session, err := NewSession(cfg, w, r)
		if err != nil {
			log.Printf("Failed to create session: %v", err)
			http.Error(w, "Failed to create session", http.StatusInternalServerError)
			return
		}
		defer session.Close()

		done := make(chan struct{}, 3)

		go session.HandleAsterisk(done)
		go session.HandleOpenAI(done)
		go session.StreamAsterisk(done)

		<-done
		session.Close()
		<-done
		<-done
		log.Println("Session closed")
	}
}

func main() {
	cfg := Config{
		ApiKey: os.Getenv("OPENAI_API_KEY"),
	}

	if cfg.ApiKey == "" {
		log.Fatal("OPENAI_API_KEY environment variable is required")
	}

	http.HandleFunc("/", handleCall(cfg))

	err := http.ListenAndServe(ADDR, nil)
	if err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
