package goridge

import (
	"encoding/gob"
	"io"

	"github.com/spiral/errors"
	"github.com/vmihailenco/msgpack"
)

func encodeJSON(out io.Writer, data interface{}) error {
	const op = errors.Op("codec: encode json")

	res, err := json.Marshal(data)
	if err != nil {
		return errors.E(op, err)
	}
	_, err = out.Write(res)
	if err != nil {
		return errors.E(op, err)
	}
	return nil
}

func encodeGob(out io.Writer, data interface{}) error {
	const op = errors.Op("codec: encode GOB")

	dec := gob.NewEncoder(out)
	err := dec.Encode(data)
	if err != nil {
		return errors.E(op, err)
	}
	return nil
}

func encodeRaw(out io.Writer, data interface{}) error {
	const op = errors.Op("codec: encode raw")
	b := data.([]byte)
	_, err := out.Write(b)
	if err != nil {
		return errors.E(op, err)
	}

	return nil
}

func encodeMsgPack(out io.Writer, data interface{}) error {
	const op = errors.Op("codec: encode msgpack")
	b, err := msgpack.Marshal(data)
	if err != nil {
		return errors.E(op, err)
	}
	_, err = out.Write(b)
	if err != nil {
		return errors.E(op, err)
	}

	return nil
}