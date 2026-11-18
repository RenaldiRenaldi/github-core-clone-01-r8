package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
)

type r8Object struct {
	kind string
	data []byte
}

func r8OID(o r8Object) string {
	b := []byte(fmt.Sprintf("%s %d\x00", o.kind, len(o.data)))
	b = append(b, o.data...)
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:])
}
func r8ReadVarint(b []byte, i *int) int {
	n, shift := 0, uint(0)
	for {
		if *i >= len(b) || shift > 56 {
			panic("bad delta")
		}
		c := b[*i]
		*i++
		n |= int(c&127) << shift
		if c&128 == 0 {
			return n
		}
		shift += 7
	}
}
func r8ApplyDelta(base, d []byte) []byte {
	i := 0
	if r8ReadVarint(d, &i) != len(base) {
		panic("delta base size")
	}
	size := r8ReadVarint(d, &i)
	if size > 64<<20 {
		panic("delta size")
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
						panic("delta offset")
					}
					off |= int(d[i]) << (8 * j)
					i++
				}
			}
			for j := 0; j < 3; j++ {
				if c&(1<<(4+j)) != 0 {
					if i >= len(d) {
						panic("delta length")
					}
					n |= int(d[i]) << (8 * j)
					i++
				}
			}
			if n == 0 {
				n = 65536
			}
			if off+n > len(base) {
				panic("delta bounds")
			}
			out = append(out, base[off:off+n]...)
		} else {
			n := int(c)
			if n == 0 || i+n > len(d) {
				panic("delta insert")
			}
			out = append(out, d[i:i+n]...)
			i += n
		}
		if len(out) > size {
			panic("delta overflow")
		}
	}
	if len(out) != size {
		panic("delta size mismatch")
	}
	return out
}
func r8InflatePack(raw []byte, existing map[string]r8Object) map[string]r8Object {
	if len(raw) < 32 || string(raw[:4]) != "PACK" {
		panic("missing pack")
	}
	version := binary.BigEndian.Uint32(raw[4:8])
	if version != 2 && version != 3 {
		panic("pack version")
	}
	count := binary.BigEndian.Uint32(raw[8:12])
	if count > 100000 {
		panic("pack count")
	}
	type item struct {
		off, typ, base int
		baseID         string
		data           []byte
	}
	items := []item{}
	pos := 12
	for k := uint32(0); k < count; k++ {
		off := pos
		if pos >= len(raw)-20 {
			panic("pack eof")
		}
		c := raw[pos]
		pos++
		typ := int(c >> 4 & 7)
		size := int(c & 15)
		shift := uint(4)
		for c&128 != 0 {
			if pos >= len(raw)-20 || shift > 56 {
				panic("pack size")
			}
			c = raw[pos]
			pos++
			size |= int(c&127) << shift
			shift += 7
		}
		it := item{off: off, typ: typ}
		if typ == 6 {
			c = raw[pos]
			pos++
			dist := int(c & 127)
			for c&128 != 0 {
				c = raw[pos]
				pos++
				dist = ((dist + 1) << 7) | int(c&127)
			}
			it.base = off - dist
		} else if typ == 7 {
			if pos+20 > len(raw)-20 {
				panic("ref delta")
			}
			it.baseID = hex.EncodeToString(raw[pos : pos+20])
			pos += 20
		} else if map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}[typ] == "" {
			panic("object type")
		}
		compressed := bytes.NewReader(raw[pos:])
		z, e := zlib.NewReader(compressed)
		if e != nil {
			panic("zlib")
		}
		data, e := io.ReadAll(io.LimitReader(z, 64<<20+1))
		z.Close()
		if e != nil || len(data) != size {
			panic("object size")
		}
		pos = len(raw) - compressed.Len()
		it.data = data
		items = append(items, it)
	}
	sum := sha1.Sum(raw[:pos])
	if pos+20 != len(raw) || !bytes.Equal(raw[pos:], sum[:]) {
		panic("pack checksum")
	}
	out := map[string]r8Object{}
	byOff := map[int]r8Object{}
	remaining := items
	for len(remaining) > 0 {
		next := []item{}
		for _, it := range remaining {
			var o r8Object
			if it.typ == 6 || it.typ == 7 {
				var base r8Object
				var ok bool
				if it.typ == 6 {
					base, ok = byOff[it.base]
				} else {
					base, ok = out[it.baseID]
					if !ok {
						base, ok = existing[it.baseID]
					}
				}
				if !ok {
					next = append(next, it)
					continue
				}
				o = r8Object{base.kind, r8ApplyDelta(base.data, it.data)}
			} else {
				o = r8Object{map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}[it.typ], it.data}
			}
			id := r8OID(o)
			out[id] = o
			byOff[it.off] = o
		}
		if len(next) == len(remaining) {
			panic("unresolved delta")
		}
		remaining = next
	}
	return out
}
