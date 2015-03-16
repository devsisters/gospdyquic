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
var cert string
var key string

func init() {
	flag.IntVar(&numOfServers, "n", 1, "Number of concurrent quic dispatchers")
	flag.IntVar(&port, "port", 8080, "TCP/UDP port number to listen")
	flag.IntVar(&logLevel, "loglevel", -1, "Log level")
	flag.StringVar(&cert, "cert", "", "Certificate file (PEM), will use encrypted QUIC and SSL when provided")
	flag.StringVar(&key, "key", "", "Private key file (PEM), will use encrypted QUIC and SSL when provided")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s backend_url\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}
}

func main() {
	goquic.SetLogLevel(logLevel)

	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		return
	}

	useEncryption := false
	if len(cert) > 0 && len(key) > 0 {
		useEncryption = true
	}
	proxyUrl := flag.Arg(0)

	scheme := "http"
	if useEncryption {
		scheme = "https"
	}
	log.Printf("About to listen on %d. Go to %s://127.0.0.1:%d/", port, scheme, port)
	portStr := fmt.Sprintf(":%d", port)

	parsedUrl, err := url.Parse(proxyUrl)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Starting reverse proxy for backend URL: %v", parsedUrl)

	if useEncryption {
		err = gospdyquic.ListenAndServeSecure(portStr, cert, key, numOfServers, httputil.NewSingleHostReverseProxy(parsedUrl))
	} else {
		err = gospdyquic.ListenAndServe(portStr, numOfServers, httputil.NewSingleHostReverseProxy(parsedUrl))
	}
	if err != nil {
		log.Fatal(err)
	}
}
