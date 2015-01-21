package main

import (
	"bytes"
	"errors"
	"fmt"
	"goquic"
	"io"
	"net"
	"net/http"
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

	headers := make(http.Header)
	bounds := MAX_FRAME_SIZE - 12 // Maximum frame size minus maximum non-headers data (SYN_STREAM)
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
			headers.Add(string(name), string(value))
		}
	}

	return headers, nil
}

/* ----------- End SPDY Parsing (from SlyMarbo/spdy ) --------- */

// USERSPACE -----------------------------------------------------------------

type SpdyStream struct {
	stream_id     uint32
	headers       http.Header
	header_parsed bool
	buffer        bytes.Buffer
}

func (stream *SpdyStream) ProcessData(newBytes []byte) int {
	fmt.Println("========================================================== ProcessData:", newBytes)

	stream.buffer.Write(newBytes)

	if !stream.header_parsed {
		// We don't want to consume the buffer *yet*, so create a new reader
		reader := bytes.NewReader(stream.buffer.Bytes())
		headers, err := ParseHeaders(reader)
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
		stream.headers = headers
		fmt.Println("headers:", stream.headers)
	}

	// Process body
	fmt.Println("stream.buffer:", stream.buffer.Bytes())
	fmt.Println("stream.buffer len:", stream.buffer.Len())
	fmt.Println("stream.buffer as string:", string(stream.buffer.Bytes()[:]))

	return len(newBytes)
}

func (stream *SpdyStream) OnFinRead() {
	fmt.Println("========================================================== OnFinRead")

	if !stream.header_parsed {
		// TODO(serialx): Send error message
	}

	fmt.Println("headers:", stream.headers)
	fmt.Println("stream.buffer:", stream.buffer.Bytes())
	fmt.Println("stream.buffer len:", stream.buffer.Len())
	fmt.Println("stream.buffer as string:", string(stream.buffer.Bytes()[:]))
}

type SpdySession struct {
}

func (s *SpdySession) CreateIncomingDataStream(stream_id uint32) goquic.DataStreamProcessor {
	stream := &SpdyStream{
		stream_id:     stream_id,
		header_parsed: false,
	}
	return stream
}

func CreateSpdySession() goquic.DataStreamCreator {
	return &SpdySession{}
}

func main() {
	fmt.Printf("hello, world\n")
	goquic.Initialize()
	goquic.SetLogLevel(-1)
	//C.test_quic()

	buf := make([]byte, 65535)
	listen_addr := net.UDPAddr{
		Port: 8080,
		IP:   net.ParseIP("0.0.0.0"),
	}
	conn, err := net.ListenUDP("udp4", &listen_addr)
	if err != nil {
		panic(err)
	}
	dispatcher := goquic.CreateQuicDispatcher(conn, CreateSpdySession)
	for {
		n, peer_addr, err := conn.ReadFromUDP(buf)

		if err != nil {
			panic(err)
		}
		dispatcher.ProcessPacket(&listen_addr, peer_addr, buf[:n])
	}
}
