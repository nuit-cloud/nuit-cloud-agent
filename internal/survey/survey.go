// Package survey reads a machine and reports facts about it. It changes nothing. It reports facts only (what is there); what
// they mean, and what to do about them, is the platform's business, so rules can change without a new agent.
package survey

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Report is the survey's result, sent to the platform as the task's findings.
type Report struct {
	CollectedAt string  `json:"collectedAt"`
	Hostname    string  `json:"hostname"`
	OS          OS      `json:"os"`
	CPU         CPU     `json:"cpu"`
	Memory      Memory  `json:"memory"`
	Cgroups     Cgroups `json:"cgroups"`
	Disks       []Disk  `json:"disks"`
	Network     Network `json:"network"`
	Time        Time    `json:"time"`
	SSH         SSH     `json:"ssh"`
	Power       Power   `json:"power"`
	// AuthorizedKeys are the keys that can log in as the platform's login account (nuitcloud): fingerprints and the registry id of
	// the lines the platform manages. null when the file could not be read (see collectionProblems), [] when there is none.
	AuthorizedKeys []AuthorizedKey `json:"authorizedKeys"`
	Problems       []string        `json:"collectionProblems,omitempty"` // things the survey could not read, so a gap is not mistaken for a clean bill
}

type OS struct {
	ID         string `json:"id"`
	VersionID  string `json:"versionId"`
	PrettyName string `json:"prettyName"`
	Kernel     string `json:"kernel"`
	Arch       string `json:"arch"`
}

type CPU struct {
	Model   string `json:"model"`
	Logical int    `json:"logical"`
	// InterruptController is the interrupt controller an arm64 machine has (GICv3, GICv2), read from /proc/interrupts. The
	// arm64 virtual machine monitor of the Nuit Container Service needs GICv3. Empty on other CPUs or when it cannot be read.
	InterruptController string `json:"interruptController,omitempty"`
	// Virtualisation is whether the CPU can run the hardware-assisted virtual machines the Nuit Container Service needs.
	Virtualisation Virtualisation `json:"virtualisation"`
}

type Virtualisation struct {
	Supported    bool   `json:"supported"`
	Flag         string `json:"flag,omitempty"` // vmx (Intel), svm (AMD), or kvm (arm64, which has no CPU flag)
	KVMDevice    bool   `json:"kvmDevice"`      // /dev/kvm exists
	KVMReadWrite bool   `json:"kvmReadWrite"`   // and this process can open it
}

type Memory struct {
	TotalMiB     int64 `json:"totalMiB"`
	AvailableMiB int64 `json:"availableMiB"`
	SwapMiB      int64 `json:"swapMiB"`
}

type Cgroups struct {
	V2          bool     `json:"v2"`
	Controllers []string `json:"controllers"`
}

type Disk struct {
	Mount    string `json:"mount"`
	Device   string `json:"device"`
	FSType   string `json:"fsType"`
	TotalMiB int64  `json:"totalMiB"`
	FreeMiB  int64  `json:"freeMiB"`
}

type Network struct {
	Interfaces   []Interface `json:"interfaces"`
	DefaultRoute *Route      `json:"defaultRoute"`
}

type Interface struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"` // wired, wireless, virtual or loopback
	Up        bool     `json:"up"`
	SpeedMbps int      `json:"speedMbps"` // -1 when the link does not say
	MAC       string   `json:"mac,omitempty"`
	Addresses []string `json:"addresses"`
}

type Route struct {
	Interface string `json:"interface"`
	Gateway   string `json:"gateway"`
}

type Time struct {
	// Synchronized is whether the clock is being kept right by an NTP service; nil when that could not be told.
	Synchronized *bool `json:"synchronized"`
}

type SSH struct {
	PermitRootLogin        string `json:"permitRootLogin"` // what sshd uses (or, from the file, as configured); "" when unknown
	PasswordAuthentication string `json:"passwordAuthentication"`
	// Source says where the two values come from: "effective" is `sshd -T`, what sshd will use after merging the main file, its
	// included files and its defaults; "file" is only what /etc/ssh/sshd_config says (a setting made elsewhere, or left to the
	// default, is then ""); empty when neither could be read.
	Source string `json:"source,omitempty"`
}

// Power is whether the machine is a laptop and whether it will sleep on its own. A laptop shut on a shelf as a server is suspended by systemd's default when its lid closes, and a
// suspended machine answers nothing; the survey says what logind will do, and the platform says whether that is a problem.
type Power struct {
	// Laptop is true when the chassis type says portable (DMI chassis types 8, 9, 10, 14, 30, 31, 32) or a battery is present.
	Laptop  bool `json:"laptop"`
	Battery bool `json:"battery"`
	// Chassis is the DMI chassis type number, "" when the machine does not say.
	Chassis string `json:"chassis,omitempty"`
	// What logind will do, as it answers (`HandleLidSwitch` and the others on login1's Manager): "ignore", "suspend", "poweroff", ...; "" when it could not be asked.
	LidSwitch              string `json:"lidSwitch,omitempty"`
	LidSwitchExternalPower string `json:"lidSwitchExternalPower,omitempty"`
	LidSwitchDocked        string `json:"lidSwitchDocked,omitempty"`
	IdleAction             string `json:"idleAction,omitempty"`
	// SleepMasked is whether every sleep target (sleep, suspend, hibernate, hybrid-sleep) is masked, so nothing can suspend the machine; nil when it could not be told.
	SleepMasked *bool `json:"sleepMasked"`
}

// Sources are where the survey reads from, so a test can point it at fixtures.
type Sources struct {
	Root       string // prefix for /proc, /sys, /etc ... ("" on a real machine)
	Run        func(ctx context.Context, name string, args ...string) (string, error)
	Now        func() time.Time
	Hostname   func() (string, error)
	Interfaces func() ([]net.Interface, error)
	Addrs      func(net.Interface) ([]net.Addr, error)
	Statfs     func(path string) (total, free int64, err error)
	Open       func(path string) error
}

// Real is the real machine.
func Real() Sources {
	return Sources{
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			out, err := exec.CommandContext(ctx, name, args...).Output()
			return string(out), err
		},
		Now:        time.Now,
		Hostname:   os.Hostname,
		Interfaces: net.Interfaces,
		Addrs:      func(i net.Interface) ([]net.Addr, error) { return i.Addrs() },
		Statfs: func(path string) (int64, int64, error) {
			var s syscall.Statfs_t
			if err := syscall.Statfs(path, &s); err != nil {
				return 0, 0, err
			}
			return int64(s.Blocks) * int64(s.Bsize), int64(s.Bavail) * int64(s.Bsize), nil
		},
		Open: func(path string) error {
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err == nil {
				f.Close()
			}
			return err
		},
	}
}

func (s Sources) path(p string) string { return filepath.Join(s.Root, p) }

func (s Sources) read(p string) (string, error) {
	raw, err := os.ReadFile(s.path(p))
	return string(raw), err
}

// Collect surveys the machine. Anything it cannot read is left out and listed under collectionProblems.
func Collect(ctx context.Context, s Sources) Report {
	r := Report{CollectedAt: s.Now().UTC().Format(time.RFC3339), Disks: []Disk{}, Network: Network{Interfaces: []Interface{}}}
	problem := func(what string, err error) {
		r.Problems = append(r.Problems, what+": "+err.Error())
	}

	if h, err := s.Hostname(); err == nil {
		r.Hostname = h
	} else {
		problem("hostname", err)
	}
	if text, err := s.read("/etc/os-release"); err == nil {
		r.OS = ParseOSRelease(text)
	} else {
		problem("os-release", err)
	}
	r.OS.Arch = runtime.GOARCH
	if text, err := s.read("/proc/sys/kernel/osrelease"); err == nil {
		r.OS.Kernel = strings.TrimSpace(text)
	}
	if text, err := s.read("/proc/cpuinfo"); err == nil {
		r.CPU = ParseCPUInfo(text)
	} else {
		problem("cpuinfo", err)
	}
	if _, err := os.Stat(s.path("/dev/kvm")); err == nil {
		r.CPU.Virtualisation.KVMDevice = true
		r.CPU.Virtualisation.KVMReadWrite = s.Open(s.path("/dev/kvm")) == nil
	}
	AdjustVirtualisationForArch(r.OS.Arch, &r.CPU.Virtualisation)
	if r.OS.Arch == "arm64" {
		if text, err := s.read("/proc/interrupts"); err == nil {
			r.CPU.InterruptController = ParseInterruptController(text)
		}
	}
	if text, err := s.read("/proc/meminfo"); err == nil {
		r.Memory = ParseMemInfo(text)
	} else {
		problem("meminfo", err)
	}
	if text, err := s.read("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		r.Cgroups = Cgroups{V2: true, Controllers: strings.Fields(text)}
	} else {
		r.Cgroups = Cgroups{Controllers: []string{}}
	}
	if text, err := s.read("/proc/mounts"); err == nil {
		for _, m := range RealMounts(text) {
			// A single file mounted over another (as container runtimes do for /etc/hosts) is not storage.
			if info, err := os.Stat(s.path(m.Mount)); err == nil && !info.IsDir() {
				continue
			}
			total, free, err := s.Statfs(m.Mount)
			if err != nil {
				continue
			}
			r.Disks = append(r.Disks, Disk{Mount: m.Mount, Device: m.Device, FSType: m.FSType, TotalMiB: total >> 20, FreeMiB: free >> 20})
		}
	} else {
		problem("mounts", err)
	}
	r.Network = collectNetwork(s, problem)
	r.Time = collectTime(ctx, s)
	r.SSH = collectSSH(ctx, s, problem)
	r.Power = collectPower(ctx, s)
	r.AuthorizedKeys = collectAuthorizedKeys(s, problem)
	return r
}

// ParseOSRelease reads /etc/os-release.
func ParseOSRelease(text string) OS {
	var o OS
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		switch k {
		case "ID":
			o.ID = v
		case "VERSION_ID":
			o.VersionID = v
		case "PRETTY_NAME":
			o.PrettyName = v
		}
	}
	return o
}

// AdjustVirtualisationForArch: arm64 CPUs publish no vmx/svm-style flag; the kernel offers /dev/kvm only when the machine booted
// able to run KVM, so a usable /dev/kvm is the evidence of hardware virtualisation there.
func AdjustVirtualisationForArch(arch string, v *Virtualisation) {
	if arch == "arm64" && !v.Supported && v.KVMDevice && v.KVMReadWrite {
		v.Supported, v.Flag = true, "kvm"
	}
}

// ParseInterruptController finds the ARM generic interrupt controller in /proc/interrupts ("GICv3", "GICv2"), or "".
func ParseInterruptController(text string) string {
	for _, gic := range []string{"GICv3", "GICv2", "GIC-400"} {
		if strings.Contains(text, gic) {
			if gic == "GIC-400" {
				return "GICv2" // the GIC-400 is a GICv2 implementation
			}
			return gic
		}
	}
	return ""
}

// ParseCPUInfo reads /proc/cpuinfo: the model, the number of logical CPUs, and whether the CPU has the hardware
// virtualisation flag (vmx on Intel, svm on AMD).
func ParseCPUInfo(text string) CPU {
	var c CPU
	var implementer, part string
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "CPU implementer":
			if implementer == "" {
				implementer = v
			}
		case "CPU part":
			if part == "" {
				part = v
			}
		case "processor":
			c.Logical++
		case "model name", "Model", "cpu model":
			if c.Model == "" {
				c.Model = v
			}
		case "flags", "Features":
			if c.Virtualisation.Flag == "" {
				for _, f := range strings.Fields(v) {
					if f == "vmx" || f == "svm" {
						c.Virtualisation.Supported = true
						c.Virtualisation.Flag = f
						break
					}
				}
			}
		}
	}
	if c.Model == "" && implementer != "" {
		// arm64 /proc/cpuinfo names no model, only an implementer and part code (0x61 is Apple)
		c.Model = "ARM CPU (implementer " + implementer + ", part " + part + ")"
	}
	return c
}

// ParseMemInfo reads /proc/meminfo (values are in kB).
func ParseMemInfo(text string) Memory {
	var m Memory
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "MemTotal":
			m.TotalMiB = kb >> 10
		case "MemAvailable":
			m.AvailableMiB = kb >> 10
		case "SwapTotal":
			m.SwapMiB = kb >> 10
		}
	}
	return m
}

// Mount is one line of /proc/mounts worth reporting.
type Mount struct{ Device, Mount, FSType string }

var ignoredFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "tmpfs": true, "cgroup": true, "cgroup2": true, "pstore": true,
	"securityfs": true, "debugfs": true, "tracefs": true, "configfs": true, "fusectl": true, "mqueue": true, "hugetlbfs": true,
	"bpf": true, "autofs": true, "binfmt_misc": true, "overlay": true, "squashfs": true, "ramfs": true, "efivarfs": true, "nsfs": true,
}

// RealMounts keeps the mounts that are real storage: not the kernel's virtual filesystems, container layers or snaps.
func RealMounts(text string) []Mount {
	seen := map[string]bool{}
	var out []Mount
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || ignoredFS[f[2]] || !strings.HasPrefix(f[0], "/") {
			continue
		}
		mount := strings.ReplaceAll(f[1], `\040`, " ")
		if seen[mount] || strings.HasPrefix(mount, "/var/lib/docker/") || strings.HasPrefix(mount, "/snap/") || strings.HasPrefix(mount, "/run/") {
			continue
		}
		seen[mount] = true
		out = append(out, Mount{Device: f[0], Mount: mount, FSType: f[2]})
	}
	return out
}

// ParseDefaultRoute reads /proc/net/route: the interface and gateway of the default route, or nil.
func ParseDefaultRoute(text string) *Route {
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Scan() // header
	var best *Route
	bestMetric := int64(1 << 62)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 7 || f[1] != "00000000" || f[7-1] == "" {
			continue
		}
		metric, _ := strconv.ParseInt(f[6], 10, 64)
		gw, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil || metric >= bestMetric {
			continue
		}
		bestMetric = metric
		ip := net.IPv4(byte(gw), byte(gw>>8), byte(gw>>16), byte(gw>>24)) // the kernel prints it little-endian
		best = &Route{Interface: f[0], Gateway: ip.String()}
	}
	return best
}

func collectNetwork(s Sources, problem func(string, error)) Network {
	n := Network{Interfaces: []Interface{}}
	ifaces, err := s.Interfaces()
	if err != nil {
		problem("interfaces", err)
		return n
	}
	sort.Slice(ifaces, func(i, j int) bool { return ifaces[i].Name < ifaces[j].Name })
	for _, i := range ifaces {
		item := Interface{Name: i.Name, Up: i.Flags&net.FlagUp != 0, SpeedMbps: -1, Addresses: []string{}, MAC: i.HardwareAddr.String()}
		item.Kind = interfaceKind(s, i)
		if text, err := s.read("/sys/class/net/" + i.Name + "/speed"); err == nil {
			if v, err := strconv.Atoi(strings.TrimSpace(text)); err == nil && v > 0 {
				item.SpeedMbps = v
			}
		}
		if addrs, err := s.Addrs(i); err == nil {
			for _, a := range addrs {
				item.Addresses = append(item.Addresses, a.String())
			}
		}
		n.Interfaces = append(n.Interfaces, item)
	}
	if text, err := s.read("/proc/net/route"); err == nil {
		n.DefaultRoute = ParseDefaultRoute(text)
	}
	return n
}

func interfaceKind(s Sources, i net.Interface) string {
	if i.Flags&net.FlagLoopback != 0 {
		return "loopback"
	}
	base := "/sys/class/net/" + i.Name
	if _, err := os.Stat(s.path(base + "/wireless")); err == nil {
		return "wireless"
	}
	if _, err := os.Stat(s.path(base + "/phy80211")); err == nil {
		return "wireless"
	}
	// A virtual device (a bridge, a veth, a VLAN, a tunnel) lives under /sys/devices/virtual, not under a bus.
	if target, err := os.Readlink(s.path(base)); err == nil && strings.Contains(target, "/virtual/") {
		return "virtual"
	}
	return "wired"
}

func collectTime(ctx context.Context, s Sources) Time {
	out, err := s.Run(ctx, "timedatectl", "show", "--property=NTPSynchronized", "--value")
	if err != nil {
		return Time{}
	}
	switch strings.TrimSpace(strings.ToLower(out)) {
	case "yes":
		v := true
		return Time{Synchronized: &v}
	case "no":
		v := false
		return Time{Synchronized: &v}
	}
	return Time{}
}

// collectSSH asks sshd what it will use (`sshd -T`: the main file, its included files and the defaults, merged as sshd merges
// them). Without it only the main file's text is read, which misses a setting made in sshd_config.d/ or left to the default.
func collectSSH(ctx context.Context, s Sources, problem func(string, error)) SSH {
	for _, binary := range []string{"sshd", "/usr/sbin/sshd"} {
		out, err := s.Run(ctx, binary, "-T")
		if err != nil {
			continue
		}
		c := ParseSSHDConfig(out)
		if c.PermitRootLogin != "" || c.PasswordAuthentication != "" {
			c.Source = "effective"
			return c
		}
	}
	var c SSH
	if text, err := s.read("/etc/ssh/sshd_config"); err == nil {
		c = ParseSSHDConfig(text)
		c.Source = "file"
		problem("sshd -T", errors.New("could not ask sshd what it uses, so the SSH settings are only what /etc/ssh/sshd_config says"))
	} else {
		problem("sshd_config", err)
	}
	return c
}

// ParseSSHDConfig reads the settings that matter for hardening from sshd_config; an unset one is "".
func ParseSSHDConfig(text string) SSH {
	var c SSH
	sc := bufio.NewScanner(bytes.NewBufferString(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch strings.ToLower(f[0]) {
		case "permitrootlogin":
			if c.PermitRootLogin == "" { // sshd uses the first value it sees
				c.PermitRootLogin = strings.ToLower(f[1])
			}
		case "passwordauthentication":
			if c.PasswordAuthentication == "" {
				c.PasswordAuthentication = strings.ToLower(f[1])
			}
		}
	}
	return c
}

// laptopChassis are the DMI chassis types of a portable machine: portable, laptop, notebook, sub-notebook, tablet, convertible, detachable.
var laptopChassis = map[string]bool{"8": true, "9": true, "10": true, "14": true, "30": true, "31": true, "32": true}

// ParseBusctlString reads `busctl get-property` output for a string property: `s "ignore"`.
func ParseBusctlString(out string) string {
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) == 2 && f[0] == "s" {
		return strings.Trim(f[1], `"`)
	}
	return ""
}

// ParseSleepMasked reads `systemctl is-enabled` for the sleep targets, one state per line (the command exits non-zero for a masked unit but still prints it): true when every one is masked.
func ParseSleepMasked(out string, targets int) *bool {
	lines := strings.Fields(strings.TrimSpace(out))
	if len(lines) < targets {
		return nil
	}
	all := true
	for _, l := range lines[:targets] {
		if l != "masked" {
			all = false
		}
	}
	return &all
}

func collectPower(ctx context.Context, s Sources) Power {
	var p Power
	if text, err := s.read("/sys/class/dmi/id/chassis_type"); err == nil {
		p.Chassis = strings.TrimSpace(text)
	}
	if matches, _ := filepath.Glob(s.path("/sys/class/power_supply/BAT*")); len(matches) > 0 {
		p.Battery = true
	}
	p.Laptop = p.Battery || laptopChassis[p.Chassis]
	ask := func(property string) string {
		out, err := s.Run(ctx, "busctl", "--system", "get-property", "org.freedesktop.login1", "/org/freedesktop/login1", "org.freedesktop.login1.Manager", property)
		if err != nil {
			return ""
		}
		return ParseBusctlString(out)
	}
	p.LidSwitch = ask("HandleLidSwitch")
	p.LidSwitchExternalPower = ask("HandleLidSwitchExternalPower")
	p.LidSwitchDocked = ask("HandleLidSwitchDocked")
	p.IdleAction = ask("IdleAction")
	out, _ := s.Run(ctx, "systemctl", "is-enabled", "sleep.target", "suspend.target", "hibernate.target", "hybrid-sleep.target")
	p.SleepMasked = ParseSleepMasked(out, 4)
	return p
}
