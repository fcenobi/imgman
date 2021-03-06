package main

import (
	"fmt"
	"log"
	"net/http"

	"golang.org/x/net/websocket"
)

var (
	sockPath  = "/sock"
	codecJSON = websocket.JSON // codec for JSON
	//Message  = websocket.Message // codec for string, []byte
	//activeClients = make(map[clientConn]int) // map containing clients
	activeClients = make(map[string]clientConn) // by ip address
)

type clientConn struct {
	websocket *websocket.Conn
	clientIP  string
}

type clientMsg struct {
	Msg string
}

type socketHandler func(ws *websocket.Conn, data clientMsg)

func sockEcho(msg event) {
	fmt.Println("ECHO!", msg)
	for ip, cs := range activeClients {
		if err := codecJSON.Send(cs.websocket, msg); err != nil {
			// we could not send the message to a peer
			log.Println("Could not send message to ", ip, err.Error())
		}
	}
}

func socketDump(ws *websocket.Conn, data clientMsg) {
	log.Println("SOCK DATA:", data)
}

func sockWrapper(ws *websocket.Conn) {
	//sockListener(ws, sockEcho)
	sockListener(ws, socketDump)
}

func init() {
	//http.Handle(sockPath, websocket.Handler(sockServer))
	//http.Handle(sockPath, websocket.Handler(sockListener))
	http.Handle(sockPath, websocket.Handler(sockWrapper))
}

func sockListener(ws *websocket.Conn, fn socketHandler) {
	var err error
	var clientData clientMsg

	// cleanup on server side
	defer func() {
		if err = ws.Close(); err != nil {
			log.Println("Websocket could not be closed", err.Error())
		}
	}()

	client := ws.Request().RemoteAddr
	log.Println("Client connected:", client)
	sockCli := clientConn{ws, client}
	activeClients[client] = sockCli
	log.Println("Number of clients connected ...", len(activeClients))

	// for loop so the websocket stays open otherwise
	// it'll close after one Receieve and Send
	for {
		if err = codecJSON.Receive(ws, &clientData); err != nil {
			// If we cannot Read then the connection is closed
			log.Println("Websocket Disconnected waiting", err.Error())
			// remove the ws client conn from our active clients
			client := ws.Request().RemoteAddr
			delete(activeClients, client)
			//delete(activeClients, sockCli)
			log.Println("Number of clients still connected ...", len(activeClients))
			return
		}
		fn(ws, clientData)
	}
}
