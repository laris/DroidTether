package usb

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/gousb"
	"github.com/rs/zerolog/log"
)

// Diagnostics for the idle state. Upstream logged nothing at info level while
// waiting, so "no phone", "phone not tethering" and "phone hidden from libusb"
// all looked identical: a single "Starting DroidTether..." line forever.
// Nothing here changes which devices are claimed; MatchRNDIS still decides that.

const (
	diagHeartbeat   = 60 * time.Second // repeat an unchanged waiting state this often
	diagIoregEvery  = 10 * time.Second // cross-check with IOKit at most this often
	diagClaimRepeat = 30 * time.Second // repeat an identical claim failure this often
)

// Extra Android vendor IDs used ONLY to recognise phones in diagnostics.
var diagAndroidVIDs = []uint16{
	0x2d95, // vivo
	0x339b, // Honor
	0x19d2, // ZTE / nubia
	0x1004, // LG
	0x0bb4, // HTC
	0x0b05, // ASUS
	0x2a45, // Meizu
}

func isAndroidVID(vid uint16) bool {
	for _, v := range knownAndroidVIDs {
		if v == vid {
			return true
		}
	}
	for _, v := range diagAndroidVIDs {
		if v == vid {
			return true
		}
	}
	return false
}

// hasRNDISControl reports a standard RNDIS control interface: E0/01/03, or the
// CDC-ACM/vendor form 02/02/FF that some gadgets use.
func hasRNDISControl(class, sub, proto uint8) bool {
	return (class == RNDISClass && sub == RNDISSubClass && proto == RNDISProtocol) ||
		(class == 0x02 && sub == 0x02 && proto == 0xFF)
}

// seenDevice is a libusb-visible device worth mentioning while waiting.
type seenDevice struct {
	VID, PID uint16
	RNDIS    bool   // exposes a standard RNDIS control interface
	Intfs    string // interface classes of the lowest-numbered configuration
}

func (s seenDevice) id() string { return fmt.Sprintf("%04x:%04x", s.VID, s.PID) }

// describeDesc returns the device summary and whether it is worth reporting.
func describeDesc(desc *gousb.DeviceDesc) (seenDevice, bool) {
	sd := seenDevice{VID: uint16(desc.Vendor), PID: uint16(desc.Product)}
	cfgNums := make([]int, 0, len(desc.Configs))
	for n := range desc.Configs {
		cfgNums = append(cfgNums, n)
	}
	sort.Ints(cfgNums)
	for i, n := range cfgNums {
		for _, intf := range desc.Configs[n].Interfaces {
			for _, alt := range intf.AltSettings {
				c, s, p := uint8(alt.Class), uint8(alt.SubClass), uint8(alt.Protocol)
				if hasRNDISControl(c, s, p) {
					sd.RNDIS = true
				}
				if i == 0 && alt.Alternate == 0 {
					sd.Intfs += fmt.Sprintf(" %d:%02x/%02x/%02x", alt.Number, c, s, p)
				}
			}
		}
	}
	sd.Intfs = strings.TrimSpace(sd.Intfs)
	return sd, sd.RNDIS || isAndroidVID(sd.VID)
}

// ioregDevice is one IOUSBHostDevice node from `ioreg -p IOUSB -l`.
type ioregDevice struct {
	Name, Vendor, Product, Owner string
	VID, PID                     uint16
	NeedsEntitlement             bool
}

func (d ioregDevice) id() string { return fmt.Sprintf("%04x:%04x", d.VID, d.PID) }

func (d ioregDevice) label() string {
	name := strings.TrimSpace(d.Vendor + " " + d.Product)
	if name == "" {
		name = d.Name
	}
	return fmt.Sprintf("%s (%s)", d.id(), name)
}

// ioregUSBDevices lists the USB devices macOS itself sees, independent of libusb.
func ioregUSBDevices() ([]ioregDevice, error) {
	out, err := exec.Command("/usr/sbin/ioreg", "-p", "IOUSB", "-l", "-w0").Output()
	if err != nil {
		return nil, err
	}
	var devs []ioregDevice
	var cur *ioregDevice
	flush := func() {
		if cur != nil && (cur.VID != 0 || cur.PID != 0) {
			devs = append(devs, *cur)
		}
		cur = nil
	}
	quoted := func(line string) string {
		if i := strings.Index(line, "= "); i >= 0 {
			return strings.Trim(strings.TrimSpace(line[i+2:]), `"`)
		}
		return ""
	}
	number := func(line string) uint16 {
		v, _ := strconv.ParseUint(quoted(line), 10, 16)
		return uint16(v)
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "-o "); i >= 0 && strings.Contains(line, "<class ") {
			flush()
			name := line[i+3:]
			if j := strings.IndexAny(name, "@<"); j >= 0 {
				name = name[:j]
			}
			cur = &ioregDevice{Name: strings.TrimSpace(name)}
			continue
		}
		if cur == nil {
			continue
		}
		switch {
		case strings.Contains(line, `"idVendor" = `):
			cur.VID = number(line)
		case strings.Contains(line, `"idProduct" = `):
			cur.PID = number(line)
		case strings.Contains(line, `"USB Vendor Name" = `):
			cur.Vendor = quoted(line)
		case strings.Contains(line, `"USB Product Name" = `):
			cur.Product = quoted(line)
		case strings.Contains(line, `"UsbExclusiveOwner" = `):
			cur.Owner = quoted(line)
		case strings.Contains(line, `"NeedsDeviceAccessEntitlement" = Yes`):
			cur.NeedsEntitlement = true
		}
	}
	flush()
	return devs, sc.Err()
}

func (d ioregDevice) looksAndroid() bool {
	return isAndroidVID(d.VID) || IsKnownAndroidManufacturer(d.Vendor+" "+d.Product)
}

// diagState tracks what was last logged so the idle loop only speaks on change.
type diagState struct {
	lastMsg    string
	lastLogged time.Time
	lastIoreg  time.Time
	ioreg      []ioregDevice
	ioregErr   error
	claimMsg   string
	claimAt    time.Time
}

func (s *diagState) refreshIoreg(force bool) {
	if !force && time.Since(s.lastIoreg) < diagIoregEvery {
		return
	}
	s.ioreg, s.ioregErr = ioregUSBDevices()
	s.lastIoreg = time.Now()
}

func (s *diagState) ioregByID(id string) (ioregDevice, bool) {
	for _, d := range s.ioreg {
		if d.id() == id {
			return d, true
		}
	}
	return ioregDevice{}, false
}

// reportWaiting is called after a scan that found nothing to claim.
func (s *diagState) reportWaiting(seen []seenDevice) {
	s.refreshIoreg(false)

	inLibusb := map[string]bool{}
	var lines []string
	for _, sd := range seen {
		inLibusb[sd.id()] = true
		label := sd.id()
		if d, ok := s.ioregByID(sd.id()); ok {
			label = d.label()
		}
		if sd.RNDIS {
			lines = append(lines, fmt.Sprintf("%s exposes RNDIS but was not matched [%s]", label, sd.Intfs))
		} else {
			lines = append(lines, fmt.Sprintf("%s is connected but USB tethering (RNDIS) is OFF [%s]: "+
				"turn on USB tethering on the phone, or run: sudo tether rndis", label, sd.Intfs))
		}
	}
	for _, d := range s.ioreg {
		if d.looksAndroid() && !inLibusb[d.id()] {
			owner := ""
			if d.Owner != "" {
				owner = ", exclusive owner: " + d.Owner
			}
			lines = append(lines, fmt.Sprintf("%s is attached to macOS (IOKit) but INVISIBLE to libusb "+
				"(NeedsDeviceAccessEntitlement=%v%s): USB access is being blocked", d.label(), d.NeedsEntitlement, owner))
		}
	}

	msg := "no Android phone on USB; plug it in and turn on USB tethering"
	if len(lines) > 0 {
		sort.Strings(lines)
		msg = strings.Join(lines, "; ")
	}
	if s.ioregErr != nil {
		msg += fmt.Sprintf(" (IOKit cross-check failed: %v)", s.ioregErr)
	}

	changed := msg != s.lastMsg
	if !changed && time.Since(s.lastLogged) < diagHeartbeat {
		return
	}
	ev := log.Info()
	if len(lines) > 0 {
		ev = log.Warn()
	}
	prefix := "Waiting for an Android phone in USB tethering mode"
	if !changed {
		prefix = "Still waiting"
	}
	ev.Str("component", "usb").Msg(prefix + ": " + msg)
	s.lastMsg, s.lastLogged = msg, time.Now()
	s.claimMsg = ""
}

// reportClaimFailure rate-limits identical claim failures and adds a likely cause.
func (s *diagState) reportClaimFailure(err error, sd seenDevice) {
	msg := err.Error()
	if msg == s.claimMsg && time.Since(s.claimAt) < diagClaimRepeat {
		return
	}
	s.refreshIoreg(true)
	ev := log.Warn().Str("component", "usb").Err(err).Str("device", sd.id()).Str("interfaces", sd.Intfs)
	if d, ok := s.ioregByID(sd.id()); ok {
		ev = ev.Str("name", strings.TrimSpace(d.Vendor+" "+d.Product))
		if d.Owner != "" {
			ev = ev.Str("exclusive_owner", d.Owner)
		}
	}
	hint := "RNDIS interface present but could not be claimed: another process may own the phone " +
		"(adb server, Android File Transfer, a second droidtether); a replug or USB reset usually clears it"
	if !sd.RNDIS {
		hint = "the phone exposes no RNDIS interface, so USB tethering is OFF (MTP/charging mode): " +
			"turn on USB tethering on the phone, or run: sudo tether rndis"
	}
	if msg == s.claimMsg {
		hint = "still failing: " + hint
	}
	ev.Str("hint", hint).Msg("Failed to initialize Android RNDIS device wrapper")
	s.claimMsg, s.claimAt = msg, time.Now()
	s.lastMsg = ""
}

// TetheringHint explains a failed RNDIS handshake when the phone exposes no
// standard RNDIS control interface: only the vendor-ID fallback matched, so
// USB tethering is off on the phone. Empty when an RNDIS interface exists.
func (d *Device) TetheringHint() string {
	if sd, _ := describeDesc(d.usbd.Desc); !sd.RNDIS {
		return "the phone exposes no RNDIS interface, so USB tethering is OFF (MTP/charging mode): " +
			"turn on USB tethering on the phone, or run: sudo tether rndis"
	}
	return ""
}
