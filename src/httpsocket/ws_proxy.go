package main

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Серверный обработчик JSON-RPC поверх Websocket

// Общие параметры прокси
type WsProxy struct {
	params ProxyParams
}

const (
	ReadBufferSizeLimit  = 32768
	WriteBufferSizeLimit = 32768
	MessageSizeLimit     = 1 * 1024 * 1024
	ReadDeadline         = 60 * time.Second
	PingInterval         = 50 * time.Second
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  ReadBufferSizeLimit,
	WriteBufferSize: WriteBufferSizeLimit,
	CheckOrigin: func(r *http.Request) bool {
		return true // проверим origin сами до Upgrader, потому что эта штука некрасиво паникует
	},
}

var (
	globalStatCounter = NewStatCounter(nil)
)

// Проверка, пускать ли клиента к вебсокету, на основе заголовка Origin
func (p *WsProxy) CheckOrigin(r *http.Request) bool {
	if len(p.params.WhitelistedOrigins) == 0 {
		return true
	}
	origin := r.Header.Get("Origin")
	for _, h := range p.params.WhitelistedOrigins {
		if h != "" && strings.HasSuffix(origin, h) {
			return true
		}
	}
	return false
}

// Обработчик Websocket
func (p *WsProxy) ServeWebsocket(w http.ResponseWriter, r *http.Request) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	dieOnError(err)

	globalStatCounter.ConnectionAttempt()

	if !p.CheckOrigin(r) {
		log.Printf("WARN: request from non-whitelisted origin: `%s`", r.Header.Get("Origin"))
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	dieOnError(err)
	defer conn.Close()

	client := &ProxyClient{
		params:          &p.params,
		originalRequest: r,
		xRealIp:         ip,
		conn:            conn,
		statCounter:     NewStatCounter(globalStatCounter),
	}
	if *logConnections {
		client.LogInfof("Connected")
		defer client.LogInfof("Disconnected")
	}
	globalStatCounter.OpenedConnection()
	defer globalStatCounter.ClosedConnection()

	conn.SetReadLimit(MessageSizeLimit)
	conn.SetReadDeadline(time.Now().Add(ReadDeadline))
	conn.SetPongHandler(func(string) error { conn.SetReadDeadline(time.Now().Add(ReadDeadline)); return nil })

	pingTicker := time.NewTicker(PingInterval)
	defer pingTicker.Stop()

	go func() {
		for _ = range pingTicker.C {
			client.writeLock.Lock()
			if err := conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				client.writeLock.Unlock()
				break
			}
			client.writeLock.Unlock()
		}
	}()

	for {
		rq := JsonRpcRequest{}
		err := conn.ReadJSON(&rq)
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				break
			}
			if *logConnections {
				client.LogErrorf("On read: %s", err)
			}
			break
		}
		go client.HandleRpcRequest(&rq)

		conn.SetReadDeadline(time.Now().Add(ReadDeadline))
	}
}

// Обработчик HTTP, для упрощения отладки HTTP-over-JSON-RPC
func (p *WsProxy) ServeHttp(w http.ResponseWriter, r *http.Request) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	dieOnError(err)

	client := &ProxyClient{
		params:          &p.params,
		originalRequest: r,
		xRealIp:         ip,
		conn:            &HttpJsonWriter{w},
	}

	rq := JsonRpcRequest{}
	bs, err := ioutil.ReadAll(r.Body)
	dieOnError(err)
	err = json.Unmarshal(bs, &rq)
	dieOnError(err)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	client.HandleRpcRequest(&rq)
}

// Обертка над http.ResponseWriter для реализации интерфейса JsonWriter
type HttpJsonWriter struct {
	rw http.ResponseWriter
}

func (w *HttpJsonWriter) WriteJSON(v interface{}) error {
	bs, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.rw.Write(bs)
	return err
}
