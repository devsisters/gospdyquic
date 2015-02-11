package gospdyquic

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/devsisters/goquic"
	"github.com/devsisters/gospdyquic/spdy"
)

type SpdyStream struct {
	stream_id     uint32
	header        http.Header
	header_parsed bool
	buffer        *bytes.Buffer
	server        *QuicSpdyServer
}

type spdyResponseWriter struct {
	serverStream goquic.QuicStream
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

func (stream *SpdyStream) ProcessData(serverStream goquic.QuicStream, newBytes []byte) int {
	stream.buffer.Write(newBytes)

	if !stream.header_parsed {
		// We don't want to consume the buffer *yet*, so create a new reader
		reader := bytes.NewReader(stream.buffer.Bytes())
		header, err := spdy.ParseHeaders(reader)
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

func (stream *SpdyStream) OnFinRead(quicStream goquic.QuicStream) {
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
	// TODO(serialx): To buffered async read
	req.Body = ioutil.NopCloser(stream.buffer)

	w := &spdyResponseWriter{
		serverStream: quicStream,
		header:       make(http.Header),
	}
	if stream.server.Handler != nil {
		stream.server.Handler.ServeHTTP(w, req)
	} else {
		http.DefaultServeMux.ServeHTTP(w, req)
	}

	quicStream.WriteOrBufferData(make([]byte, 0), true)
}

type SpdySession struct {
	server *QuicSpdyServer
}

func (s *SpdySession) CreateIncomingDataStream(stream_id uint32) goquic.DataStreamProcessor {
	stream := &SpdyStream{
		stream_id:     stream_id,
		header_parsed: false,
		server:        s.server,
		buffer:        new(bytes.Buffer),
	}
	return stream
}

func (s *SpdySession) CreateOutgoingDataStream() goquic.DataStreamProcessor {
	// NOT SUPPORTED
	return nil
}

type QuicSpdyServer struct {
	Addr           string
	Handler        http.Handler
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	MaxHeaderBytes int
	numOfServers   int
	//TLSConfig *tls.Config
}

func (srv *QuicSpdyServer) ListenAndServe() error {
	addr := srv.Addr
	if addr == "" {
		addr = ":http"
	}

	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	udp_conn, ok := conn.(*net.UDPConn)
	if !ok {
		return errors.New("ListenPacket did not return net.UDPConn")
	}

	chanArray := make([](chan udpData), srv.numOfServers)

	for i := 0; i < srv.numOfServers; i++ {
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

		chanArray[connId%uint64(srv.numOfServers)] <- udpData{n: n, addr: peer_addr, buf: buf_new}
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

func ListenAndServe(addr string, certFile string, keyFile string, numOfServers int, handler http.Handler) error {
	port := addr[1:]

	if handler == nil {
		handler = http.DefaultServeMux
	}

	altProtoMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("Alternate-Protocol", port+":quic")
			next.ServeHTTP(w, r)
		})
	}

	go http.ListenAndServe(addr, altProtoMiddleware(handler))
	server := &QuicSpdyServer{Addr: addr, Handler: handler, numOfServers: numOfServers}
	return server.ListenAndServe()
}

func ListenAndServeQuicSpdyOnly(addr string, certFile string, keyFile string, numOfServers int, handler http.Handler) error {
	server := &QuicSpdyServer{Addr: addr, Handler: handler, numOfServers: numOfServers}
	return server.ListenAndServe()
}
