package main

import (
	"bytes"
	"errors"
	"fmt"
	"goquic"
	"io"
	"log"
	"net"
	"net/http"
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

func (w *spdyResponseWriter) Header() http.Header {
	return w.header
}

func (w *spdyResponseWriter) Write(buffer []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}

	w.serverStream.WriteOrBufferData(buffer)
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
	fmt.Println("========================================================== ProcessData:", newBytes)

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
		fmt.Println("header:", stream.header)

		// TODO(serialx): Parsing header should also exist on OnFinRead
	}

	// Process body
	fmt.Println("stream.buffer:", stream.buffer.Bytes())
	fmt.Println("stream.buffer len:", stream.buffer.Len())
	fmt.Println("stream.buffer as string:", string(stream.buffer.Bytes()[:]))

	return len(newBytes)
}

func (stream *SpdyStream) OnFinRead(serverStream *goquic.QuicSpdyServerStream) {
	fmt.Println("========================================================== OnFinRead")

	if !stream.header_parsed {
		// TODO(serialx): Send error message
	}

	fmt.Println("header:", stream.header)
	fmt.Println("stream.buffer:", stream.buffer.Bytes())
	fmt.Println("stream.buffer len:", stream.buffer.Len())
	fmt.Println("stream.buffer as string:", string(stream.buffer.Bytes()[:]))

	//var fake_headers map[string]string = make(map[string]string)
	//// headergenerate
	//fake_headers[":status"] = "200 OK"
	//fake_headers[":version"] = "HTTP/1.1"
	//fake_headers["content-type"] = "text/html"

	////
	//respBody := fmt.Sprintln("<html><body><h1>Hello, World!</h1><pre>", stream.headers, "</pre></body></html>")
	//serverStream.WriteHeader(fake_headers, false)
	//serverStream.WriteOrBufferData([]byte(respBody))

	//rawUrl := header.Get(":scheme") + "://" + header.Get(":host") + header.Get(":path")
	header := stream.header
	req := new(http.Request)
	req.Method = header.Get(":method")
	req.RequestURI = header.Get(":path")
	req.Proto = header.Get(":version")
	req.Header = header
	req.Host = header.Get(":host")
	// req.RemoteAddr = serverStream. TODO(serialx): Add remote addr
	req.URL = &url.URL{
		Scheme: header.Get(":scheme"),
		Host:   header.Get(":host"),
		Path:   header.Get(":path"),
	}

	w := &spdyResponseWriter{
		serverStream: serverStream,
		header:       make(http.Header),
	}
	if stream.server.Handler != nil {
		stream.server.Handler.ServeHTTP(w, req)
	} else {
		http.DefaultServeMux.ServeHTTP(w, req)
	}
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

// TODO(hodduc) ListenAndServe() and Serve() should be moved to goquic side
func (srv *QuicSpdyServer) ListenAndServe() error {
	addr := srv.Addr
	if addr == "" {
		addr = ":http"
	}

	goquic.Initialize()
	goquic.SetLogLevel(-1)

	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	udp_conn, ok := conn.(*net.UDPConn)
	if !ok {
		return errors.New("ListenPacket did not return net.UDPConn")
	}

	return srv.Serve(udp_conn)
}

func (srv *QuicSpdyServer) Serve(conn *net.UDPConn) error {
	defer conn.Close()

	listen_addr, err := net.ResolveUDPAddr("udp", conn.LocalAddr().String())
	if err != nil {
		// TODO(serialx): Don't panic and keep calm...
		panic(err)
	}
	createSpdySession := func() goquic.DataStreamCreator {
		return &SpdySession{server: srv}
	}

	type UDPData struct {
		n    int
		addr *net.UDPAddr
	}

	readChan := make(chan UDPData)
	alarmChan := make(chan *goquic.GoQuicAlarm)
	buf := make([]byte, 65536)

	dispatcher := goquic.CreateQuicDispatcher(conn, createSpdySession, &goquic.TaskRunner{AlarmChan: alarmChan})

	go func(readChan chan UDPData, buf []byte) {
		for {
			n, peer_addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				// TODO(serialx): Don't panic and keep calm...
				panic(err)
			}
			readChan <- UDPData{n, peer_addr}
		}
	}(readChan, buf)

	for {
		select {
		case result, ok := <-readChan:
			fmt.Println(" ********** Triggered ProcessPacket")
			if !ok {
				break
			}
			dispatcher.ProcessPacket(listen_addr, result.addr, buf[:result.n])
			fmt.Println(" ********** End ProcessPacket")
		case alarm, ok := <-alarmChan:
			fmt.Println(" ********** Triggered OnAlarm")
			if !ok {
				break
			}
			alarm.OnAlarm()
			fmt.Println(" ********** End OnAlarm")
		}
	}
}

func ListenAndServeQuicSpdyOnly(addr string, certFile string, keyFile string, handler http.Handler) error {
	server := &QuicSpdyServer{Addr: addr, Handler: handler}
	return server.ListenAndServe()
}

func httpHandler(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("This is an example server.\n"))
}

func main() {
	http.HandleFunc("/", httpHandler)
	http.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.Dir("/home/serialx/"))))
	log.Printf("About to listen on 10443. Go to https://127.0.0.1:10443/")
	err := ListenAndServeQuicSpdyOnly(":8080", "cert.pem", "key.pem", nil)
	if err != nil {
		log.Fatal(err)
	}
}
