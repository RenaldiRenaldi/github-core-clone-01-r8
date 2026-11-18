package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"io"
	"sync"
)

var zipPool = sync.Pool{New: func() any { z, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed); return z }}

var types = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func pack(ob map[string]Object, ids []string) []byte {
	var b bytes.Buffer
	b.WriteString("PACK")
	binary.Write(&b, binary.BigEndian, uint32(2))
	binary.Write(&b, binary.BigEndian, uint32(len(ids)))
	for _, id := range ids {
		o := ob[id]
		typ := 3
		for k, v := range types {
			if v == o.Type {
				typ = k
			}
		}
		n := len(o.Data)
		c := byte(typ<<4 | n&15)
		n >>= 4
		for n > 0 {
			b.WriteByte(c | 128)
			c = byte(n & 127)
			n >>= 7
		}
		b.WriteByte(c)
		z := zipPool.Get().(*zlib.Writer)
		z.Reset(&b)
		z.Write(o.Data)
		z.Close()
		zipPool.Put(z)
	}
	sum := sha1.Sum(b.Bytes())
	b.Write(sum[:])
	return b.Bytes()
}
func delta(base, d []byte) []byte {
	i := 0
	readVar := func() int {
		n, shift := 0, uint(0)
		for {
			if i >= len(d) {
				fail(400, "Truncated delta")
			}
			c := d[i]
			i++
			n |= int(c&127) << shift
			shift += 7
			if c&128 == 0 {
				return n
			}
			if shift > 63 {
				fail(400, "Delta overflow")
			}
		}
	}
	if readVar() != len(base) {
		fail(400, "Delta source length")
	}
	size := readVar()
	if size > 64<<20 {
		fail(400, "Delta too large")
	}
	out := make([]byte, 0, size)
	for i < len(d) {
		c := d[i]
		i++
		if c&128 != 0 {
			off, n := 0, 0
			for j := 0; j < 4; j++ {
				if c&(1<<j) != 0 {
					if i >= len(d) {
						fail(400, "Delta truncated")
					}
					off |= int(d[i]) << (8 * j)
					i++
				}
			}
			for j := 0; j < 3; j++ {
				if c&(1<<(4+j)) != 0 {
					if i >= len(d) {
						fail(400, "Delta truncated")
					}
					n |= int(d[i]) << (8 * j)
					i++
				}
			}
			if n == 0 {
				n = 65536
			}
			if off+n > len(base) {
				fail(400, "Delta bounds")
			}
			out = append(out, base[off:off+n]...)
		} else {
			n := int(c)
			if n == 0 || i+n > len(d) {
				fail(400, "Delta opcode")
			}
			out = append(out, d[i:i+n]...)
			i += n
		}
		if len(out) > size {
			fail(400, "Delta overflow")
		}
	}
	if len(out) != size {
		fail(400, "Delta target length")
	}
	return out
}

func parsePack(raw []byte, existing map[string]Object) map[string]Object {
	return parsePackReader(bytes.NewReader(raw), existing)
}

func sideband(raw []byte) []byte {
	var framed bytes.Buffer
	for len(raw) != 0 {
		n := min(len(raw), 65500)
		framed.Write(pkt(string(append([]byte{1}, raw[:n]...))))
		raw = raw[n:]
	}
	return framed.Bytes()
}

// The R8 decoder handles regular, offset-delta, and reference-delta entries.
func parsePackReader(input io.Reader, existing map[string]Object) map[string]Object {
	raw, err := io.ReadAll(io.LimitReader(input, 64<<20+1))
	if err != nil || len(raw) > 64<<20 {
		fail(400, "Pack too large")
	}
	prior := make(map[string]r8Object, len(existing))
	for id, obj := range existing {
		prior[id] = r8Object{kind: obj.Type, data: obj.Data}
	}
	decoded := r8InflatePack(raw, prior)
	out := make(map[string]Object, len(decoded))
	for id, obj := range decoded {
		out[id] = Object{Type: obj.kind, Data: obj.data}
	}
	return out
}
