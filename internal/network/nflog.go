package network

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The NFLOG side of the flow log: a netlink socket bound to FlowLogGroup and
// the parsing of what it receives, written against the kernel's own header
// (linux/netfilter/nfnetlink_log.h) rather than a library, as the rest of this
// package talks to the kernel through the tools or x/sys.
const (
	nfnlSubsysULOG  = 4 // NFNL_SUBSYS_ULOG
	nfulnlMsgPacket = 0 // NFULNL_MSG_PACKET
	nfulnlMsgConfig = 1 // NFULNL_MSG_CONFIG

	nfulaCfgCmd  = 1 // NFULA_CFG_CMD
	nfulaCfgMode = 2 // NFULA_CFG_MODE

	nfulnlCfgCmdBind = 1 // NFULNL_CFG_CMD_BIND
	nfulnlCopyPacket = 2 // NFULNL_COPY_PACKET

	nfulaIfindexIndev = 4  // NFULA_IFINDEX_INDEV
	nfulaPayload      = 9  // NFULA_PAYLOAD
	nfulaPrefix       = 10 // NFULA_PREFIX

	nlaTypeMask = 0x3fff // attribute type without NLA_F_NESTED / NLA_F_NET_BYTEORDER

	// nflogCopyRange is how much of each packet the kernel copies: the IPv4
	// header at its longest (60 bytes) and the destination port after it.
	// Nothing of what the guest sent beyond that reaches the daemon.
	nflogCopyRange = 64
)

// nflogPacket is one record as the kernel hands it over.
type nflogPacket struct {
	prefix  string
	indev   uint32 // ifindex, 0 if absent
	payload []byte // the first nflogCopyRange bytes of the IPv4 packet
}

// nflogConn is a netlink socket bound to one NFLOG group.
type nflogConn struct {
	fd  int
	seq uint32
}

// openNFLOG binds a socket to group, asking for the start of each packet.
// Only one socket can hold a group: a second daemon, or ulogd on the same
// group, makes this fail.
func openNFLOG(group uint16) (*nflogConn, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_NETFILTER)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	c := &nflogConn{fd: fd}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		c.close()
		return nil, fmt.Errorf("netlink bind: %w", err)
	}
	// Room for bursts; past it the kernel drops records and recv says
	// ENOBUFS, which is counted, not fatal. Best-effort: the default works.
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 1<<20)
	// A timeout on recv lets the reader notice it was told to stop: closing
	// a socket another goroutine is blocked on does not wake it up.
	tv := unix.NsecToTimeval(time.Second.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		c.close()
		return nil, fmt.Errorf("netlink recv timeout: %w", err)
	}

	mode := make([]byte, 6) // struct nfulnl_msg_config_mode
	binary.BigEndian.PutUint32(mode, nflogCopyRange)
	mode[4] = nfulnlCopyPacket
	for _, attrs := range [][]byte{
		nlAttr(nfulaCfgCmd, []byte{nfulnlCfgCmdBind}),
		nlAttr(nfulaCfgMode, mode),
	} {
		if err := c.config(group, attrs); err != nil {
			c.close()
			return nil, fmt.Errorf("binding NFLOG group %d: %w", group, err)
		}
	}
	return c, nil
}

// config sends one NFULNL_MSG_CONFIG for group and waits for its ack.
func (c *nflogConn) config(group uint16, attrs []byte) error {
	c.seq++
	msg := nfnlMessage(nfnlSubsysULOG<<8|nfulnlMsgConfig, unix.NLM_F_REQUEST|unix.NLM_F_ACK, c.seq, unix.AF_UNSPEC, group, attrs)
	if err := unix.Sendto(c.fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	for {
		n, _, err := unix.Recvfrom(c.fd, buf, 0)
		if err != nil {
			return err
		}
		for _, m := range splitNetlink(buf[:n]) {
			if m.typ != unix.NLMSG_ERROR || m.seq != c.seq {
				continue
			}
			if len(m.data) < 4 {
				return errors.New("short netlink ack")
			}
			if code := int32(binary.NativeEndian.Uint32(m.data)); code != 0 {
				return unix.Errno(-code)
			}
			return nil
		}
	}
}

// read waits for the next batch of records. errNFLOGTimeout means none came
// within the receive timeout; unix.ENOBUFS that the kernel dropped some.
func (c *nflogConn) read(buf []byte) ([]nflogPacket, error) {
	n, _, err := unix.Recvfrom(c.fd, buf, 0)
	if err != nil {
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			return nil, errNFLOGTimeout
		}
		return nil, err
	}
	var pkts []nflogPacket
	for _, m := range splitNetlink(buf[:n]) {
		if m.typ != nfnlSubsysULOG<<8|nfulnlMsgPacket {
			continue
		}
		if p, ok := parseNFLOGPacket(m.data); ok {
			pkts = append(pkts, p)
		}
	}
	return pkts, nil
}

var errNFLOGTimeout = errors.New("no record within the timeout")

func (c *nflogConn) close() { _ = unix.Close(c.fd) }

// nlMessage is one netlink message: its header fields and what follows it.
type nlMessage struct {
	typ  uint16
	seq  uint32
	data []byte
}

// splitNetlink cuts a datagram into its messages. A malformed length ends
// the walk: what came before it is still returned.
func splitNetlink(b []byte) []nlMessage {
	var out []nlMessage
	for len(b) >= unix.NLMSG_HDRLEN {
		l := int(binary.NativeEndian.Uint32(b[0:4]))
		if l < unix.NLMSG_HDRLEN || l > len(b) {
			break
		}
		out = append(out, nlMessage{
			typ:  binary.NativeEndian.Uint16(b[4:6]),
			seq:  binary.NativeEndian.Uint32(b[8:12]),
			data: b[unix.NLMSG_HDRLEN:l],
		})
		b = b[min(nlAlign(l), len(b)):]
	}
	return out
}

// parseNFLOGPacket reads one NFULNL_MSG_PACKET: the nfgenmsg header, then
// its attributes. Only the three the flow log uses are kept.
func parseNFLOGPacket(b []byte) (nflogPacket, bool) {
	const nfgenmsgLen = 4
	if len(b) < nfgenmsgLen {
		return nflogPacket{}, false
	}
	var p nflogPacket
	b = b[nfgenmsgLen:]
	for len(b) >= 4 {
		l := int(binary.NativeEndian.Uint16(b[0:2]))
		if l < 4 || l > len(b) {
			break
		}
		v := b[4:l]
		switch binary.NativeEndian.Uint16(b[2:4]) & nlaTypeMask {
		case nfulaPrefix:
			p.prefix, _, _ = strings.Cut(string(v), "\x00")
		case nfulaIfindexIndev:
			if len(v) == 4 {
				p.indev = binary.BigEndian.Uint32(v)
			}
		case nfulaPayload:
			p.payload = v
		}
		b = b[min(nlAlign(l), len(b)):]
	}
	return p, p.payload != nil
}

// ipv4Summary is what the flow log keeps of a packet.
type ipv4Summary struct {
	proto   string
	src     string
	dst     string
	dstPort int
}

// parseIPv4 reads the header of an IPv4 packet and, for tcp and udp, the
// destination port — not on a non-first fragment, which carries no port.
func parseIPv4(b []byte) (ipv4Summary, bool) {
	if len(b) < 20 || b[0]>>4 != 4 {
		return ipv4Summary{}, false
	}
	ihl := int(b[0]&0x0f) * 4
	if ihl < 20 || ihl > len(b) {
		return ipv4Summary{}, false
	}
	s := ipv4Summary{
		src: net.IP(b[12:16]).String(),
		dst: net.IP(b[16:20]).String(),
	}
	firstFragment := binary.BigEndian.Uint16(b[6:8])&0x1fff == 0
	switch b[9] {
	case unix.IPPROTO_TCP:
		s.proto = "tcp"
	case unix.IPPROTO_UDP:
		s.proto = "udp"
	case unix.IPPROTO_ICMP:
		s.proto = "icmp"
	default:
		s.proto = strconv.Itoa(int(b[9]))
	}
	if (s.proto == "tcp" || s.proto == "udp") && firstFragment && len(b) >= ihl+4 {
		s.dstPort = int(binary.BigEndian.Uint16(b[ihl+2 : ihl+4]))
	}
	return s, true
}

// nfnlMessage builds a netfilter netlink message: header, nfgenmsg (family,
// version 0, res_id in network order), attributes.
func nfnlMessage(typ uint16, flags uint16, seq uint32, family uint8, resID uint16, attrs []byte) []byte {
	b := make([]byte, unix.NLMSG_HDRLEN+4, unix.NLMSG_HDRLEN+4+len(attrs))
	binary.NativeEndian.PutUint16(b[4:6], typ)
	binary.NativeEndian.PutUint16(b[6:8], flags)
	binary.NativeEndian.PutUint32(b[8:12], seq)
	b[unix.NLMSG_HDRLEN] = family
	binary.BigEndian.PutUint16(b[unix.NLMSG_HDRLEN+2:], resID)
	b = append(b, attrs...)
	binary.NativeEndian.PutUint32(b[0:4], uint32(len(b)))
	return b
}

// nlAttr encodes one netlink attribute, padded to 4 bytes.
func nlAttr(typ uint16, v []byte) []byte {
	b := make([]byte, nlAlign(4+len(v)))
	binary.NativeEndian.PutUint16(b[0:2], uint16(4+len(v)))
	binary.NativeEndian.PutUint16(b[2:4], typ)
	copy(b[4:], v)
	return b
}

func nlAlign(n int) int { return (n + 3) &^ 3 }
