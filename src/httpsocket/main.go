package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
)

var (
	listenAddr                          = flag.String("listen", ":6066", "host:port to listen on")
	defaultHost                         = flag.String("default-host", "", "if not empty, requests without specified host will be proxied to this host")
	defaultTimeout                      = flag.Int("timeout-seconds", 60, "timeout for proxied HTTP requests, in seconds")
	upstreamHostWhitelist               = flag.String("upstream-host-whitelist", "", "comma-separated list of allowed upstream hosts")
	originWhitelist                     = flag.String("origin-whitelist", "", "comma-separated list of allowed origin hosts (suffixes)")
	fakeUpstreamResponseTimeMs          = flag.Int("fake-upstream-response-time-ms", 0, "if greater than 0, instead of actually proxying requests, sleep for specified duration in milliseconds before returning a 502 Bad Gateway response")
	throttleRpsPerClient                = flag.Int("throttle-rps-per-client", 50, "if greater than 0, RPS per client will be limited to specified number (by blocking for the remainder of current second once the limit is reached)")
	throttleConcurrentRequestsPerClient = flag.Int("throttle-concurrent-requests-per-client", 10, "if greater than 0, number of concurrent (in-flight) requests per client will be limited to specified number (by blocking for the remainder of current second once the limit is reached)")
	logConnections                      = flag.Bool("log-connections", false, "log connection opening/closing")
	logClientIoErrors                   = flag.Bool("log-client-io-errors", false, "log input/output errors on client sockets")
	debug                               = flag.Bool("debug", false, "enable more detailed logging")
)

// Оборачиваем хендлер-функцию в стандартные миддлвари
func httpHandleFunc(url string, handler func(http.ResponseWriter, *http.Request)) {
	handler = panicCatcherMiddleware(handler)

	http.HandleFunc(url, handler)
}

// Перехват паник и вывод трейсбеков
func panicCatcherMiddleware(next func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(rw http.ResponseWriter, r *http.Request) {
		defer func() {
			if x := recover(); x != nil {
				stack := GetTraceback()
				errinfo := fmt.Sprintf("ERROR: PANIC: %s\n%s", x, stack)
				log.Printf("%s", errinfo)
				http.Error(rw, errinfo, 500)
			}
		}()
		next(rw, r)
	}
}

func main() {
	flag.Parse()

	initHttpClient(*defaultTimeout)

	proxy := &WsProxy{
		params: ProxyParams{
			DefaultHost:              *defaultHost,
			WhitelistedUpstreamHosts: []string{},
			WhitelistedOrigins:       []string{},
		},
	}
	if *upstreamHostWhitelist != "" {
		proxy.params.WhitelistedUpstreamHosts = strings.Split(*upstreamHostWhitelist, ",")
	}
	if *originWhitelist != "" {
		proxy.params.WhitelistedOrigins = strings.Split(*originWhitelist, ",")
	}

	httpHandleFunc("/", handleFrontpage)
	httpHandleFunc("/ws", proxy.ServeWebsocket)
	httpHandleFunc("/jsonrpc", proxy.ServeHttp)

	go globalStatCounter.TickingLoop()

	log.Printf("Listening on %s...", *listenAddr)
	log.Fatal(http.ListenAndServe(*listenAddr, nil))
}
