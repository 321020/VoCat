package modem

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	quectelVendorID = "2c7c"
	sierraVendorID  = "1199"
)

type SysFSDiscoverer struct {
	SysRoot string
	DevRoot string
}

func NewSysFSDiscoverer(sysRoot, devRoot string) *SysFSDiscoverer {
	return &SysFSDiscoverer{
		SysRoot: filepath.Clean(sysRoot),
		DevRoot: filepath.Clean(devRoot),
	}
}

type discoveredUSBDevice struct {
	candidate Candidate
	ports     map[string]Port
}

func (d *SysFSDiscoverer) Discover(ctx context.Context) ([]Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	usbRoot := filepath.Join(d.SysRoot, "bus", "usb", "devices")
	entries, err := os.ReadDir(usbRoot)
	if err != nil {
		if os.IsNotExist(err) {
			entries = nil
		} else {
			return nil, fmt.Errorf("discover Quectel USB devices: %w", err)
		}
	}

	aliases := readSerialAliases(filepath.Join(d.DevRoot, "serial", "by-id"))
	// EM7430 PID 9077 is missing from several distro qcserial/option ID tables.
	// On a real host, register that exact ID and then repair the MBIM binding.
	// A dynamic option ID also matches interfaces 12/13 after a later USB reset,
	// even when cdc_mbim was loaded first, so ordering alone is not sufficient.
	d.prepareSierraEM7430(ctx, usbRoot, entries)
	devices := make(map[string]*discoveredUSBDevice)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		interfaceNumber, ok := parseUSBInterfaceName(entry.Name())
		if !ok {
			continue
		}
		interfacePath := filepath.Join(usbRoot, entry.Name())
		resolvedInterface, err := filepath.EvalSymlinks(interfacePath)
		if err != nil {
			resolvedInterface = interfacePath
		}
		if value, err := readHexByte(filepath.Join(resolvedInterface, "bInterfaceNumber")); err == nil {
			interfaceNumber = value
		}

		deviceName := strings.SplitN(entry.Name(), ":", 2)[0]
		devicePath := filepath.Join(usbRoot, deviceName)
		resolvedDevice, err := filepath.EvalSymlinks(devicePath)
		if err != nil {
			resolvedDevice = devicePath
		}
		vendorID := strings.ToLower(readTrimmed(filepath.Join(resolvedDevice, "idVendor")))
		productID := strings.ToLower(readTrimmed(filepath.Join(resolvedDevice, "idProduct")))
		hardwareKind, idPrefix, supported := supportedUSBModem(vendorID, productID)
		if !supported {
			continue
		}

		state := devices[deviceName]
		if state == nil {
			serialNumber := readTrimmed(filepath.Join(resolvedDevice, "serial"))
			state = &discoveredUSBDevice{
				candidate: Candidate{
					HardwareKind: hardwareKind,
					ID:           candidateID(idPrefix, productID, serialNumber, deviceName),
					VendorID:     vendorID,
					ProductID:    productID,
					Manufacturer: readTrimmed(filepath.Join(resolvedDevice, "manufacturer")),
					Product:      readTrimmed(filepath.Join(resolvedDevice, "product")),
					SerialNumber: serialNumber,
					USBPath:      devicePath,
				},
				ports: make(map[string]Port),
			}
			devices[deviceName] = state
		}

		ttyNames, qmiControls, networkInterfaces := scanUSBInterface(resolvedInterface)
		for _, name := range ttyNames {
			if !strings.HasPrefix(name, "ttyUSB") && !strings.HasPrefix(name, "ttyACM") {
				continue
			}
			path := filepath.Join(d.DevRoot, name)
			state.ports[name] = Port{
				Path:            path,
				StablePath:      aliases[name],
				Name:            name,
				InterfaceNumber: interfaceNumber,
				Role:            usbPortRole(vendorID, interfaceNumber, name),
			}
		}
		if protocol := usbControlProtocol(resolvedInterface); protocol != "" &&
			(state.candidate.ControlProtocol == "" || protocol == "mbim") {
			state.candidate.ControlProtocol = protocol
		}
		if state.candidate.QMIControl == "" && len(qmiControls) > 0 {
			state.candidate.QMIControl = filepath.Join(d.DevRoot, qmiControls[0])
		}
		if state.candidate.NetworkInterface == "" && len(networkInterfaces) > 0 {
			state.candidate.NetworkInterface = networkInterfaces[0]
		}
	}

	result := make([]Candidate, 0, len(devices))
	for _, state := range devices {
		state.candidate.Ports = make([]Port, 0, len(state.ports))
		for _, port := range state.ports {
			state.candidate.Ports = append(state.candidate.Ports, port)
		}
		sort.Slice(state.candidate.Ports, func(i, j int) bool {
			left, right := state.candidate.Ports[i], state.candidate.Ports[j]
			if left.InterfaceNumber != right.InterfaceNumber {
				return left.InterfaceNumber < right.InterfaceNumber
			}
			return left.Name < right.Name
		})
		if state.candidate.VendorID == sierraVendorID {
			assignSierraPortRoles(state.candidate.Ports)
		} else {
			assignQuectelPortRoles(state.candidate.Ports)
		}
		state.candidate.ATPort = selectATPort(state.candidate.Ports)
		if state.candidate.VendorID == sierraVendorID && !state.candidate.HasATPort() {
			state.candidate.DiscoveryIssue = "sierra_serial_driver_missing"
		}
		result = append(result, state.candidate)
	}
	wwanCandidates, err := d.discoverWWAN(ctx)
	if err != nil {
		return nil, err
	}
	result = append(result, wwanCandidates...)
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

type discoveredWWANDevice struct {
	index    string
	ports    []Port
	qmiNames []string
	sysPath  string
}

// discoverWWAN covers PCIe/MHI modems exposed through Linux's wwan subsystem,
// for example /dev/wwan0at0 and /dev/wwan0qmi0. These devices do not appear on
// the USB bus and therefore need a separate discovery path.
func (d *SysFSDiscoverer) discoverWWAN(ctx context.Context) ([]Candidate, error) {
	classRoot := filepath.Join(d.SysRoot, "class", "wwan")
	classEntries, err := os.ReadDir(classRoot)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("discover PCIe/MHI WWAN devices: %w", err)
		}
		classEntries = nil
	}

	// Normal kernels expose these ports in /sys/class/wwan. Also inspect /dev
	// because some downstream MHI packages create the character devices but do
	// not populate the class directory in the host namespace/container.
	portNames := make(map[string]struct{})
	for _, entry := range classEntries {
		portNames[entry.Name()] = struct{}{}
	}
	if devEntries, devErr := os.ReadDir(d.DevRoot); devErr == nil {
		for _, entry := range devEntries {
			if _, _, _, ok := parseWWANPortName(entry.Name()); ok {
				portNames[entry.Name()] = struct{}{}
			}
		}
	} else if !os.IsNotExist(devErr) {
		return nil, fmt.Errorf("inspect WWAN device nodes: %w", devErr)
	}
	names := make([]string, 0, len(portNames))
	for name := range portNames {
		names = append(names, name)
	}
	sort.Strings(names)

	groups := make(map[string]*discoveredWWANDevice)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		index, kind, portIndex, ok := parseWWANPortName(name)
		if !ok {
			continue
		}
		group := groups[index]
		if group == nil {
			group = &discoveredWWANDevice{index: index}
			groups[index] = group
		}
		classPath := filepath.Join(classRoot, name)
		if resolved, resolveErr := filepath.EvalSymlinks(classPath); resolveErr == nil {
			group.sysPath = filepath.Dir(resolved)
		}
		switch kind {
		case "at":
			group.ports = append(group.ports, Port{
				Path: filepath.Join(d.DevRoot, name), Name: name,
				InterfaceNumber: portIndex, Role: PortRoleAT,
			})
		case "qmi":
			group.qmiNames = append(group.qmiNames, name)
		}
	}
	result := make([]Candidate, 0, len(groups))
	for _, group := range groups {
		sort.Slice(group.ports, func(i, j int) bool {
			return group.ports[i].InterfaceNumber < group.ports[j].InterfaceNumber
		})
		sort.Strings(group.qmiNames)
		if len(group.ports) == 0 && len(group.qmiNames) == 0 {
			continue
		}
		if group.sysPath == "" {
			group.sysPath = filepath.Join(classRoot, "wwan"+group.index)
		}
		vendorID, productID := readPCIIdentity(group.sysPath, d.SysRoot)
		manufacturer := ""
		if vendorID == "17cb" {
			manufacturer = "Qualcomm"
		}
		candidate := Candidate{
			HardwareKind: "wwan", ID: "mhi-wwan" + group.index,
			VendorID: vendorID, ProductID: productID, Manufacturer: manufacturer,
			Product: "PCIe/MHI WWAN modem", USBPath: group.sysPath,
			Ports: group.ports, NetworkInterface: selectWWANNetworkInterface(d.SysRoot, group.index),
		}
		if len(group.ports) > 0 {
			candidate.ATPort = group.ports[0]
		}
		if len(group.qmiNames) > 0 {
			candidate.QMIControl = filepath.Join(d.DevRoot, group.qmiNames[0])
			candidate.ControlProtocol = "qmi"
		}
		result = append(result, candidate)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func parseWWANPortName(name string) (index, kind string, portIndex int, ok bool) {
	if !strings.HasPrefix(name, "wwan") {
		return "", "", 0, false
	}
	rest := strings.TrimPrefix(name, "wwan")
	cut := 0
	for cut < len(rest) && rest[cut] >= '0' && rest[cut] <= '9' {
		cut++
	}
	if cut == 0 {
		return "", "", 0, false
	}
	index, rest = rest[:cut], rest[cut:]
	for _, candidateKind := range []string{"at", "qmi"} {
		if !strings.HasPrefix(rest, candidateKind) {
			continue
		}
		numberText := strings.TrimPrefix(rest, candidateKind)
		number, err := strconv.Atoi(numberText)
		if err != nil || number < 0 {
			return "", "", 0, false
		}
		return index, candidateKind, number, true
	}
	return "", "", 0, false
}

func selectWWANNetworkInterface(sysRoot, index string) string {
	exact := "wwan" + index
	if _, err := os.Stat(filepath.Join(sysRoot, "class", "net", exact)); err == nil {
		return exact
	}
	entries, _ := os.ReadDir(filepath.Join(sysRoot, "class", "net"))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), exact) {
			return entry.Name()
		}
	}
	return ""
}

func readPCIIdentity(path, sysRoot string) (vendorID, productID string) {
	root := filepath.Clean(sysRoot)
	for current := filepath.Clean(path); current != "." && current != string(filepath.Separator); current = filepath.Dir(current) {
		vendor := strings.TrimPrefix(strings.ToLower(readTrimmed(filepath.Join(current, "vendor"))), "0x")
		device := strings.TrimPrefix(strings.ToLower(readTrimmed(filepath.Join(current, "device"))), "0x")
		if vendor != "" && device != "" {
			return vendor, device
		}
		if current == root {
			break
		}
	}
	return "", ""
}

func parseUSBInterfaceName(name string) (int, bool) {
	_, suffix, ok := strings.Cut(name, ":")
	if !ok {
		return 0, false
	}
	_, numberText, ok := strings.Cut(suffix, ".")
	if !ok || numberText == "" {
		return 0, false
	}
	number, err := strconv.ParseInt(numberText, 10, 32)
	return int(number), err == nil
}

func readHexByte(path string) (int, error) {
	value := readTrimmed(path)
	number, err := strconv.ParseUint(value, 16, 8)
	return int(number), err
}

func readTrimmed(path string) string {
	value, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(value))
}

func scanUSBInterface(root string) (ttyNames, qmiControls, networkInterfaces []string) {
	ttySeen := make(map[string]struct{})
	qmiSeen := make(map[string]struct{})
	netSeen := make(map[string]struct{})
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := entry.Name()
		switch {
		case entry.IsDir() && (strings.HasPrefix(name, "ttyUSB") || strings.HasPrefix(name, "ttyACM")):
			ttySeen[name] = struct{}{}
		case strings.HasPrefix(name, "cdc-wdm"):
			qmiSeen[name] = struct{}{}
		case entry.IsDir() && filepath.Base(filepath.Dir(path)) == "net":
			netSeen[name] = struct{}{}
		}
		return nil
	})
	for name := range ttySeen {
		ttyNames = append(ttyNames, name)
	}
	for name := range qmiSeen {
		qmiControls = append(qmiControls, name)
	}
	for name := range netSeen {
		networkInterfaces = append(networkInterfaces, name)
	}
	sort.Strings(ttyNames)
	sort.Strings(qmiControls)
	sort.Strings(networkInterfaces)
	return
}

func readSerialAliases(root string) map[string]string {
	result := make(map[string]string)
	entries, err := os.ReadDir(root)
	if err != nil {
		return result
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		target, err := os.Readlink(path)
		if err != nil {
			continue
		}
		name := filepath.Base(filepath.Clean(target))
		if strings.HasPrefix(name, "ttyUSB") || strings.HasPrefix(name, "ttyACM") {
			if existing := result[name]; existing == "" || path < existing {
				result[name] = path
			}
		}
	}
	return result
}

func candidateID(prefix, productID, serialNumber, usbName string) string {
	serialNumber = strings.TrimSpace(serialNumber)
	if serialNumber != "" && !strings.EqualFold(serialNumber, "android") {
		// A surprising number of EC20/EC25 carrier boards expose the same
		// factory/default USB serial number.  The device manager is keyed by this
		// value, so using the serial alone silently collapsed two modems connected
		// to the same hub into one entry.  Include the physical USB topology in the
		// discovery key; configured devices remain stable through ATMapper's
		// USB-path/IMEI matching even when Linux renumbers ttyUSB nodes.
		return prefix + "-" + sanitizeID(serialNumber+"-"+usbName)
	}
	return prefix + "-" + sanitizeID(productID+"-"+usbName)
}

func supportedUSBModem(vendorID, productID string) (hardwareKind, idPrefix string, ok bool) {
	switch strings.ToLower(vendorID) {
	case quectelVendorID:
		return "usb", "quectel", true
	case sierraVendorID:
		switch strings.ToLower(productID) {
		case "9077", "9078", "9079", "907a", "907b":
			return "sierra_usb", "sierra", true
		}
	}
	return "", "", false
}

func usbPortRole(vendorID string, interfaceNumber int, name string) PortRole {
	if vendorID == sierraVendorID {
		switch interfaceNumber {
		case 0:
			return PortRoleDiagnostic
		case 2:
			return PortRoleNMEA
		case 3:
			return PortRoleAT
		}
		return PortRoleUnknown
	}
	return quecPortRole(interfaceNumber, name)
}

func assignSierraPortRoles(ports []Port) {
	for index := range ports {
		ports[index].Role = usbPortRole(sierraVendorID, ports[index].InterfaceNumber, ports[index].Name)
	}
}

func usbControlProtocol(interfacePath string) string {
	if target, err := filepath.EvalSymlinks(filepath.Join(interfacePath, "driver")); err == nil {
		switch filepath.Base(target) {
		case "cdc_mbim":
			return "mbim"
		case "qmi_wwan":
			return "qmi"
		}
	}
	class := strings.ToLower(readTrimmed(filepath.Join(interfacePath, "bInterfaceClass")))
	subclass := strings.ToLower(readTrimmed(filepath.Join(interfacePath, "bInterfaceSubClass")))
	if class == "02" && subclass == "0e" {
		return "mbim"
	}
	return ""
}

func (d *SysFSDiscoverer) prepareSierraEM7430(ctx context.Context, usbRoot string, entries []os.DirEntry) {
	if filepath.Clean(d.SysRoot) != filepath.Clean("/sys") || filepath.Clean(d.DevRoot) != filepath.Clean("/dev") {
		return
	}
	found := false
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ":") {
			continue
		}
		path := filepath.Join(usbRoot, entry.Name())
		if strings.EqualFold(readTrimmed(filepath.Join(path, "idVendor")), sierraVendorID) &&
			strings.EqualFold(readTrimmed(filepath.Join(path, "idProduct")), "9077") {
			found = true
			break
		}
	}
	if !found {
		return
	}
	// Ignore errors here: discovery will still return a degraded candidate with
	// a precise remediation code instead of hiding the hardware completely.
	_ = exec.CommandContext(ctx, "modprobe", "cdc_mbim").Run()
	_ = exec.CommandContext(ctx, "modprobe", "option").Run()
	_ = os.WriteFile(
		filepath.Join(d.SysRoot, "bus", "usb-serial", "drivers", "option1", "new_id"),
		[]byte(sierraVendorID+" 9077\n"),
		0o200,
	)
	d.repairSierraEM7430MBIM(usbRoot, entries)
}

func (d *SysFSDiscoverer) repairSierraEM7430MBIM(usbRoot string, entries []os.DirEntry) {
	devices := make(map[string]struct{})
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ":") {
			continue
		}
		path := filepath.Join(usbRoot, entry.Name())
		if strings.EqualFold(readTrimmed(filepath.Join(path, "idVendor")), sierraVendorID) &&
			strings.EqualFold(readTrimmed(filepath.Join(path, "idProduct")), "9077") {
			devices[entry.Name()] = struct{}{}
		}
	}

	var controlsToBind []string
	for _, entry := range entries {
		deviceName, _, ok := strings.Cut(entry.Name(), ":")
		if !ok {
			continue
		}
		if _, ok := devices[deviceName]; !ok {
			continue
		}
		interfacePath := filepath.Join(usbRoot, entry.Name())
		class := strings.ToLower(readTrimmed(filepath.Join(interfacePath, "bInterfaceClass")))
		subclass := strings.ToLower(readTrimmed(filepath.Join(interfacePath, "bInterfaceSubClass")))
		protocol := strings.ToLower(readTrimmed(filepath.Join(interfacePath, "bInterfaceProtocol")))
		isControl := class == "02" && subclass == "0e"
		isData := class == "0a" && protocol == "02"
		if !isControl && !isData {
			continue
		}

		driverPath, err := filepath.EvalSymlinks(filepath.Join(interfacePath, "driver"))
		if err == nil && filepath.Base(driverPath) == "cdc_mbim" {
			continue
		}
		if isControl {
			controlsToBind = append(controlsToBind, entry.Name())
		}
		if err == nil && filepath.Base(driverPath) == "option" {
			_ = os.WriteFile(filepath.Join(driverPath, "unbind"), []byte(entry.Name()), 0o200)
		}
	}

	bindPath := filepath.Join(d.SysRoot, "bus", "usb", "drivers", "cdc_mbim", "bind")
	for _, interfaceName := range controlsToBind {
		// Binding the MBIM control interface also claims its paired CDC data
		// interface. Errors are intentionally non-fatal so discovery can return a
		// degraded device with actionable details on kernels without cdc_mbim.
		_ = os.WriteFile(bindPath, []byte(interfaceName), 0o200)
	}
}

func sanitizeID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var result strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' ||
			character == '-' || character == '_' {
			result.WriteRune(character)
		} else {
			result.WriteByte('-')
		}
	}
	return strings.Trim(result.String(), "-")
}

func assignQuectelPortRoles(ports []Port) {
	// ttyUSB numbers are allocated globally by Linux. A second modem therefore
	// commonly exposes ttyUSB4..ttyUSB7, so absolute tty names cannot identify
	// the logical AT port. Infer the Quectel composition once per physical USB
	// device and assign roles from that device's interface numbers.
	base := 0x02
	for _, port := range ports {
		if port.InterfaceNumber <= 0x01 {
			base = 0x00
			break
		}
	}
	for index := range ports {
		switch ports[index].InterfaceNumber - base {
		case 0:
			ports[index].Role = PortRoleDiagnostic
		case 1:
			ports[index].Role = PortRoleNMEA
		case 2:
			ports[index].Role = PortRoleAT
		case 3:
			ports[index].Role = PortRoleModem
		default:
			ports[index].Role = PortRoleUnknown
		}
	}
}

func quecPortRole(interfaceNumber int, name string) PortRole {
	// Initial best effort. assignQuectelPortRoles replaces this once every
	// interface belonging to the same physical modem has been collected.
	switch interfaceNumber {
	case 0x00:
		return PortRoleDiagnostic
	case 0x01:
		return PortRoleNMEA
	case 0x02:
		return PortRoleAT
	case 0x03:
		return PortRoleModem
	default:
		if name == "ttyUSB2" {
			return PortRoleAT
		}
		return PortRoleUnknown
	}
}

func selectATPort(ports []Port) Port {
	var best Port
	bestScore := 0
	for _, port := range ports {
		score := 0
		switch {
		case port.Role == PortRoleAT:
			score = 120
		case port.Name == "ttyUSB2":
			score = 100
		case port.InterfaceNumber == 0x04:
			score = 90
		case port.InterfaceNumber == 0x05:
			score = 40
		case port.Role == PortRoleModem:
			score = 30
		}
		if score > bestScore {
			best, bestScore = port, score
		}
	}
	if bestScore <= 0 {
		return Port{}
	}
	return best
}

type unsupportedDiscoverer struct{}

func (unsupportedDiscoverer) Discover(context.Context) ([]Candidate, error) {
	return nil, ErrUnsupportedPlatform
}
