package mc

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// servers.dat is a gzip-compressed NBT file the official launcher reads to fill
// the multiplayer list. This file implements just enough NBT to read it, merge
// one {name, ip} entry into the "servers" list, and write it back — preserving
// every other tag so existing server entries survive untouched. No third-party
// NBT library: the format is small (a handful of tag types) and self-contained.

// NBT tag type bytes (spec.toolshed / "Unnamed Binary Tag").
const (
	tagEnd       = 0x00
	tagByte      = 0x01
	tagShort     = 0x02
	tagInt       = 0x03
	tagLong      = 0x04
	tagFloat     = 0x05
	tagDouble    = 0x06
	tagByteArray = 0x07
	tagString    = 0x08
	tagList      = 0x09
	tagCompound  = 0x0A
	tagIntArray  = 0x0B
	tagLongArray = 0x0C
)

// nbtNode is one named NBT value. For compound nodes val is []*nbtNode (ordered
// children, matching wire order); for list nodes it is []*nbtNode whose names
// are empty and whose typ repeats the list element type.
type nbtNode struct {
	name string
	typ  byte
	val  any
}

// child returns the named child of a compound node, or nil.
func (n *nbtNode) child(name string) *nbtNode {
	nodes, _ := n.val.([]*nbtNode)
	for _, c := range nodes {
		if c.name == name {
			return c
		}
	}
	return nil
}

// set creates or replaces the named child of a compound node.
func (n *nbtNode) set(name string, typ byte, val any) {
	nodes := n.val.([]*nbtNode)
	for _, c := range nodes {
		if c.name == name {
			c.typ, c.val = typ, val
			return
		}
	}
	n.val = append(nodes, &nbtNode{name: name, typ: typ, val: val})
}

// AddServerToLauncher inserts (or updates) a server {name, addr} entry into the
// launcher's multiplayer list (servers.dat). gameDir, when empty, is auto-detected
// (PCL's .minecraft, then the official one). The original file is preserved as
// servers.dat.bak first. Returns true when the list changed.
func AddServerToLauncher(name, addr, gameDir string) (bool, error) {
	if gameDir == "" {
		gameDir = GameDir()
	}
	if gameDir == "" {
		return false, fmt.Errorf("未找到 .minecraft 目录")
	}
	return addServerToLauncherIn(gameDir, name, addr)
}

// addServerToLauncherIn does the merge inside a given .minecraft directory
// (separated out so tests can exercise it against a temp dir).
func addServerToLauncherIn(mc, name, addr string) (bool, error) {
	path := filepath.Join(mc, "servers.dat")
	root, exists, err := readServersDat(path)
	if err != nil {
		return false, err
	}

	servers := root.child("servers")
	if servers == nil {
		servers = &nbtNode{typ: tagList, val: []*nbtNode{}}
		root.set("servers", tagList, []*nbtNode{})
		servers = root.child("servers")
	}
	list := servers.val.([]*nbtNode)

	changed := false
	for _, e := range list {
		if e.stringOf("ip") == addr {
			e.set("name", tagString, name)
			changed = true
		}
	}
	if !changed {
		entry := &nbtNode{typ: tagCompound, val: []*nbtNode{
			{name: "name", typ: tagString, val: name},
			{name: "ip", typ: tagString, val: addr},
		}}
		list = append(list, entry)
		servers.val = list
		changed = true
	}
	if !changed {
		return false, nil
	}

	var buf bytes.Buffer
	gw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return false, err
	}
	if err := writeNBT(gw, root); err != nil {
		return false, err
	}
	if err := gw.Close(); err != nil {
		return false, err
	}

	if exists {
		if err := copyFile(path, path+".bak"); err != nil {
			return false, fmt.Errorf("备份 servers.dat 失败: %w", err)
		}
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return false, fmt.Errorf("写 servers.dat 失败: %w", err)
	}
	return true, nil
}

// readServersDat loads the root compound of servers.dat. A missing file is not
// an error: the caller builds a fresh root.
func readServersDat(path string) (root *nbtNode, exists bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &nbtNode{typ: tagCompound, val: []*nbtNode{}}, false, nil
		}
		return nil, false, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, true, fmt.Errorf("读取 servers.dat（不是有效的 gzip）: %w", err)
	}
	defer gz.Close()
	root, err = readNBT(bufio.NewReader(gz))
	if err != nil {
		return nil, true, fmt.Errorf("解析 servers.dat: %w", err)
	}
	if root == nil {
		return &nbtNode{typ: tagCompound, val: []*nbtNode{}}, true, nil
	}
	return root, true, nil
}

func (n *nbtNode) stringOf(name string) string {
	if c := n.child(name); c != nil {
		if s, ok := c.val.(string); ok {
			return s
		}
	}
	return ""
}

// readNBT reads one named value (or nil for TAG_End).
func readNBT(r *bufio.Reader) (*nbtNode, error) {
	typ, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if typ == tagEnd {
		return nil, nil
	}
	name, err := readString(r)
	if err != nil {
		return nil, err
	}
	val, err := readValue(r, typ)
	if err != nil {
		return nil, err
	}
	return &nbtNode{name: name, typ: typ, val: val}, nil
}

func readString(r *bufio.Reader) (string, error) {
	var n uint16
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return "", err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func readValue(r *bufio.Reader, typ byte) (any, error) {
	switch typ {
	case tagByte:
		b, err := r.ReadByte()
		return int8(b), err
	case tagShort:
		var v int16
		return v, binary.Read(r, binary.BigEndian, &v)
	case tagInt:
		var v int32
		return v, binary.Read(r, binary.BigEndian, &v)
	case tagLong:
		var v int64
		return v, binary.Read(r, binary.BigEndian, &v)
	case tagFloat:
		var v float32
		return v, binary.Read(r, binary.BigEndian, &v)
	case tagDouble:
		var v float64
		return v, binary.Read(r, binary.BigEndian, &v)
	case tagByteArray:
		var n int32
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			return nil, err
		}
		buf := make([]byte, n)
		_, err := io.ReadFull(r, buf)
		return buf, err
	case tagString:
		return readString(r)
	case tagList:
		elemType, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		var n int32
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			return nil, err
		}
		out := make([]*nbtNode, 0, n)
		for i := int32(0); i < n; i++ {
			v, err := readValue(r, elemType)
			if err != nil {
				return nil, err
			}
			out = append(out, &nbtNode{typ: elemType, val: v})
		}
		return out, nil
	case tagCompound:
		var out []*nbtNode
		for {
			c, err := readNBT(r)
			if err != nil {
				return nil, err
			}
			if c == nil {
				break
			}
			out = append(out, c)
		}
		return out, nil
	case tagIntArray:
		var n int32
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			return nil, err
		}
		out := make([]int32, n)
		for i := range out {
			if err := binary.Read(r, binary.BigEndian, &out[i]); err != nil {
				return nil, err
			}
		}
		return out, nil
	case tagLongArray:
		var n int32
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			return nil, err
		}
		out := make([]int64, n)
		for i := range out {
			if err := binary.Read(r, binary.BigEndian, &out[i]); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("未知 NBT 标签类型 0x%02x", typ)
}

func writeNBT(w io.Writer, n *nbtNode) error {
	if n == nil {
		_, err := w.Write([]byte{tagEnd})
		return err
	}
	if _, err := w.Write([]byte{n.typ}); err != nil {
		return err
	}
	if err := writeString(w, n.name); err != nil {
		return err
	}
	return writeValue(w, n.typ, n.val)
}

func writeString(w io.Writer, s string) error {
	if len(s) > 0xFFFF {
		return fmt.Errorf("字符串过长: %d", len(s))
	}
	if err := binary.Write(w, binary.BigEndian, uint16(len(s))); err != nil {
		return err
	}
	_, err := io.WriteString(w, s)
	return err
}

func writeValue(w io.Writer, typ byte, val any) error {
	switch typ {
	case tagByte:
		_, err := w.Write([]byte{byte(val.(int8))})
		return err
	case tagShort:
		return binary.Write(w, binary.BigEndian, val.(int16))
	case tagInt:
		return binary.Write(w, binary.BigEndian, val.(int32))
	case tagLong:
		return binary.Write(w, binary.BigEndian, val.(int64))
	case tagFloat:
		return binary.Write(w, binary.BigEndian, val.(float32))
	case tagDouble:
		return binary.Write(w, binary.BigEndian, val.(float64))
	case tagByteArray:
		b := val.([]byte)
		if err := binary.Write(w, binary.BigEndian, int32(len(b))); err != nil {
			return err
		}
		_, err := w.Write(b)
		return err
	case tagString:
		return writeString(w, val.(string))
	case tagList:
		nodes := val.([]*nbtNode)
		elemType := byte(tagEnd)
		if len(nodes) > 0 {
			elemType = nodes[0].typ
		}
		if _, err := w.Write([]byte{elemType}); err != nil {
			return err
		}
		if err := binary.Write(w, binary.BigEndian, int32(len(nodes))); err != nil {
			return err
		}
		for _, c := range nodes {
			if err := writeValue(w, c.typ, c.val); err != nil {
				return err
			}
		}
		return nil
	case tagCompound:
		for _, c := range val.([]*nbtNode) {
			if err := writeNBT(w, c); err != nil {
				return err
			}
		}
		_, err := w.Write([]byte{tagEnd})
		return err
	case tagIntArray:
		arr := val.([]int32)
		if err := binary.Write(w, binary.BigEndian, int32(len(arr))); err != nil {
			return err
		}
		for _, v := range arr {
			if err := binary.Write(w, binary.BigEndian, v); err != nil {
				return err
			}
		}
		return nil
	case tagLongArray:
		arr := val.([]int64)
		if err := binary.Write(w, binary.BigEndian, int32(len(arr))); err != nil {
			return err
		}
		for _, v := range arr {
			if err := binary.Write(w, binary.BigEndian, v); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("未知 NBT 标签类型 0x%02x", typ)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
