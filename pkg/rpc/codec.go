package rpc

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	stderr "errors"
	"io"
	"net/rpc"
	"sync"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/goridge/v3/pkg/frame"
	"github.com/roadrunner-server/goridge/v3/pkg/relay"
	"github.com/roadrunner-server/goridge/v3/pkg/socket"
	"github.com/vmihailenco/msgpack/v5"
	"google.golang.org/protobuf/proto"
)

// Codec represent net/rpc bridge over Goridge socket relay.
type Codec struct {
	relay  relay.Relay
	closed bool
	frame  *frame.Frame
	codec  sync.Map

	bPool sync.Pool
	fPool sync.Pool
}

// NewCodec initiates new server rpc codec over socket connection.
func NewCodec(rwc io.ReadWriteCloser) *Codec {
	return &Codec{
		relay: socket.NewSocketRelay(rwc),
		codec: sync.Map{},

		bPool: sync.Pool{New: func() any {
			return new(bytes.Buffer)
		}},

		fPool: sync.Pool{New: func() any {
			return frame.NewFrame()
		}},
	}
}

// NewCodecWithRelay initiates new server rpc codec with a relay of choice.
func NewCodecWithRelay(relay relay.Relay) *Codec {
	return &Codec{relay: relay}
}

func (c *Codec) get() *bytes.Buffer {
	return c.bPool.Get().(*bytes.Buffer)
}

func (c *Codec) put(b *bytes.Buffer) {
	b.Reset()
	c.bPool.Put(b)
}

func (c *Codec) getFrame() *frame.Frame {
	return c.fPool.Get().(*frame.Frame)
}

func (c *Codec) putFrame(f *frame.Frame) {
	f.Reset()
	c.fPool.Put(f)
}

// WriteResponse marshals response, byte slice or error to remote.
func (c *Codec) WriteResponse(r *rpc.Response, body any) error { //nolint:funlen
	const op = errors.Op("goridge_write_response")
	fr := c.getFrame()
	defer c.putFrame(fr)

	// SEQ_ID + METHOD_NAME_LEN
	fr.WriteOptions(fr.HeaderPtr(), uint32(r.Seq), uint32(len(r.ServiceMethod))) //nolint:gosec
	// Write protocol version
	fr.WriteVersion(fr.Header(), frame.Version1)

	// load and delete associated codec to not waste memory
	// because we write it to the fr and don't need more information about it
	codec, ok := c.codec.LoadAndDelete(r.Seq)
	if !ok {
		// fallback codec
		fr.WriteFlags(fr.Header(), frame.CodecGob)
	} else {
		fr.WriteFlags(fr.Header(), codec.(byte))
	}

	// if error returned, we sending it via relay and return error from WriteResponse
	if r.Error != "" {
		// Append error flag
		return c.handleError(r, fr, r.Error)
	}

	switch {
	case codec.(byte)&frame.CodecProto != 0:
		d, err := proto.Marshal(body.(proto.Message))
		if err != nil {
			return c.handleError(r, fr, err.Error())
		}

		// initialize buffer
		buf := c.get()
		defer c.put(buf)

		buf.Grow(len(d) + len(r.ServiceMethod))
		// writeServiceMethod to the buffer
		buf.WriteString(r.ServiceMethod)
		buf.Write(d)

		fr.WritePayloadLen(fr.Header(), uint32(buf.Len())) //nolint:gosec
		// copy inside
		fr.WritePayload(buf.Bytes())
		fr.WriteCRC(fr.Header())
		// send buffer
		return c.relay.Send(fr)
	case codec.(byte)&frame.CodecRaw != 0:
		// initialize buffer
		buf := c.get()
		defer c.put(buf)

		switch data := body.(type) {
		case []byte:
			buf.Grow(len(data) + len(r.ServiceMethod))
			// writeServiceMethod to the buffer
			buf.WriteString(r.ServiceMethod)
			buf.Write(data)

			fr.WritePayloadLen(fr.Header(), uint32(buf.Len())) //nolint:gosec
			fr.WritePayload(buf.Bytes())
		case *[]byte:
			buf.Grow(len(*data) + len(r.ServiceMethod))
			// writeServiceMethod to the buffer
			buf.WriteString(r.ServiceMethod)
			buf.Write(*data)

			fr.WritePayloadLen(fr.Header(), uint32(buf.Len())) //nolint:gosec
			fr.WritePayload(buf.Bytes())
		default:
			return c.handleError(r, fr, "unknown Raw payload type")
		}

		// send buffer
		fr.WriteCRC(fr.Header())
		return c.relay.Send(fr)

	case codec.(byte)&frame.CodecJSON != 0:
		data, err := json.Marshal(body)
		if err != nil {
			return c.handleError(r, fr, err.Error())
		}

		// initialize buffer
		buf := c.get()
		defer c.put(buf)

		buf.Grow(len(data) + len(r.ServiceMethod))
		// writeServiceMethod to the buffer
		buf.WriteString(r.ServiceMethod)
		buf.Write(data)

		fr.WritePayloadLen(fr.Header(), uint32(buf.Len())) //nolint:gosec
		// copy inside
		fr.WritePayload(buf.Bytes())
		fr.WriteCRC(fr.Header())
		// send buffer
		return c.relay.Send(fr)

	case codec.(byte)&frame.CodecMsgpack != 0:
		b, err := msgpack.Marshal(body)
		if err != nil {
			return errors.E(op, err)
		}
		// initialize buffer
		buf := c.get()
		defer c.put(buf)

		buf.Grow(len(b) + len(r.ServiceMethod))
		// writeServiceMethod to the buffer
		buf.WriteString(r.ServiceMethod)
		buf.Write(b)

		fr.WritePayloadLen(fr.Header(), uint32(buf.Len())) //nolint:gosec
		// copy inside
		fr.WritePayload(buf.Bytes())
		fr.WriteCRC(fr.Header())
		// send buffer
		return c.relay.Send(fr)

	case codec.(byte)&frame.CodecGob != 0:
		// initialize buffer
		buf := c.get()
		defer c.put(buf)

		buf.WriteString(r.ServiceMethod)

		dec := gob.NewEncoder(buf)
		err := dec.Encode(body)
		if err != nil {
			return errors.E(op, err)
		}

		fr.WritePayloadLen(fr.Header(), uint32(buf.Len())) //nolint:gosec
		// copy inside
		fr.WritePayload(buf.Bytes())
		fr.WriteCRC(fr.Header())
		// send buffer
		return c.relay.Send(fr)
	default:
		return c.handleError(r, fr, errors.E(op, errors.Str("unknown codec")).Error())
	}
}

func (c *Codec) handleError(r *rpc.Response, fr *frame.Frame, err string) error {
	buf := c.get()
	defer c.put(buf)

	// write all possible errors
	buf.WriteString(r.ServiceMethod)

	const op = errors.Op("handle codec error")
	fr.WriteFlags(fr.Header(), frame.ERROR)
	// error should be here
	if err != "" {
		buf.WriteString(err)
	}
	fr.WritePayloadLen(fr.Header(), uint32(buf.Len())) //nolint:gosec
	fr.WritePayload(buf.Bytes())

	fr.WriteCRC(fr.Header())
	_ = c.relay.Send(fr)
	return errors.E(op, errors.Str(r.Error))
}

// ReadRequestHeader receives frame with options
// options should have 2 values
// [0] - integer, sequence ID
// [1] - integer, offset for method name
// For example:
// 15Test.Payload
// SEQ_ID: 15
// METHOD_LEN: 12 and we take 12 bytes from the payload as method name
func (c *Codec) ReadRequestHeader(r *rpc.Request) error {
	const op = errors.Op("goridge_read_request_header")
	f := c.getFrame()

	err := c.relay.Receive(f)
	if err != nil {
		if stderr.Is(err, io.EOF) {
			c.putFrame(f)
			return err
		}

		c.putFrame(f)
		return err
	}

	// opts[0] sequence ID
	// opts[1] service method name offset from payload in bytes
	opts := f.ReadOptions(f.Header())
	if len(opts) != 2 {
		c.putFrame(f)
		return errors.E(op, errors.Str("should be 2 options. SEQ_ID and METHOD_LEN"))
	}

	r.Seq = uint64(opts[0])
	r.ServiceMethod = string(f.Payload()[:opts[1]])
	c.frame = f
	return c.storeCodec(r, f.ReadFlags())
}

func (c *Codec) storeCodec(r *rpc.Request, flag byte) error {
	switch {
	case flag&frame.CodecProto != 0:
		c.codec.Store(r.Seq, frame.CodecProto)
	case flag&frame.CodecJSON != 0:
		c.codec.Store(r.Seq, frame.CodecJSON)
	case flag&frame.CodecRaw != 0:
		c.codec.Store(r.Seq, frame.CodecRaw)
	case flag&frame.CodecMsgpack != 0:
		c.codec.Store(r.Seq, frame.CodecMsgpack)
	case flag&frame.CodecGob != 0:
		c.codec.Store(r.Seq, frame.CodecGob)
	default:
		c.codec.Store(r.Seq, frame.CodecGob)
	}

	return nil
}

// ReadRequestBody fetches prefixed body data and automatically unmarshal it as json. RawBody flag will populate
// []byte lice argument for rpc method.
func (c *Codec) ReadRequestBody(out any) error {
	const op = errors.Op("goridge_read_request_body")
	if out == nil {
		return nil
	}

	defer c.putFrame(c.frame)

	flags := c.frame.ReadFlags()

	switch { //nolint:dupl
	case flags&frame.CodecProto != 0:
		opts := c.frame.ReadOptions(c.frame.Header())
		if len(opts) != 2 {
			return errors.E(op, errors.Str("should be 2 options. SEQ_ID and METHOD_LEN"))
		}
		payload := c.frame.Payload()[opts[1]:]
		if len(payload) == 0 {
			return nil
		}

		// check if the out message is a correct proto.Message
		// instead send an error
		if pOut, ok := out.(proto.Message); ok {
			err := proto.Unmarshal(payload, pOut)
			if err != nil {
				return errors.E(op, err)
			}
			return nil
		}

		return errors.E(op, errors.Str("message type is not a proto"))
	case flags&frame.CodecJSON != 0:
		opts := c.frame.ReadOptions(c.frame.Header())
		if len(opts) != 2 {
			return errors.E(op, errors.Str("should be 2 options. SEQ_ID and METHOD_LEN"))
		}
		payload := c.frame.Payload()[opts[1]:]
		if len(payload) == 0 {
			return nil
		}
		return json.Unmarshal(payload, out)
	case flags&frame.CodecGob != 0:
		opts := c.frame.ReadOptions(c.frame.Header())
		if len(opts) != 2 {
			return errors.E(op, errors.Str("should be 2 options. SEQ_ID and METHOD_LEN"))
		}
		payload := c.frame.Payload()[opts[1]:]
		if len(payload) == 0 {
			return nil
		}

		buf := c.get()
		defer c.put(buf)

		dec := gob.NewDecoder(buf)
		buf.Write(payload)

		err := dec.Decode(out)
		if err != nil {
			return errors.E(op, err)
		}

		return nil
	case flags&frame.CodecRaw != 0:
		opts := c.frame.ReadOptions(c.frame.Header())
		if len(opts) != 2 {
			return errors.E(op, errors.Str("should be 2 options. SEQ_ID and METHOD_LEN"))
		}
		payload := c.frame.Payload()[opts[1]:]
		if len(payload) == 0 {
			return nil
		}

		if raw, ok := out.(*[]byte); ok {
			*raw = append(*raw, payload...)
		}

		return nil
	case flags&frame.CodecMsgpack != 0:
		opts := c.frame.ReadOptions(c.frame.Header())
		if len(opts) != 2 {
			return errors.E(op, errors.Str("should be 2 options. SEQ_ID and METHOD_LEN"))
		}
		payload := c.frame.Payload()[opts[1]:]
		if len(payload) == 0 {
			return nil
		}

		return msgpack.Unmarshal(payload, out)
	default:
		return errors.E(op, errors.Str("unknown decoder used in frame"))
	}
}

// Close underlying socket.
func (c *Codec) Close() error {
	if c.closed {
		return nil
	}

	c.closed = true
	return c.relay.Close()
}
