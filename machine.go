package virtualbox

import (
	"bufio"
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// MachineState stores the last retrieved VM state.
type MachineState string

const (
	// Poweroff is a MachineState value.
	Poweroff = MachineState("poweroff")
	// Running is a MachineState value.
	Running = MachineState("running")
	// Paused is a MachineState value.
	Paused = MachineState("paused")
	// Saved is a MachineState value.
	Saved = MachineState("saved")
	// Aborted is a MachineState value.
	Aborted = MachineState("aborted")
)

// Flag is an active VM configuration toggle
type Flag int

// Flag names in lowercases to be consistent with VBoxManage options.
const (
	ACPI Flag = 1 << iota
	IOAPIC
	RTCUSEUTC
	CPUHOTPLUG
	PAE
	LONGMODE
	HPET
	HWVIRTEX
	TRIPLEFAULTRESET
	NESTEDPAGING
	LARGEPAGES
	VTXVPID
	VTXUX
	ACCELERATE3D
)

// Convert bool to "on"/"off"
func bool2string(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// Get tests if flag is set. Return "on" or "off".
func (f Flag) Get(o Flag) string {
	return bool2string(f&o == o)
}

// Machine information.
type Machine struct {
	Name       string
	UUID       string
	State      MachineState
	CPUs       uint
	Memory     uint // main memory (in MB)
	VRAM       uint // video memory (in MB)
	CfgFile    string
	BaseFolder string
	OSType     string
	Flag       Flag
	BootOrder  []string // max 4 slots, each in {none|floppy|dvd|disk|net}
	NICs       []NIC
}

// New creates a new machine.
func New() *Machine {
	return &Machine{
		BootOrder: make([]string, 0, 4),
		NICs:      make([]NIC, 0, 4),
	}
}

// Refresh reloads the machine information.
func (m *Machine) Refresh() error {
	id := m.Name
	if id == "" {
		id = m.UUID
	}
	mm, err := GetMachine(id)
	if err != nil {
		return err
	}
	*m = *mm
	return nil
}

// Start starts the machine.
func (m *Machine) Start() error {
	switch m.State {
	case Paused:
		return Manage().run("controlvm", m.Name, "resume")
	case Poweroff, Saved, Aborted:
		return Manage().run("startvm", m.Name, "--type", "headless")
	}
	return nil
}

// DisconnectSerialPort sets given serial port to disconnected.
func (m *Machine) DisconnectSerialPort(portNumber int) error {
	return Manage().run("modifyvm", m.Name, fmt.Sprintf("--uartmode%d", portNumber), "disconnected")
}

// Save suspends the machine and saves its state to disk.
func (m *Machine) Save() error {
	switch m.State {
	case Paused:
		if err := m.Start(); err != nil {
			return err
		}
	case Poweroff, Aborted, Saved:
		return nil
	}
	return Manage().run("controlvm", m.Name, "savestate")
}

// Pause pauses the execution of the machine.
func (m *Machine) Pause() error {
	switch m.State {
	case Paused, Poweroff, Aborted, Saved:
		return nil
	}
	return Manage().run("controlvm", m.Name, "pause")
}

// Stop gracefully stops the machine.
func (m *Machine) Stop() error {
	switch m.State {
	case Poweroff, Aborted, Saved:
		return nil
	case Paused:
		if err := m.Start(); err != nil {
			return err
		}
	}

	for m.State != Poweroff { // busy wait until the machine is stopped
		if err := Manage().run("controlvm", m.Name, "acpipowerbutton"); err != nil {
			return err
		}
		time.Sleep(1 * time.Second)
		if err := m.Refresh(); err != nil {
			return err
		}
	}
	return nil
}

// Poweroff forcefully stops the machine. State is lost and might corrupt the disk image.
func (m *Machine) Poweroff() error {
	switch m.State {
	case Poweroff, Aborted, Saved:
		return nil
	}
	return Manage().run("controlvm", m.Name, "poweroff")
}

// Restart gracefully restarts the machine.
func (m *Machine) Restart() error {
	switch m.State {
	case Paused, Saved:
		if err := m.Start(); err != nil {
			return err
		}
	}
	if err := m.Stop(); err != nil {
		return err
	}
	return m.Start()
}

// Reset forcefully restarts the machine. State is lost and might corrupt the disk image.
func (m *Machine) Reset() error {
	switch m.State {
	case Paused, Saved:
		if err := m.Start(); err != nil {
			return err
		}
	}
	return Manage().run("controlvm", m.Name, "reset")
}

// Delete deletes the machine and associated disk images.
func (m *Machine) Delete() error {
	if err := m.Poweroff(); err != nil {
		return err
	}
	return Manage().run("unregistervm", m.Name, "--delete")
}

// Machine returns the current machine state based on the current state.
func (m *manager) Machine(ctx context.Context, id string) (*Machine, error) {
	/* There is a strage behavior where running multiple instances of
	'VBoxManage showvminfo' on same VM simultaneously can return an error of
	'object is not ready (E_ACCESSDENIED)', so we sequential the operation with a mutex.
	Note if you are running multiple process of go-virtualbox or 'showvminfo'
	in the command line side by side, this not gonna work. */
	m.lock.Lock()
	stdout, stderr, err := m.run(ctx, "showvminfo", id, "--machinereadable")
	m.lock.Unlock()
	if err != nil {
		if reMachineNotFound.FindString(stderr) != "" {
			return nil, ErrMachineNotExist
		}
		return nil, err
	}

	/* Read all VM info into a map */
	propMap := make(map[string]string)
	s := bufio.NewScanner(strings.NewReader(stdout))
	for s.Scan() {
		res := reVMInfoLine.FindStringSubmatch(s.Text())
		if res == nil {
			continue
		}
		key := res[1]
		if key == "" {
			key = res[2]
		}
		val := res[3]
		if val == "" {
			val = res[4]
		}
		propMap[key] = val
	}

	/* Extract basic info */
	vm := New()
	vm.Name = propMap["name"]
	vm.UUID = propMap["UUID"]
	vm.State = MachineState(propMap["VMState"])
	n, err := strconv.ParseUint(propMap["memory"], 10, 32)
	if err != nil {
		return nil, err
	}
	vm.Memory = uint(n)
	n, err = strconv.ParseUint(propMap["cpus"], 10, 32)
	if err != nil {
		return nil, err
	}
	vm.CPUs = uint(n)
	n, err = strconv.ParseUint(propMap["vram"], 10, 32)
	if err != nil {
		return nil, err
	}
	vm.VRAM = uint(n)
	vm.CfgFile = propMap["CfgFile"]
	vm.BaseFolder = filepath.Dir(vm.CfgFile)

	/* Extract NIC info */
	for i := 1; i <= 4; i++ {
		var nic NIC
		nicType, ok := propMap[fmt.Sprintf("nic%d", i)]
		if !ok || nicType == "none" {
			break
		}
		nic.Network = NICNetwork(nicType)
		nic.Hardware = NICHardware(propMap[fmt.Sprintf("nictype%d", i)])
		if nic.Hardware == "" {
			return nil, fmt.Errorf("Could not find corresponding 'nictype%d'", i)
		}
		nic.MacAddr = propMap[fmt.Sprintf("macaddress%d", i)]
		if nic.MacAddr == "" {
			return nil, fmt.Errorf("Could not find corresponding 'macaddress%d'", i)
		}
		if nic.Network == NICNetHostonly {
			nic.HostInterface = propMap[fmt.Sprintf("hostonlyadapter%d", i)]
		} else if nic.Network == NICNetBridged {
			nic.HostInterface = propMap[fmt.Sprintf("bridgeadapter%d", i)]
		}
		vm.NICs = append(vm.NICs, nic)
	}

	if err := s.Err(); err != nil {
		return nil, err
	}
	return vm, nil
}

// GetMachine finds a machine by its name or UUID.
//
// Deprecated: Use Manager.Machine()
func GetMachine(id string) (*Machine, error) {
	return defaultManager.Machine(context.Background(), id)
}

// ListMachines lists all registered machines.
//
// Deprecated: Use Manager.ListMachines()
func ListMachines() ([]*Machine, error) {
	return defaultManager.ListMachines(context.Background())
}

// ListMachines lists all registered machines.
func (m *manager) ListMachines(ctx context.Context) ([]*Machine, error) {
	m.lock.Lock()
	out, _, err := m.run(ctx, "list", "vms")
	m.lock.Unlock()
	if err != nil {
		return nil, err
	}

	ms := []*Machine{}
	s := bufio.NewScanner(strings.NewReader(out))
	for s.Scan() {
		res := reVMNameUUID.FindStringSubmatch(s.Text())
		if res == nil {
			continue
		}
		m, err := m.Machine(ctx, res[1])
		if err != nil {
			// Sometimes a VM is listed but not available, so we need to handle this.
			if err == ErrMachineNotExist {
				continue
			} else {
				return nil, err
			}
		}
		ms = append(ms, m)
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return ms, nil
}

// CreateMachine creates a new machine. If basefolder is empty, use default.
func CreateMachine(name, basefolder string) (*Machine, error) {
	if name == "" {
		return nil, fmt.Errorf("machine name is empty")
	}

	// Check if a machine with the given name already exists.
	ms, err := ListMachines()
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		if m.Name == name {
			return nil, ErrMachineExist
		}
	}

	// Create and register the machine.
	args := []string{"createvm", "--name", name, "--register"}
	if basefolder != "" {
		args = append(args, "--basefolder", basefolder)
	}
	if err = Manage().run(args...); err != nil {
		return nil, err
	}

	m, err := GetMachine(name)
	if err != nil {
		return nil, err
	}

	return m, nil
}

// UpdateMachine updates the machine details based on the struct fields.
func (m *manager) UpdateMachine(ctx context.Context, vm *Machine) error {
	args := []string{"modifyvm", vm.Name,
		"--firmware", "bios",
		"--bioslogofadein", "off",
		"--bioslogofadeout", "off",
		"--bioslogodisplaytime", "0",
		"--biosbootmenu", "disabled",

		"--ostype", vm.OSType,
		"--cpus", fmt.Sprintf("%d", vm.CPUs),
		"--memory", fmt.Sprintf("%d", vm.Memory),
		"--vram", fmt.Sprintf("%d", vm.VRAM),

		"--acpi", vm.Flag.Get(ACPI),
		"--ioapic", vm.Flag.Get(IOAPIC),
		"--rtcuseutc", vm.Flag.Get(RTCUSEUTC),
		"--cpuhotplug", vm.Flag.Get(CPUHOTPLUG),
		"--pae", vm.Flag.Get(PAE),
		"--longmode", vm.Flag.Get(LONGMODE),
		"--hpet", vm.Flag.Get(HPET),
		"--hwvirtex", vm.Flag.Get(HWVIRTEX),
		"--triplefaultreset", vm.Flag.Get(TRIPLEFAULTRESET),
		"--nestedpaging", vm.Flag.Get(NESTEDPAGING),
		"--largepages", vm.Flag.Get(LARGEPAGES),
		"--vtxvpid", vm.Flag.Get(VTXVPID),
		"--vtxux", vm.Flag.Get(VTXUX),
		"--accelerate3d", vm.Flag.Get(ACCELERATE3D),
	}

	for i, dev := range vm.BootOrder {
		if i > 3 {
			break // Only four slots `--boot{1,2,3,4}`. Ignore the rest.
		}
		args = append(args, fmt.Sprintf("--boot%d", i+1), dev)
	}

	for i, nic := range vm.NICs {
		n := i + 1
		args = append(args,
			fmt.Sprintf("--nic%d", n), string(nic.Network),
			fmt.Sprintf("--nictype%d", n), string(nic.Hardware),
			fmt.Sprintf("--cableconnected%d", n), "on")
		if nic.Network == NICNetHostonly {
			args = append(args, fmt.Sprintf("--hostonlyadapter%d", n), nic.HostInterface)
		} else if nic.Network == NICNetBridged {
			args = append(args, fmt.Sprintf("--bridgeadapter%d", n), nic.HostInterface)
		}
	}

	if _, _, err := m.run(ctx, args...); err != nil {
		return err
	}
	return vm.Refresh()
}

func (m *Machine) Modify() error {
	return defaultManager.UpdateMachine(context.Background(), m)
}

// AddNATPF adds a NAT port forarding rule to the n-th NIC with the given name.
func (m *Machine) AddNATPF(n int, name string, rule PFRule) error {
	return Manage().run("controlvm", m.Name, fmt.Sprintf("natpf%d", n),
		fmt.Sprintf("%s,%s", name, rule.Format()))
}

// DelNATPF deletes the NAT port forwarding rule with the given name from the n-th NIC.
func (m *Machine) DelNATPF(n int, name string) error {
	return Manage().run("controlvm", m.Name, fmt.Sprintf("natpf%d", n), "delete", name)
}

// SetNIC set the n-th NIC.
func (m *Machine) SetNIC(n int, nic NIC) error {
	args := []string{"modifyvm", m.Name,
		fmt.Sprintf("--nic%d", n), string(nic.Network),
		fmt.Sprintf("--nictype%d", n), string(nic.Hardware),
		fmt.Sprintf("--cableconnected%d", n), "on",
	}

	if nic.Network == NICNetHostonly {
		args = append(args, fmt.Sprintf("--hostonlyadapter%d", n), nic.HostInterface)
	} else if nic.Network == NICNetBridged {
		args = append(args, fmt.Sprintf("--bridgeadapter%d", n), nic.HostInterface)
	}
	return Manage().run(args...)
}

// AddStorageCtl adds a storage controller with the given name.
func (m *Machine) AddStorageCtl(name string, ctl StorageController) error {
	args := []string{"storagectl", m.Name, "--name", name}
	if ctl.SysBus != "" {
		args = append(args, "--add", string(ctl.SysBus))
	}
	if ctl.Ports > 0 {
		args = append(args, "--portcount", fmt.Sprintf("%d", ctl.Ports))
	}
	if ctl.Chipset != "" {
		args = append(args, "--controller", string(ctl.Chipset))
	}
	args = append(args, "--hostiocache", bool2string(ctl.HostIOCache))
	args = append(args, "--bootable", bool2string(ctl.Bootable))
	return Manage().run(args...)
}

// DelStorageCtl deletes the storage controller with the given name.
func (m *Machine) DelStorageCtl(name string) error {
	return Manage().run("storagectl", m.Name, "--name", name, "--remove")
}

// AttachStorage attaches a storage medium to the named storage controller.
func (m *Machine) AttachStorage(ctlName string, medium StorageMedium) error {
	_, _, err := defaultManager.run(context.Background(),
		"storageattach", m.Name, "--storagectl", ctlName,
		"--port", fmt.Sprintf("%d", medium.Port),
		"--device", fmt.Sprintf("%d", medium.Device),
		"--type", string(medium.DriveType),
		"--medium", medium.Medium,
	)
	return err
}

// SetExtraData attaches custom string to the VM.
func (m *Machine) SetExtraData(key, val string) error {
	_, _, err := defaultManager.run(context.Background(),
		"setextradata", m.Name, key, val)
	return err
}

// GetExtraData retrieves custom string from the VM.
func (m *Machine) GetExtraData(key string) (*string, error) {
	value, _, err := defaultManager.run(context.Background(),
		"getextradata", m.Name, key)
	if err != nil {
		return nil, err
	}
	value = strings.TrimSpace(value)
	/* 'getextradata get' returns 0 even when the key is not found,
	so we need to check stdout for this case */
	if strings.HasPrefix(value, "No value set") {
		return nil, nil
	}
	trimmed := strings.TrimPrefix(value, "Value: ")
	return &trimmed, nil
}

// DeleteExtraData removes custom string from the VM.
func (m *Machine) DeleteExtraData(key string) error {
	_, _, err := defaultManager.run(context.Background(),
		"setextradata", m.Name, key)
	return err
}

// CloneMachine clones the given machine name into a new one.
func CloneMachine(baseImageName string, newImageName string, register bool) error {
	if register {
		_, _, err := defaultManager.run(context.Background(),
			"clonevm", baseImageName, "--name", newImageName, "--register")
		return err
	}
	_, _, err := defaultManager.run(context.Background(),
		"clonevm", baseImageName, "--name", newImageName)
	return err
}
