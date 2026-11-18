package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
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

// bufio.Reader implements io.ByteReader, preventing zlib from consuming the next object.
type packStream struct {
	r *bufio.Reader
	h hash.Hash
	n int
}

func (s *packStream) Read(p []byte) (int, error) {
	n, e := s.r.Read(p)
	s.n += n
	s.h.Write(p[:n])
	return n, e
}
func (s *packStream) ReadByte() (byte, error) {
	b, e := s.r.ReadByte()
	if e == nil {
		s.n++
		s.h.Write([]byte{b})
	}
	return b, e
}
func parsePack(raw []byte, existing map[string]Object) map[string]Object {
	return parsePackReader(bytes.NewReader(raw), existing)
}

// Only inflated objects are retained: compressed input is consumed directly from io.Reader.
func parsePackReader(input io.Reader, existing map[string]Object) map[string]Object {
	br := &packStream{r: bufio.NewReader(input), h: sha1.New()}
	header := make([]byte, 12)
	if _, e := io.ReadFull(br, header); e != nil || string(header[:4]) != "PACK" {
		fail(400, "Invalid pack")
	}
	version := binary.BigEndian.Uint32(header[4:8])
	if version != 2 && version != 3 {
		fail(400, "Pack version")
	}
	count := int(binary.BigEndian.Uint32(header[8:12]))
	if count > 1000000 {
		fail(400, "Pack count")
	}
	offset := func() int { return br.n }
	type pending struct {
		off, typ, baseOff int
		baseID            string
		data              []byte
	}
	items := []pending{}
	for k := 0; k < count; k++ {
		off := offset()
		c, e := br.ReadByte()
		if e != nil {
			fail(400, "Pack EOF")
		}
		typ := int(c >> 4 & 7)
		size := int(c & 15)
		shift := uint(4)
		for c&128 != 0 {
			c, e = br.ReadByte()
			if e != nil || shift > 60 {
				fail(400, "Pack size")
			}
			size |= int(c&127) << shift
			shift += 7
		}
		p := pending{off: off, typ: typ}
		if typ == 6 {
			c, e = br.ReadByte()
			if e != nil {
				fail(400, "Delta offset")
			}
			dist := int(c & 127)
			for c&128 != 0 {
				c, e = br.ReadByte()
				if e != nil {
					fail(400, "Delta offset")
				}
				dist = ((dist + 1) << 7) | int(c&127)
			}
			p.baseOff = off - dist
		} else if typ == 7 {
			id := make([]byte, 20)
			if _, e = io.ReadFull(br, id); e != nil {
				fail(400, "Delta id")
			}
			p.baseID = hex.EncodeToString(id)
		} else if types[typ] == "" {
			fail(400, "Pack object type")
		}
		if size > 64<<20 {
			fail(400, "Object too large")
		}
		z, e := zlib.NewReader(br)
		if e != nil {
			fail(400, e.Error())
		}
		p.data, e = io.ReadAll(io.LimitReader(z, 64<<20+1))
		z.Close()
		if e != nil || len(p.data) != size {
			fail(400, "Inflated size mismatch")
		}
		items = append(items, p)
	}
	checksum := make([]byte, 20)
	if _, e := io.ReadFull(br.r, checksum); e != nil || !bytes.Equal(checksum, br.h.Sum(nil)) {
		fail(400, "Pack checksum")
	}
	if _, e := br.r.Peek(1); e != io.EOF {
		fail(400, "Trailing pack data")
	}
	out := map[string]Object{}
	byOff := map[int]Object{}
	remaining := items
	for len(remaining) > 0 {
		next := []pending{}
		for _, p := range remaining {
			var o Object
			if p.typ == 6 || p.typ == 7 {
				base, ok := byOff[p.baseOff]
				if p.typ == 7 {
					base, ok = out[p.baseID]
					if !ok {
						base, ok = existing[p.baseID]
					}
				}
				if !ok {
					next = append(next, p)
					continue
				}
				o = Object{base.Type, delta(base.Data, p.data)}
			} else {
				o = Object{types[p.typ], p.data}
			}
			out[oid(o)] = o
			byOff[p.off] = o
		}
		if len(next) == len(remaining) {
			fail(400, "Unresolved delta")
		}
		remaining = next
	}
	return out
}
func pkt(s string) []byte { return append([]byte(fmt.Sprintf("%04x", len(s)+4)), []byte(s)...) }
func packets(raw []byte) ([]string, int) {
	var out []string
	i := 0
	for i+4 <= len(raw) {
		var n int
		if _, e := fmt.Sscanf(string(raw[i:i+4]), "%04x", &n); e != nil {
			break
		}
		i += 4
		if n == 0 {
			return out, i
		}
		if n < 4 {
			continue
		}
		if i+n-4 > len(raw) {
			fail(400, "Truncated pkt-line")
		}
		out = append(out, string(raw[i:i+n-4]))
		i += n - 4
	}
	return out, i
}
func sideband(raw []byte) []byte {
	var b bytes.Buffer
	for len(raw) > 0 {
		n := len(raw)
		if n > 65500 {
			n = 65500
		}
		b.Write(pkt(string(append([]byte{1}, raw[:n]...))))
		raw = raw[n:]
	}
	return b.Bytes()
}
