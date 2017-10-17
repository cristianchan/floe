package server

import (
	"fmt"
	"net"
	"net/http"

	"golang.org/x/net/websocket"

	"github.com/julienschmidt/httprouter"

	"github.com/floeit/floe/hub"
	"github.com/floeit/floe/log"
	"github.com/floeit/floe/server/push"
)

const rootPath = "/build/api"

// LaunchWeb sets up all the http routes runs the server and launches the trigger flows
// rp is the root path. Returns the address it binds to.
func LaunchWeb(host, rp string, hub *hub.Hub, addrChan chan string) {
	if rp == "" {
		rp = rootPath
	}
	r := httprouter.New()
	r.HandleMethodNotAllowed = false
	r.NotFound = notFoundHandler{}
	r.PanicHandler = panicHandler

	h := handler{hub: hub}

	// --- authentication ---
	r.POST(rp+"/login", h.mw(loginHandler, false))
	r.POST(rp+"/logout", h.mw(logoutHandler, true))

	// --- api ---
	r.GET(rp+"/config", h.mw(confHandler, true)) // return host config and what it knows about other hosts
	r.GET(rp+"/flows", h.mw(hndAllFlows, true))  // list all the flows configs
	r.GET(rp+"/runs/active", h.mw(hndActiveRuns, true))
	r.GET(rp+"/runs/pending", h.mw(hndPendingRuns, true))
	r.GET(rp+"/runs/archive", h.mw(hndArchiveRuns, true))

	// --- p2p api ---
	r.POST(rp+"/flows/exec", h.mw(hndExecFlow, true)) // internal api to pass a pending todo to activate it on this host

	// --- push endpoints ---
	h.setupPushes(rp+"/push/", r, hub)

	// --- static files for the spa ---
	r.ServeFiles("/static/css/*filepath", http.Dir("webapp/css"))
	r.ServeFiles("/static/img/*filepath", http.Dir("webapp/img"))
	r.ServeFiles("/static/js/*filepath", http.Dir("webapp/js"))
	r.GET("/app/*filepath", singleFile("webapp/index.html"))
	r.GET("/favicon.ico", singleFile("webapp/favicon.ico"))

	// ws endpoint
	r.GET("/ws", getWsHandler())

	// --- CORS ---
	r.OPTIONS(rp+"/*all", h.mw(nil, false)) // catch all options

	/*
		r.GET(rp+"/flows/:flid", h.mw(floeHandler, true))
		r.POST(rp+"/flows/:flid/exec", h.mw(execHandler, true))
		r.POST(rp+"/flows/:flid/stop", h.mw(stopHandler, true))
		r.GET(rp+"/flows/:flid/run/:agentid/:runid", h.mw(runHandler, true)) // get the current progress of a run for an agent and run

		// --- web socket connection ---
		r.GET(rp+"/msg", wsHandler)



		// --- the web page stuff ---
		r.GET("/build/", indexHandler)
		r.ServeFiles("/build/css/*filepath", http.Dir("public/build/css"))
		r.ServeFiles("/build/fonts/*filepath", http.Dir("public/build/fonts"))
		r.ServeFiles("/build/img/*filepath", http.Dir("public/build/img"))
		r.ServeFiles("/build/js/*filepath", http.Dir("public/build/js"))

	*/
	log.Debug("attempting to listen on:", host)

	listener, err := net.Listen("tcp", host)
	if err != nil {
		log.Fatal(err)
	}
	address := listener.Addr().(*net.TCPAddr).String()

	// in separate go routine message the passed in chan with the server address
	if addrChan != nil {
		go func() {
			addrChan <- address
		}()
	}

	log.Debug("agent server starting on:", address)

	log.Fatal(http.Serve(listener, r))
}

func getWsHandler() httprouter.Handle {
	return func(rw http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		h := websocket.Handler(wsHandler)
		h.ServeHTTP(rw, r)
	}
}

func wsHandler(ws *websocket.Conn) {
	for {
		msg := make([]byte, 512)
		n, err := ws.Read(msg)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Receive: %s\n", msg[:n])

		m, err := ws.Write(msg[:n])
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Send: %s\n", msg[:m])
	}
}

func singleFile(path string) httprouter.Handle {
	return func(rw http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		http.ServeFile(rw, r, path)
	}
}

// pushes is the map of all trigger types that can be triggered via the trigger endpoints.
// This map will be used to attach these pushes types to the http server.
// The key here will be used as the sub path to route to this trigger.
var pushes = map[string]push.Push{
	"data": push.Data{},
}
