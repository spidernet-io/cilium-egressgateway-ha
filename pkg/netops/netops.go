// Package netops manages egress IPs on a network interface: it adds and
// removes /32 addresses tagged with an address label, runs duplicate address
// detection (DAD) and sends gratuitous ARP.
package netops

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/mdlayher/arp"
	"github.com/mdlayher/ethernet"
	"github.com/mdlayher/packet"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// LabelSuffix is appended to the interface name to form the address label
// (e.g. "eth0:egw"), which identifies addresses managed by this project.
const LabelSuffix = ":egw"

// ErrInterfaceNotFound is returned when the interface does not exist.
var ErrInterfaceNotFound = errors.New("interface not found")

// Label returns the address label for iface. Linux requires the label to
// start with the interface name and to be at most 15 bytes, so the suffix is
// truncated for long names.
func Label(iface string) (string, error) {
	const maxLen = unix.IFNAMSIZ - 1
	if len(iface)+2 > maxLen {
		return "", fmt.Errorf("interface name %q is too long for an address label", iface)
	}
	l := iface + LabelSuffix
	if len(l) > maxLen {
		l = l[:maxLen]
	}
	return l, nil
}

// Conflict describes a device that answered DAD for an IP.
type Conflict struct {
	IP  netip.Addr
	MAC net.HardwareAddr
}

func (c *Conflict) Error() string {
	return fmt.Sprintf("%s is answered by %s", c.IP, c.MAC)
}

// DADConfig controls duplicate address detection.
type DADConfig struct {
	// Probes is the number of ARP probes to send.
	Probes int
	// Interval is the time between probes.
	Interval time.Duration
	// Wait is how long to wait for replies after the last probe.
	Wait time.Duration
}

func DefaultDADConfig() DADConfig {
	return DADConfig{Probes: 3, Interval: 200 * time.Millisecond, Wait: 500 * time.Millisecond}
}

// Manager operates on interfaces in the network namespace of the caller.
type Manager struct {
	DAD DADConfig
}

func New() *Manager { return &Manager{DAD: DefaultDADConfig()} }

func link(iface string) (netlink.Link, error) {
	l, err := netlink.LinkByName(iface)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil, fmt.Errorf("%w: %s", ErrInterfaceNotFound, iface)
		}
		return nil, fmt.Errorf("get interface %s: %w", iface, err)
	}
	return l, nil
}

// List returns the managed IPv4 addresses on iface.
func (m *Manager) List(iface string) ([]netip.Addr, error) {
	label, err := Label(iface)
	if err != nil {
		return nil, err
	}
	l, err := link(iface)
	if err != nil {
		return nil, err
	}
	return listManaged(l, label)
}

// ListAll returns the managed IPv4 addresses on all interfaces, keyed by interface name.
func (m *Manager) ListAll() (map[string][]netip.Addr, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}
	out := map[string][]netip.Addr{}
	for _, l := range links {
		label, err := Label(l.Attrs().Name)
		if err != nil {
			continue
		}
		ips, err := listManaged(l, label)
		if err != nil {
			return nil, err
		}
		if len(ips) > 0 {
			out[l.Attrs().Name] = ips
		}
	}
	return out, nil
}

func listManaged(l netlink.Link, label string) ([]netip.Addr, error) {
	addrs, err := netlink.AddrList(l, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("list addresses on %s: %w", l.Attrs().Name, err)
	}
	var out []netip.Addr
	for _, a := range addrs {
		if a.Label != label || a.IPNet == nil {
			continue
		}
		if ones, bits := a.Mask.Size(); ones != 32 || bits != 32 {
			continue
		}
		if ip, ok := netip.AddrFromSlice(a.IP.To4()); ok {
			out = append(out, ip)
		}
	}
	return out, nil
}

func addr(ip netip.Addr, label string) *netlink.Addr {
	return &netlink.Addr{
		IPNet: &net.IPNet{IP: ip.AsSlice(), Mask: net.CIDRMask(32, 32)},
		Label: label,
	}
}

// Add adds ip/32 with the managed label to iface. Adding an existing address is a no-op.
func (m *Manager) Add(iface string, ip netip.Addr) error {
	label, err := Label(iface)
	if err != nil {
		return err
	}
	l, err := link(iface)
	if err != nil {
		return err
	}
	if err := netlink.AddrAdd(l, addr(ip, label)); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("add %s to %s: %w", ip, iface, err)
	}
	return nil
}

// Del removes the managed ip/32 from iface. Removing a missing address is a no-op.
func (m *Manager) Del(iface string, ip netip.Addr) error {
	label, err := Label(iface)
	if err != nil {
		return err
	}
	l, err := link(iface)
	if err != nil {
		if errors.Is(err, ErrInterfaceNotFound) {
			return nil
		}
		return err
	}
	if err := netlink.AddrDel(l, addr(ip, label)); err != nil && !errors.Is(err, unix.EADDRNOTAVAIL) {
		return fmt.Errorf("delete %s from %s: %w", ip, iface, err)
	}
	return nil
}

func arpConn(iface string) (*packet.Conn, *net.Interface, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrInterfaceNotFound, iface)
	}
	if len(ifi.HardwareAddr) != 6 {
		return nil, nil, fmt.Errorf("interface %s has no ethernet address", iface)
	}
	c, err := packet.Listen(ifi, packet.Raw, unix.ETH_P_ARP, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("open ARP socket on %s: %w", iface, err)
	}
	return c, ifi, nil
}

func sendARP(c *packet.Conn, ifi *net.Interface, op arp.Operation, srcIP, dstIP netip.Addr) error {
	p, err := arp.NewPacket(op, ifi.HardwareAddr, srcIP, ethernet.Broadcast, dstIP)
	if err != nil {
		return err
	}
	// The target hardware address of a request is ignored and conventionally zero.
	p.TargetHardwareAddr = make(net.HardwareAddr, 6)
	pb, err := p.MarshalBinary()
	if err != nil {
		return err
	}
	f := &ethernet.Frame{
		Destination: ethernet.Broadcast,
		Source:      ifi.HardwareAddr,
		EtherType:   ethernet.EtherTypeARP,
		Payload:     pb,
	}
	fb, err := f.MarshalBinary()
	if err != nil {
		return err
	}
	_, err = c.WriteTo(fb, &packet.Addr{HardwareAddr: ethernet.Broadcast})
	return err
}

// Detect runs duplicate address detection for ip on iface, like "arping -D".
// It returns a *Conflict error when another device uses ip.
func (m *Manager) Detect(ctx context.Context, iface string, ip netip.Addr) error {
	c, ifi, err := arpConn(iface)
	if err != nil {
		return err
	}
	defer c.Close()

	cfg := m.DAD
	if cfg.Probes < 1 {
		cfg = DefaultDADConfig()
	}
	deadline := time.Now().Add(time.Duration(cfg.Probes-1)*cfg.Interval + cfg.Wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	result := make(chan error, 1)
	go func() { result <- readConflict(c, ifi, ip, deadline) }()

	sendErr := func() error {
		for i := 0; i < cfg.Probes; i++ {
			if i > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(cfg.Interval):
				}
			}
			if err := sendARP(c, ifi, arp.OperationRequest, netip.IPv4Unspecified(), ip); err != nil {
				return fmt.Errorf("send ARP probe on %s: %w", iface, err)
			}
		}
		return nil
	}()
	if sendErr != nil {
		_ = c.SetReadDeadline(time.Now())
		<-result
		return sendErr
	}
	return <-result
}

// readConflict reads ARP packets until deadline and reports any packet sent by
// another device that claims ip as its sender address (RFC 5227 2.1.1).
func readConflict(c *packet.Conn, ifi *net.Interface, ip netip.Addr, deadline time.Time) error {
	if err := c.SetReadDeadline(deadline); err != nil {
		return err
	}
	buf := make([]byte, ifi.MTU+14)
	for {
		n, _, err := c.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || isTimeout(err) {
				return nil
			}
			return fmt.Errorf("read ARP on %s: %w", ifi.Name, err)
		}
		var f ethernet.Frame
		if err := f.UnmarshalBinary(buf[:n]); err != nil || f.EtherType != ethernet.EtherTypeARP {
			continue
		}
		var p arp.Packet
		if err := p.UnmarshalBinary(f.Payload); err != nil {
			continue
		}
		if p.SenderIP == ip && p.SenderHardwareAddr.String() != ifi.HardwareAddr.String() {
			return &Conflict{IP: ip, MAC: p.SenderHardwareAddr}
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Announce sends a gratuitous ARP request for ip on iface so that neighbors
// update their ARP caches.
func (m *Manager) Announce(iface string, ip netip.Addr) error {
	c, ifi, err := arpConn(iface)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := sendARP(c, ifi, arp.OperationRequest, ip, ip); err != nil {
		return fmt.Errorf("send gratuitous ARP for %s on %s: %w", ip, iface, err)
	}
	return nil
}
