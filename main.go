package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"goquic"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

/* ------------ SPDY Parsing (from SlyMarbo/spdy ) ----------- */

// ReadExactly is used to ensure that the given number of bytes
// are read if possible, even if multiple calls to Read
// are required.
func ReadExactly(r io.Reader, i int) ([]byte, error) {
	out := make([]byte, i)
	in := out[:]
	for i > 0 {
		if r == nil {
			return nil, errors.New("Error: Connection is nil.")
		}
		if n, err := r.Read(in); err != nil {
			return nil, err
		} else {
			in = in[n:]
			i -= n
		}
	}
	return out, nil
}

func BytesToUint32(b []byte) uint32 {
	return (uint32(b[0]) << 24) + (uint32(b[1]) << 16) + (uint32(b[2]) << 8) + uint32(b[3])
}

func ParseHeaders(reader io.Reader) (http.Header, error) {
	// Maximum frame size (2 ** 24 -1).
	const MAX_FRAME_SIZE = 0xffffff

	// SPDY/3 uses 32-bit fields.
	size := 4
	bytesToInt := func(b []byte) int {
		return int(BytesToUint32(b))
	}

	// Read in the number of name/value pairs.
	pairs, err := ReadExactly(reader, size)
	if err != nil {
		return nil, err
	}
	numNameValuePairs := bytesToInt(pairs)

	header := make(http.Header)
	bounds := MAX_FRAME_SIZE - 12 // Maximum frame size minus maximum non-header data (SYN_STREAM)
	for i := 0; i < numNameValuePairs; i++ {
		var nameLength, valueLength int

		// Get the name's length.
		length, err := ReadExactly(reader, size)
		if err != nil {
			return nil, err
		}
		nameLength = bytesToInt(length)
		bounds -= size

		if nameLength > bounds {
			fmt.Printf("Error: Maximum header length is %d. Received name length %d.\n", bounds, nameLength)
			return nil, errors.New("Error: Incorrect header name length.")
		}
		bounds -= nameLength

		// Get the name.
		name, err := ReadExactly(reader, nameLength)
		if err != nil {
			return nil, err
		}

		// Get the value's length.
		length, err = ReadExactly(reader, size)
		if err != nil {
			return nil, err
		}
		valueLength = bytesToInt(length)
		bounds -= size

		if valueLength > bounds {
			fmt.Printf("Error: Maximum header length is %d. Received values length %d.\n", bounds, valueLength)
			return nil, errors.New("Error: Incorrect header values length.")
		}
		bounds -= valueLength

		// Get the values.
		values, err := ReadExactly(reader, valueLength)
		if err != nil {
			return nil, err
		}

		// Split the value on null boundaries.
		for _, value := range bytes.Split(values, []byte{'\x00'}) {
			header.Add(string(name), string(value))
		}
	}

	return header, nil
}

/* ----------- End SPDY Parsing (from SlyMarbo/spdy ) --------- */

// USERSPACE -----------------------------------------------------------------

type SpdyStream struct {
	stream_id     uint32
	header        http.Header
	header_parsed bool
	buffer        bytes.Buffer
	server        *QuicSpdyServer
}

type spdyResponseWriter struct {
	serverStream *goquic.QuicSpdyServerStream
	header       http.Header
	wroteHeader  bool
}

type udpData struct {
	n    int
	addr *net.UDPAddr
	buf  []byte
}

func (w *spdyResponseWriter) Header() http.Header {
	return w.header
}

func (w *spdyResponseWriter) Write(buffer []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}

	w.serverStream.WriteOrBufferData(buffer, false)
	return len(buffer), nil
}

func (w *spdyResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.header.Set(":status", fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode))) // TODO(serialx): Do a proper status code return
	w.header.Set(":version", "HTTP/1.1")

	w.serverStream.WriteHeader(w.header, false)
	w.wroteHeader = true
}

func (stream *SpdyStream) ProcessData(serverStream *goquic.QuicSpdyServerStream, newBytes []byte) int {
	stream.buffer.Write(newBytes)

	if !stream.header_parsed {
		// We don't want to consume the buffer *yet*, so create a new reader
		reader := bytes.NewReader(stream.buffer.Bytes())
		header, err := ParseHeaders(reader)
		if err != nil {
			// Header parsing unsuccessful, maybe header is not yet completely received
			// Append it to the buffer for parsing later
			return int(len(newBytes))
		}

		// Header parsing successful
		n, _ := reader.Seek(0, 1)
		// Consume the buffer, the rest of the buffer is the body
		stream.buffer.Next(int(n))

		stream.header_parsed = true
		stream.header = header

		// TODO(serialx): Parsing header should also exist on OnFinRead
	}
	// Process body
	return len(newBytes)
}

func (stream *SpdyStream) OnFinRead(serverStream *goquic.QuicSpdyServerStream) {
	if !stream.header_parsed {
		// TODO(serialx): Send error message
	}

	header := stream.header
	req := new(http.Request)
	req.Method = header.Get(":method")
	req.RequestURI = header.Get(":path")
	req.Proto = header.Get(":version")
	req.Header = header
	req.Host = header.Get(":host")
	// req.RemoteAddr = serverStream. TODO(serialx): Add remote addr
	rawPath := header.Get(":path")

	url, err := url.ParseRequestURI(rawPath)
	if err != nil {
		return
		// TODO(serialx): Send error message
	}

	url.Scheme = header.Get(":scheme")
	url.Host = header.Get(":host")
	req.URL = url

	w := &spdyResponseWriter{
		serverStream: serverStream,
		header:       make(http.Header),
	}
	if stream.server.Handler != nil {
		stream.server.Handler.ServeHTTP(w, req)
	} else {
		http.DefaultServeMux.ServeHTTP(w, req)
	}

	serverStream.WriteOrBufferData(make([]byte, 0), true)
}

type SpdySession struct {
	server *QuicSpdyServer
}

func (s *SpdySession) CreateIncomingDataStream(stream_id uint32) goquic.DataStreamProcessor {
	stream := &SpdyStream{
		stream_id:     stream_id,
		header_parsed: false,
		server:        s.server,
	}
	return stream
}

type QuicSpdyServer struct {
	Addr           string
	Handler        http.Handler
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	MaxHeaderBytes int
	//TLSConfig *tls.Config
}

var numOfServers int
var port int
var serveRoot string
var logLevel int
var proxyUrl string

// TODO(hodduc) ListenAndServe() and Serve() should be moved to goquic side
func (srv *QuicSpdyServer) ListenAndServe() error {
	addr := srv.Addr
	if addr == "" {
		addr = ":http"
	}

	goquic.Initialize()
	goquic.SetLogLevel(logLevel)

	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	udp_conn, ok := conn.(*net.UDPConn)
	if !ok {
		return errors.New("ListenPacket did not return net.UDPConn")
	}

	chanArray := make([](chan udpData), numOfServers)

	for i := 0; i < numOfServers; i++ {
		channel := make(chan udpData)
		go srv.Serve(udp_conn, channel)
		chanArray[i] = channel
	}

	buf := make([]byte, 65535)

	for {
		n, peer_addr, err := udp_conn.ReadFromUDP(buf)
		if err != nil {
			// TODO(serialx): Don't panic and keep calm...
			panic(err)
		}

		var connId uint64

		switch buf[0] & 0xC {
		case 0xC:
			connId = binary.LittleEndian.Uint64(buf[1:9])
		case 0x8:
			connId = binary.LittleEndian.Uint64(buf[1:5])
		case 0x4:
			connId = binary.LittleEndian.Uint64(buf[1:2])
		default:
			connId = 0
		}

		//		fmt.Println("ConnId", connId, " ---- > ", connId%numOfServers)

		buf_new := make([]byte, len(buf))
		copy(buf_new, buf)

		chanArray[connId%uint64(numOfServers)] <- udpData{n: n, addr: peer_addr, buf: buf_new}
		// TODO(hodduc): Minimize heap uses of buf
		// TODO(hodduc): Smart Load Balancing
	}
}

func (srv *QuicSpdyServer) Serve(conn *net.UDPConn, readChan chan udpData) error {
	defer conn.Close()

	listen_addr, err := net.ResolveUDPAddr("udp", conn.LocalAddr().String())
	if err != nil {
		// TODO(serialx): Don't panic and keep calm...
		panic(err)
	}
	createSpdySession := func() goquic.DataStreamCreator {
		return &SpdySession{server: srv}
	}

	alarmChan := make(chan *goquic.GoQuicAlarm)
	writeChan := make(chan *goquic.WriteCallback)

	dispatcher := goquic.CreateQuicDispatcher(conn, createSpdySession, &goquic.TaskRunner{AlarmChan: alarmChan, WriteChan: writeChan})

	for {
		select {
		case result, ok := <-readChan:
			if !ok {
				break
			}
			dispatcher.ProcessPacket(listen_addr, result.addr, result.buf[:result.n])
		case alarm, ok := <-alarmChan:
			if !ok {
				break
			}
			alarm.OnAlarm()
		case write, ok := <-writeChan:
			if !ok {
				break
			}
			write.Callback()
		}
	}
}

func ListenAndServeQuicSpdyOnly(addr string, certFile string, keyFile string, handler http.Handler) error {
	server := &QuicSpdyServer{Addr: addr, Handler: handler}
	return server.ListenAndServe()
}

func httpHandler(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/zip")
	for i := 0; i < 5; i++ {
		w.Write(make([]byte, 1000000))
	}
	//w.Write([]byte("This is an example server.\n"))
}

func init() {
	flag.IntVar(&numOfServers, "n", 1, "num of servers")
	flag.IntVar(&port, "port", 8080, "UDP port number to listen")
	flag.StringVar(&serveRoot, "root", "/tmp", "Root of path to serve under https://127.0.0.1/files/")
	flag.IntVar(&logLevel, "loglevel", -1, "Log level")
	flag.StringVar(&proxyUrl, "proxyurl", "", "Set some url to use as proxy")
}

func main() {
	flag.Parse()

	log.Printf("About to listen on %d. Go to https://127.0.0.1:%d/", port, port)
	portStr := fmt.Sprintf(":%d", port)

	if len(proxyUrl) == 0 {
		http.HandleFunc("/", httpHandler)
		http.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.Dir(serveRoot))))
		err := ListenAndServeQuicSpdyOnly(portStr, "cert.pem", "key.pem", nil)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		parsedUrl, err := url.Parse(proxyUrl)
		if err != nil {
			log.Fatal(err)
		}

		err = ListenAndServeQuicSpdyOnly(portStr, "cert.pem", "key.pem", httputil.NewSingleHostReverseProxy(parsedUrl))
		if err != nil {
			log.Fatal(err)
		}
	}
}
