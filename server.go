package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/giongto35/cloud-morph/pkg/addon/textchat"
	"github.com/giongto35/cloud-morph/pkg/common/ws"
	"github.com/giongto35/cloud-morph/pkg/core/go/cloudgame"
	"github.com/gofrs/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v2"
	"gopkg.in/yaml.v2"
)

var webrtcconfig = webrtc.Configuration{ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}}

var isStarted bool

var upgrader = websocket.Upgrader{}

const configFilePath = "config.yaml"

var curApp string = "Notepad"

const indexPage string = "web/index.html"
const addr string = ":8080"

// TODO: multiplex clientID
var clientID string

type Server struct {
	httpServer *http.Server
	// browserClients are the map clientID to browser Client
	clients    map[string]*Client
	gameEvents chan cloudgame.WSPacket
	chatEvents chan textchat.ChatMessage
	cgame      cloudgame.CloudGameClient
	chat       *textchat.TextChat
}

type Client struct {
	conn     *websocket.Conn
	clientID string

	serverEvents chan cloudgame.WSPacket
	chatEvents   chan textchat.ChatMessage
	videoStream  chan rtp.Packet
	videoTrack   *webrtc.Track
	done         chan struct{}
	// TODO: Get rid of ssrc
	ssrc uint32
}

// GetWeb returns web frontend
func (o *Server) GetWeb(w http.ResponseWriter, r *http.Request) {
	tmpl, err := template.ParseFiles(indexPage)
	if err != nil {
		log.Fatal(err)
	}

	tmpl.Execute(w, nil)
}

func NewClient(c *websocket.Conn, clientID string, ssrc uint32, serverEvents chan cloudgame.WSPacket, chatEvents chan textchat.ChatMessage) *Client {
	return &Client{
		conn:         c,
		clientID:     clientID,
		serverEvents: serverEvents,
		chatEvents:   chatEvents,
		videoStream:  make(chan rtp.Packet, 1),
		ssrc:         ssrc,
		done:         make(chan struct{}),
	}
}

func NewServer() *Server {
	server := &Server{
		clients:    map[string]*Client{},
		gameEvents: make(chan cloudgame.WSPacket, 1),
		chatEvents: make(chan textchat.ChatMessage, 1),
	}

	r := mux.NewRouter()
	r.HandleFunc("/ws", server.WS)
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir("./web"))))
	// r.HandleFunc("/signal", server.Signalling)

	r.PathPrefix("/").HandlerFunc(server.GetWeb)

	svmux := &http.ServeMux{}
	svmux.Handle("/", r)

	httpServer := &http.Server{
		Addr:         addr,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		IdleTimeout:  120 * time.Second,
		Handler:      svmux,
	}
	server.httpServer = httpServer
	log.Println("Spawn server")

	// Launch Game VM
	cfg, err := readConfig(configFilePath)
	if err != nil {
		panic(err)
	}

	log.Println("config: ", cfg)
	server.cgame = cloudgame.NewCloudGameClient(cfg, server.gameEvents)
	server.chat = textchat.NewTextChat(server.chatEvents)

	return server
}

func (o *Server) Handle() {
	// Spawn CloudGaming Handle
	go o.cgame.Handle()
	// Spawn Chat Handle
	go o.chat.Handle()

	// Fanout output channel
	go func() {
		for p := range o.cgame.VideoStream() {
			for _, client := range o.clients {
				client.videoStream <- p
			}
		}
	}()
}

func (o *Server) ListenAndServe() error {
	log.Println("Server is running at", addr)
	return o.httpServer.ListenAndServe()
}

// WSO handles all connections from user/frontend to coordinator
func (o *Server) WS(w http.ResponseWriter, r *http.Request) {
	log.Println("A user is connecting...")
	defer func() {
		if r := recover(); r != nil {
			log.Println("Warn: Something wrong. Recovered in ", r)
		}
	}()

	// be aware of ReadBufferSize, WriteBufferSize (default 4096)
	// https://pkg.go.dev/github.com/gorilla/websocket?tab=doc#Upgrader
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Coordinator: [!] WS upgrade:", err)
		return
	}

	// Generate clientID for browserClient
	for {
		clientID = uuid.Must(uuid.NewV4()).String()
		// check duplicate
		if _, ok := o.clients[clientID]; !ok {
			break
		}
	}

	// Create browserClient instance
	client := NewClient(c, clientID, o.cgame.GetSSRC(), o.gameEvents, o.chatEvents)
	o.clients[clientID] = client
	// Add client to chat management
	o.chat.AddClient(clientID, ws.NewClient(client.conn))
	// TODO: Update packet
	// o.broadcast(cloudgame.WSPacket{
	// 	PType: "NUMPLAYER",
	// 	Data:  strconv.Itoa(len(o.clients)),
	// })
	o.chat.SendChatHistory(clientID)
	// Run browser listener first (to capture ping)
	go func(client *Client) {
		client.Listen()
		if client.conn != nil {
			client.conn.Close()
			client.conn = nil
		}
		delete(o.clients, client.clientID)
		close(client.videoStream)
		// Update the remaining
		// o.broadcast(cloudgame.WSPacket{
		// 	PType: "NUMPLAYER",
		// 	Data:  strconv.Itoa(len(o.clients)),
		// })
	}(o.clients[clientID])
}

// Heartbeat maintains connection to server
func (c *Client) Heartbeat() {
	// send heartbeat every 1s
	timer := time.Tick(time.Second)

	for range timer {
		select {
		case <-c.done:
			log.Println("Close heartbeat")
			return
		default:
		}
		// c.Send({PType: "heartbeat"})
	}
}

func (c *Client) Send(packet cloudgame.WSPacket) {
	data, err := json.Marshal(packet)
	if err != nil {
		return
	}

	c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c *Client) Listen() {
	defer func() {
		close(c.done)
	}()

	// Listen from video stream
	go func() {
		for packet := range c.videoStream {
			if c.videoTrack == nil {
				continue
			}
			if writeErr := c.videoTrack.WriteRTP(&packet); writeErr != nil {
				panic(writeErr)
			}
		}
	}()

	log.Println("Client listening")
	for {
		_, rawMsg, err := c.conn.ReadMessage()
		fmt.Println("received", rawMsg)
		if err != nil {
			log.Println("[!] read:", err)
			// TODO: Check explicit disconnect error to break
			break
		}
		wspacket := ws.Packet{}
		err = json.Unmarshal(rawMsg, &wspacket)

		// TODO: Refactor
		if wspacket.PType == "OFFER" {
			c.signal(wspacket.Data)
			// c.Send(cloudgame.WSPacket{
			// 	PType: "Answer
			// })
			continue
		}
		if err != nil {
			log.Println("error decoding", err)
			continue
		}
		if wspacket.PType == "CHAT" {
			c.chatEvents <- textchat.Convert(wspacket)
		} else {
			c.serverEvents <- cloudgame.Convert(wspacket)
		}
	}

}

func readConfig(path string) (cloudgame.Config, error) {
	cfgyml, err := ioutil.ReadFile(path)
	if err != nil {
		return cloudgame.Config{}, err
	}

	cfg := cloudgame.Config{}
	err = yaml.Unmarshal(cfgyml, &cfg)
	return cfg, err
}

func monitor() {
	monitoringServerMux := http.NewServeMux()

	srv := http.Server{
		Addr:    fmt.Sprintf(":%d", 3535),
		Handler: monitoringServerMux,
	}
	log.Println("Starting monitoring server at", srv.Addr)

	pprofPath := fmt.Sprintf("/debug/pprof")
	log.Println("Profiling is enabled at", srv.Addr+pprofPath)
	monitoringServerMux.Handle(pprofPath+"/", http.HandlerFunc(pprof.Index))
	monitoringServerMux.Handle(pprofPath+"/cmdline", http.HandlerFunc(pprof.Cmdline))
	monitoringServerMux.Handle(pprofPath+"/profile", http.HandlerFunc(pprof.Profile))
	monitoringServerMux.Handle(pprofPath+"/symbol", http.HandlerFunc(pprof.Symbol))
	monitoringServerMux.Handle(pprofPath+"/trace", http.HandlerFunc(pprof.Trace))
	// pprof handler for custom pprof path needs to be explicitly specified, according to: https://github.com/gin-contrib/pprof/issues/8 . Don't know why this is not fired as ticket
	// https://golang.org/src/net/http/pprof/pprof.go?s=7411:7461#L305 only render index page
	monitoringServerMux.Handle(pprofPath+"/allocs", pprof.Handler("allocs"))
	monitoringServerMux.Handle(pprofPath+"/block", pprof.Handler("block"))
	monitoringServerMux.Handle(pprofPath+"/goroutine", pprof.Handler("goroutine"))
	monitoringServerMux.Handle(pprofPath+"/heap", pprof.Handler("heap"))
	monitoringServerMux.Handle(pprofPath+"/mutex", pprof.Handler("mutex"))
	monitoringServerMux.Handle(pprofPath+"/threadcreate", pprof.Handler("threadcreate"))
	go srv.ListenAndServe()

}

func main() {
	// HTTP server
	// TODO: Make the communication over websocket
	http.Handle("/assets/", http.StripPrefix("/assets", http.FileServer(http.Dir("./assets"))))
	monitor()
	server := NewServer()
	server.Handle()
	err := server.ListenAndServe()
	if err != nil {
		log.Fatal(err)
	}
}

// Encode encodes the input in base64
// It can optionally zip the input before encoding
func Encode(obj interface{}) string {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}

	return base64.StdEncoding.EncodeToString(b)
}

// Decode decodes the input from base64
// It can optionally unzip the input after decoding
func Decode(in string, obj interface{}) {
	b, err := base64.StdEncoding.DecodeString(in)
	if err != nil {
		panic(err)
	}

	err = json.Unmarshal(b, obj)
	if err != nil {
		panic(err)
	}
}

// streapRTP is based on to https://github.com/pion/webrtc/tree/master/examples/rtp-to-webrtc
// It fetches from a RTP stream produced by FFMPEG and broadcast to all webRTC sessions
func streamRTP(conn *webrtc.PeerConnection, offer webrtc.SessionDescription, ssrc uint32) *webrtc.Track {
	// We make our own mediaEngine so we can place the sender's codecs in it.  This because we must use the
	// dynamic media type from the sender in our answer. This is not required if we are the offerer
	mediaEngine := webrtc.MediaEngine{}
	err := mediaEngine.PopulateFromSDP(offer)
	if err != nil {
		panic(err)
	}

	// Create a video track, using the same SSRC as the incoming RTP Pack)
	videoTrack, err := conn.NewTrack(webrtc.DefaultPayloadTypeVP8, ssrc, "video", "pion")
	if err != nil {
		panic(err)
	}
	if _, err = conn.AddTrack(videoTrack); err != nil {
		panic(err)
	}
	log.Println("video track", videoTrack)

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	conn.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		log.Printf("Connection State has changed %s \n", connectionState.String())
	})

	// Set the remote SessionDescription
	if err = conn.SetRemoteDescription(offer); err != nil {
		panic(err)
	}
	log.Println("Done creating videotrack")

	return videoTrack
}

func (c *Client) signal(offerString string) {
	log.Println("Signalling")
	RTCConn, err := webrtc.NewPeerConnection(webrtcconfig)
	if err != nil {
		log.Println("error ", err)
	}

	offer := webrtc.SessionDescription{}
	Decode(offerString, &offer)

	err = RTCConn.SetRemoteDescription(offer)
	if err != nil {
		log.Println("Set remote description from peer failed", err)
		return
	}

	log.Println("Get SSRC", c.ssrc)
	videoTrack := streamRTP(RTCConn, offer, c.ssrc)

	var answer webrtc.SessionDescription
	answer, err = RTCConn.CreateAnswer(nil)
	if err != nil {
		log.Println("Create Answer Failed", err)
		return
	}

	err = RTCConn.SetLocalDescription(answer)
	if err != nil {
		log.Println("Set Local Description Failed", err)
		return
	}

	isStarted = true
	log.Println("Sending answer", answer)
	c.Send(cloudgame.WSPacket{
		PType: "ANSWER",
		Data:  Encode(answer),
	})
	c.videoTrack = videoTrack
}
