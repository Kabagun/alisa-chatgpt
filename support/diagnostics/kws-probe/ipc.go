package main

import (
	"encoding/binary"
	"errors"
	"io"

	"google.golang.org/protobuf/encoding/protowire"
)

const maxFrameBytes = 1 << 20

var invalidFrame = errors.New("invalid frame")

func fields(b []byte, visit func(protowire.Number, protowire.Type, uint64, []byte) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 || num < 1 || num > protowire.MaxValidNumber {
			return invalidFrame
		}
		b = b[n:]
		var value uint64
		var data []byte
		switch typ {
		case protowire.VarintType:
			value, n = protowire.ConsumeVarint(b)
		case protowire.BytesType:
			data, n = protowire.ConsumeBytes(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return invalidFrame
		}
		if err := visit(num, typ, value, data); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

func readFrame(r io.Reader) (uint64, []byte, []byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, nil, err
	}
	size := binary.LittleEndian.Uint32(header[:])
	if size == 0 || size > maxFrameBytes {
		return 0, nil, nil, invalidFrame
	}
	body := make([]byte, int(size))
	if _, err := io.ReadFull(r, body); err != nil {
		clear(body)
		return 0, nil, nil, err
	}
	var kind uint64
	var payload []byte
	var haveKind, havePayload bool
	err := fields(body, func(num protowire.Number, typ protowire.Type, value uint64, data []byte) error {
		if num == 253 {
			if haveKind || typ != protowire.VarintType {
				return invalidFrame
			}
			haveKind, kind = true, value
		} else if num == 252 {
			if havePayload || typ != protowire.BytesType {
				return invalidFrame
			}
			havePayload, payload = true, data
		}
		return nil
	})
	if err != nil || !haveKind || !havePayload {
		clear(body)
		return 0, nil, nil, invalidFrame
	}
	return kind, payload, body, nil
}

func bytesField(b []byte, n protowire.Number, v []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(b, n, protowire.BytesType), v)
}

func subscription() []byte {
	body := bytesField(nil, 252, bytesField(nil, 1, nil))
	body = protowire.AppendVarint(protowire.AppendTag(body, 253, protowire.VarintType), 159)
	return append(binary.LittleEndian.AppendUint32(nil, uint32(len(body))), body...)
}

func decodePCM(payload []byte, consume func([]byte) bool) bool {
	return fields(payload, func(num protowire.Number, typ protowire.Type, _ uint64, data []byte) error {
		if num != 1 {
			return nil
		}
		if typ != protowire.BytesType {
			return invalidFrame
		}
		var name, pcm []byte
		var rate, format uint64
		var seen uint8
		err := fields(data, func(n protowire.Number, t protowire.Type, v uint64, b []byte) error {
			var bit uint8
			switch n {
			case 2:
				bit = 1
			case 3:
				bit = 2
			case 7:
				bit = 4
			case 8:
				bit = 8
			default:
				return nil
			}
			if seen&bit != 0 {
				return invalidFrame
			}
			seen |= bit
			if n == 2 || n == 3 {
				if t != protowire.BytesType {
					return invalidFrame
				}
				if n == 2 {
					name = b
				} else {
					pcm = b
				}
			} else {
				if t != protowire.VarintType {
					return invalidFrame
				}
				if n == 7 {
					rate = v
				} else {
					format = v
				}
			}
			return nil
		})
		if err != nil || seen&1 == 0 {
			return invalidFrame
		}
		if string(name) != "vqe_0" {
			return nil
		}
		if seen != 15 || rate != 16000 || format != 1 || len(pcm)%2 != 0 || !consume(pcm) {
			return invalidFrame
		}
		return nil
	}) == nil
}
