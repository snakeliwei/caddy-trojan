package trojan

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// upstream is a global repository for saving all users
var upstream = &Upstream{}

func init() {
	upstream.users = make(map[string]struct{})
	upstream.usage.repo = make(map[string]usage)
}

// ByteSliceToString is ...
func ByteSliceToString(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}

// StringToByteSlice is ...
func StringToByteSlice(s string) []byte {
	type SliceHeader struct {
		Data uintptr
		Len  int
		Cap  int
	}
	ptr := (*reflect.StringHeader)(unsafe.Pointer(&s))
	hdr := &SliceHeader{
		Data: ptr.Data,
		Len:  ptr.Len,
		Cap:  ptr.Len,
	}
	return *(*[]byte)(unsafe.Pointer(hdr))
}

type usage struct {
	up   int64
	down int64
}

// Upstream is ...
type Upstream struct {
	// RWMutex is ...
	sync.RWMutex
	// users is ...
	users map[string]struct{}
	// users usage
	usage struct {
		// RWMutex is ...
		sync.RWMutex
		// repo is ...
		repo map[string]usage
	}
	// total usage
	total usage
}

// AddKey is ...
func (u *Upstream) AddKey(k string) error {
	key := fmt.Sprintf("Basic %v", base64.StdEncoding.EncodeToString(StringToByteSlice(k)))
	u.Lock()
	u.users[key] = struct{}{}
	u.users[k] = struct{}{}
	u.Unlock()
	return nil
}

// Add is ...
func (u *Upstream) Add(s string) error {
	b := [HeaderLen]byte{}
	GenKey(s, b[:])
	u.AddKey(ByteSliceToString(b[:]))
	return nil
}

// DelKey is ...
func (u *Upstream) DelKey(k string) error {
	key := fmt.Sprintf("Basic %v", base64.StdEncoding.EncodeToString(StringToByteSlice(k)))
	u.Lock()
	delete(u.users, key)
	delete(u.users, k)
	u.usage.Lock()
	delete(u.usage.repo, key)
	delete(u.usage.repo, k)
	u.usage.Unlock()
	u.Unlock()
	return nil
}

// Del is ...
func (u *Upstream) Del(s string) error {
	b := [HeaderLen]byte{}
	GenKey(s, b[:])
	u.DelKey(ByteSliceToString(b[:]))
	return nil
}

// Range is ...
func (u *Upstream) Range(fn func(k string, up, down int64)) {
	const AuthLen = 82

	u.RLock()
	for k := range u.users {
		if len(k) == AuthLen {
			continue
		}

		u.usage.RLock()
		v, ok := u.usage.repo[k]
		u.usage.RUnlock()
		if !ok {
			v = usage{}
		}

		k1 := fmt.Sprintf("Basic %v", base64.StdEncoding.EncodeToString([]byte(k)))
		u.usage.RLock()
		v1, ok := u.usage.repo[k1]
		u.usage.RUnlock()
		if !ok {
			v1 = usage{}
		}

		fn(k, v.up+v1.up, v.down+v1.down)
	}
	u.RUnlock()
}

// Validate is ...
func (u *Upstream) Validate(s string) bool {
	u.RLock()
	_, ok := u.users[s]
	u.RUnlock()
	return ok
}

// Consume is ...
func (u *Upstream) Consume(s string, nr, nw int64) {
	u.usage.Lock()
	use, ok := u.usage.repo[s]
	if !ok {
		use = usage{}
	}
	use.up += nr
	use.down += nw
	u.usage.repo[s] = use
	u.usage.Unlock()

	atomic.AddInt64(&u.total.up, nr)
	atomic.AddInt64(&u.total.down, nw)
}

// HeaderLen is ...
const HeaderLen = 56

const (
	// CmdConnect is ...
	CmdConnect = 1
	// CmdAssociate is ...
	CmdAssociate = 3
)

// GenKey is ...
func GenKey(s string, key []byte) {
	hash := sha256.Sum224([]byte(s))
	hex.Encode(key, hash[:])
}

// Handle is ...
func Handle(r io.Reader, w io.Writer) (int64, int64, error) {
	b := [1 + MaxAddrLen + 2]byte{}

	// read command
	if _, err := io.ReadFull(r, b[:1]); err != nil {
		return 0, 0, fmt.Errorf("read command error: %w", err)
	}

	// read address
	addr, err := ReadAddrBuffer(r, b[3:])
	if err != nil {
		return 0, 0, fmt.Errorf("read addr error: %w", err)
	}

	// read 0x0d, 0x0a
	if _, err := io.ReadFull(r, b[1:3]); err != nil {
		return 0, 0, fmt.Errorf("read 0x0d 0x0a error: %w", err)
	}

	switch b[0] {
	case CmdConnect:
		tgt, err := ResolveTCPAddr(addr)
		if err != nil {
			return 0, 0, fmt.Errorf("resolve tcp addr error: %w", err)
		}
		nr, nw, err := HandleTCP(r, w, tgt)
		if err != nil {
			return nr, nw, fmt.Errorf("handle tcp error: %w", err)
		}
		return nr, nw, nil
	case CmdAssociate:
		nr, nw, err := HandleUDP(r, w, time.Minute*10)
		if err != nil {
			return nr, nw, fmt.Errorf("handle udp error: %w", err)
		}
		return nr, nw, nil
	default:
	}
	return 0, 0, errors.New("command error")
}

// HandleTCP is ...
// trojan TCP stream
func HandleTCP(r io.Reader, w io.Writer, addr *net.TCPAddr) (int64, int64, error) {
	rc, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		return 0, 0, err
	}
	defer rc.Close()

	type Result struct {
		Num int64
		Err error
	}

	errCh := make(chan Result, 1)
	go func(rc *net.TCPConn, r io.Reader, errCh chan Result) {
		nr, err := io.Copy(io.Writer(rc), r)
		if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			rc.CloseWrite()
			errCh <- Result{Num: nr, Err: nil}
			return
		}
		rc.SetReadDeadline(time.Now())
		errCh <- Result{Num: nr, Err: err}
	}(rc, r, errCh)

	nr, nw, err := func(rc *net.TCPConn, w io.Writer, errCh chan Result) (int64, int64, error) {
		nw, err := io.Copy(w, io.Reader(rc))
		if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			type CloseWriter interface {
				CloseWrite() error
			}
			if closer, ok := w.(CloseWriter); ok {
				closer.CloseWrite()
			}
			r := <-errCh
			return r.Num, nw, r.Err
		}
		rc.SetWriteDeadline(time.Now())
		rc.CloseWrite()
		r := <-errCh
		return r.Num, nw, err
	}(rc, w, errCh)

	return nr, nw, err
}

// HandleUDP is ...
// [AddrType(1 byte)][Addr(max 256 byte)][Port(2 byte)][Len(2 byte)][0x0d, 0x0a][Data(max 65535 byte)]
func HandleUDP(r io.Reader, w io.Writer, timeout time.Duration) (int64, int64, error) {
	rc, err := net.ListenUDP("udp", nil)
	if err != nil {
		return 0, 0, err
	}
	defer rc.Close()

	type Result struct {
		Num int64
		Err error
	}

	errCh := make(chan Result, 1)
	go func(rc *net.UDPConn, r io.Reader, errCh chan Result) (nr int64, err error) {
		defer func() {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrDeadlineExceeded) {
				err = nil
			}
			errCh <- Result{Num: nr, Err: err}
		}()

		// save previous address
		bb := make([]byte, MaxAddrLen)
		tt := (*net.UDPAddr)(nil)

		b := make([]byte, 16*1024)
		for {
			raddr, er := ReadAddrBuffer(r, b)
			if er != nil {
				err = er
				break
			}

			l := len(raddr.Addr)

			if !bytes.Equal(bb, raddr.Addr) {
				addr, er := ResolveUDPAddr(raddr)
				if er != nil {
					err = er
					break
				}
				bb = append(bb[:0], raddr.Addr...)
				tt = addr
			}

			if _, er := io.ReadFull(r, b[l:l+4]); er != nil {
				err = er
				break
			}

			l += (int(b[l])<<8 | int(b[l+1]))
			nr += int64(l) + 4

			buf := b[len(raddr.Addr):l]
			if _, er := io.ReadFull(r, buf); er != nil {
				err = er
				break
			}

			if _, ew := rc.WriteToUDP(buf, tt); ew != nil {
				err = ew
				break
			}
		}
		rc.SetReadDeadline(time.Now())
		return
	}(rc, r, errCh)

	nr, nw, err := func(rc *net.UDPConn, w io.Writer, errCh chan Result, timeout time.Duration) (_, nw int64, err error) {
		b := make([]byte, 16*1024)

		b[MaxAddrLen+2] = 0x0d
		b[MaxAddrLen+3] = 0x0a
		for {
			rc.SetReadDeadline(time.Now().Add(timeout))
			n, addr, er := rc.ReadFrom(b[MaxAddrLen+4:])
			if er != nil {
				err = er
				break
			}

			b[MaxAddrLen] = byte(n >> 8)
			b[MaxAddrLen+1] = byte(n)

			l := func(bb []byte, addr *net.UDPAddr) int64 {
				if ipv4 := addr.IP.To4(); ipv4 != nil {
					const offset = MaxAddrLen - (1 + net.IPv4len + 2)
					bb[offset] = AddrTypeIPv4
					copy(bb[offset+1:], ipv4)
					bb[offset+1+net.IPv4len], bb[offset+1+net.IPv4len+1] = byte(addr.Port>>8), byte(addr.Port)
					return 1 + net.IPv4len + 2
				} else {
					const offset = MaxAddrLen - (1 + net.IPv6len + 2)
					bb[offset] = AddrTypeIPv6
					copy(bb[offset+1:], addr.IP.To16())
					bb[offset+1+net.IPv6len], bb[offset+1+net.IPv6len+1] = byte(addr.Port>>8), byte(addr.Port)
					return 1 + net.IPv6len + 2
				}
			}(b[:MaxAddrLen], addr.(*net.UDPAddr))
			nw += 4 + int64(n) + l

			if _, ew := w.Write(b[MaxAddrLen-l : MaxAddrLen+4+n]); ew != nil {
				err = ew
				break
			}
		}
		rc.SetWriteDeadline(time.Now())

		if errors.Is(err, io.EOF) || errors.Is(err, os.ErrDeadlineExceeded) {
			r := <-errCh
			return r.Num, nw, r.Err
		}
		r := <-errCh
		return r.Num, nw, err
	}(rc, w, errCh, timeout)

	return nr, nw, err
}
