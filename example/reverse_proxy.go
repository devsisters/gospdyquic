package main

import (
	"flag"
	"fmt"
	"log"
	"net/http/httputil"
	"net/url"
	"os"

	"github.com/devsisters/goquic"
	"github.com/devsisters/gospdyquic"
)

var numOfServers int
var port int
var logLevel int

func init() {
	flag.IntVar(&numOfServers, "n", 1, "Number of concurrent quic dispatchers")
	flag.IntVar(&port, "port", 8080, "TCP/UDP port number to listen")
	flag.IntVar(&logLevel, "loglevel", -1, "Log level")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s backend_url\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}
}

func main() {
	goquic.Initialize()
	goquic.SetLogLevel(logLevel)

	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		return
	}
	proxyUrl := flag.Arg(0)

	log.Printf("About to listen on %d. Go to https://127.0.0.1:%d/", port, port)
	portStr := fmt.Sprintf(":%d", port)

	parsedUrl, err := url.Parse(proxyUrl)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Starting reverse proxy for backend URL: %v", parsedUrl)

	err = gospdyquic.ListenAndServeSecure(portStr, "cert.pem", "key.pem", numOfServers, httputil.NewSingleHostReverseProxy(parsedUrl))
	if err != nil {
		log.Fatal(err)
	}
}
