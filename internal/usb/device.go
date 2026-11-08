package usb

import (
	"fmt"
	"github.com/google/gousb"
	"time"
)

// Device wraps a gousb.Device with RNDIS specific fields and capabilities.
type Device struct {
	usbd         *gousb.Device
	usbc         *gousb.Config
	controlIntf  *gousb.Interface
	dataIntf     *gousb.Interface
	InterfaceNum int
}

// NewDevice opens and claims the RNDIS interface on the provided gousb.Device.
func NewDevice(dev *gousb.Device) (*Device, error) {
	var targetConfigNum, targetInterfaceNum, targetAltSettingNum int = -1, -1, -1

	// 1. Scan all configuration descriptors to find the best RNDIS candidate.
	// We do this BEFORE calling dev.Config() to avoid unnecessary SET_CONFIGURATION requests.
	for _, cfgDesc := range dev.Desc.Configs {
		foundInConfig := false
		for _, intfDesc := range cfgDesc.Interfaces {
			for _, altDesc := range intfDesc.AltSettings {
				if MatchRNDIS(uint16(dev.Desc.Vendor), uint16(dev.Desc.Product), uint8(altDesc.Class), uint8(altDesc.SubClass), uint8(altDesc.Protocol)) {
					// Prioritize standard RNDIS class over vendor-specific if multiple found
					if targetConfigNum == -1 || uint8(altDesc.Class) == RNDISClass {
						targetConfigNum = cfgDesc.Number
						targetInterfaceNum = intfDesc.Number
						targetAltSettingNum = altDesc.Alternate
						foundInConfig = true
					}
				}
			}
		}
		if foundInConfig {
			break
		}
	}

	if targetConfigNum == -1 {
		return nil, fmt.Errorf("no RNDIS interface found on any device configuration")
	}

	// 2. Set AutoDetach (detaches macOS built-in driver automatically).
	dev.SetAutoDetach(true)

	// 3. Open the selected configuration.
	usbc, err := dev.Config(targetConfigNum)
	if err != nil {
		return nil, fmt.Errorf("failed to open device config %d: %w", targetConfigNum, err)
	}

	// Wait for the device to stabilize after SET_CONFIGURATION (critical for Samsung)
	time.Sleep(250 * time.Millisecond)

	// 4. Claim the control interface.
	intf, err := usbc.Interface(targetInterfaceNum, targetAltSettingNum)
	if err != nil {
		usbc.Close()
		return nil, fmt.Errorf("failed to claim RNDIS control interface %d: %w", targetInterfaceNum, err)
	}

	d := &Device{
		usbd:         dev,
		usbc:         usbc,
		controlIntf:  intf,
		InterfaceNum: targetInterfaceNum,
	}

	// 5. Proactively find and claim the Data interface.
	// Many devices (Samsung) require the data interface to be claimed before the RNDIS handshake finishes.
	// Pass 1 takes a CDC-Data (0x0A) interface; pass 2 falls back to vendor-specific
	// classes. The ADB function is skipped: it has a bulk pair but is never RNDIS data.
	for pass := 1; pass <= 2 && d.dataIntf == nil; pass++ {
		for _, intfDesc := range usbc.Desc.Interfaces {
			if intfDesc.Number == targetInterfaceNum {
				continue
			}
			for _, alt := range intfDesc.AltSettings {
				class := uint8(alt.Class)
				wanted := class == 0x0A
				if pass == 2 {
					wanted = (class == 0xFF || class == 0xEF) && !isADB(alt)
				}
				if !wanted || len(alt.Endpoints) < 2 {
					continue
				}
				fmt.Printf("USB DEBUG: Proactively claiming Data Intf %d Alt %d\n", alt.Number, alt.Alternate)
				dataIntf, err := usbc.Interface(alt.Number, alt.Alternate)
				if err != nil {
					fmt.Printf("USB DEBUG: claim of Data Intf %d Alt %d failed: %v\n", alt.Number, alt.Alternate, err)
					continue
				}
				d.dataIntf = dataIntf
				break
			}
			if d.dataIntf != nil {
				break
			}
		}
	}

	return d, nil
}

// Close releases the claimed USB interfaces and config configuration.
func (d *Device) Close() error {
	if d.dataIntf != nil {
		d.dataIntf.Close()
		d.dataIntf = nil
	}
	if d.controlIntf != nil {
		d.controlIntf.Close()
		d.controlIntf = nil
	}
	if d.usbc != nil {
		d.usbc.Close()
		d.usbc = nil
	}
	return nil
}

// Control performs a vendor-specific control transfer.
func (d *Device) Control(rType, request uint8, val, idx uint16, data []byte) (int, error) {
	return d.usbd.Control(rType, request, val, idx, data)
}

// OpenBulkEndpoints returns the bulk IN and OUT endpoints for data transfer.
func (d *Device) OpenBulkEndpoints() (in *gousb.InEndpoint, out *gousb.OutEndpoint, err error) {
	// 1. Try finding endpoints on the current (Control) interface first.
	in, out, _ = d.findEndpoints(d.controlIntf)
	if in != nil && out != nil {
		return in, out, nil
	}

	// 2. Scan for a Data interface if not already claimed
	if d.dataIntf == nil {
		for _, intfDesc := range d.usbc.Desc.Interfaces {
			if intfDesc.Number == d.InterfaceNum {
				continue
			}
			for _, alt := range intfDesc.AltSettings {
				if len(alt.Endpoints) >= 2 && !isADB(alt) {
					fmt.Printf("USB DEBUG: Try claim intf %d alt %d\n", intfDesc.Number, alt.Alternate)
					intf, err := d.usbc.Interface(intfDesc.Number, alt.Alternate)
					if err != nil {
						fmt.Printf("USB DEBUG: failed to claim intf %d alt %d: %v\n", intfDesc.Number, alt.Alternate, err)
						if alt.Alternate != 0 {
							fmt.Printf("USB DEBUG: trying fallback alt 0 for intf %d\n", intfDesc.Number)
							intf, err = d.usbc.Interface(intfDesc.Number, 0)
							if err != nil {
								fmt.Printf("USB DEBUG: fallback failed: %v\n", err)
								continue
							}
						} else {
							continue
						}
					}

					in, out, _ = d.findEndpoints(intf)
					if in != nil && out != nil {
						d.prepareDataInterface(intf)
						fmt.Printf("USB DEBUG: Found bulk endpoints on intf %d alt %d\n", intfDesc.Number, intf.Setting.Alternate)
						d.dataIntf = intf
						return in, out, nil
					} else {
						fmt.Printf("USB DEBUG: Endpoints not found after claiming.\n")
					}
					intf.Close()
				}
			}
		}
	} else {
		d.prepareDataInterface(d.dataIntf)
		return d.findEndpoints(d.dataIntf)
	}

	return nil, nil, fmt.Errorf("could not find bulk endpoint pair")
}

// prepareDataInterface makes a claimed RNDIS data interface ready for traffic:
// select alt 0 explicitly and clear any stale endpoint halt left behind by an
// earlier session.
//
// Some gadgets (e.g. vivo/MediaTek) keep their bulk endpoints disabled until
// the host issues SET_INTERFACE, and transfers to a halted endpoint NAK until
// they time out. Both requests are standard and harmless on other devices.
func (d *Device) prepareDataInterface(intf *gousb.Interface) {
	num := intf.Setting.Number
	if _, err := d.Control(0x01, 0x0B, 0, uint16(num), nil); err != nil {
		fmt.Printf("USB DEBUG: SET_INTERFACE(%d, 0) failed: %v\n", num, err)
	}
	for _, ep := range intf.Setting.Endpoints {
		if _, err := d.Control(0x02, 0x01, 0, uint16(ep.Address), nil); err != nil {
			fmt.Printf("USB DEBUG: CLEAR_FEATURE(halt) ep 0x%02x failed: %v\n", ep.Address, err)
		}
	}
}

func (d *Device) findEndpoints(intf *gousb.Interface) (*gousb.InEndpoint, *gousb.OutEndpoint, error) {
	var in *gousb.InEndpoint
	var out *gousb.OutEndpoint

	for _, ep := range intf.Setting.Endpoints {
		if ep.TransferType == gousb.TransferTypeBulk {
			if ep.Direction == gousb.EndpointDirectionIn {
				in, _ = intf.InEndpoint(ep.Number)
			} else {
				out, _ = intf.OutEndpoint(ep.Number)
			}
		}
	}
	if in == nil || out == nil {
		return nil, nil, fmt.Errorf("missing endpoints")
	}
	return in, out, nil
}

// isADB reports the Android Debug Bridge function (class 0xFF, subclass 0x42, protocol 0x01).
func isADB(alt gousb.InterfaceSetting) bool {
	return uint8(alt.Class) == 0xFF && uint8(alt.SubClass) == 0x42 && uint8(alt.Protocol) == 0x01
}
