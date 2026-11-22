package lanscan

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func fake(answering map[string]string, installed ...string) (Scanner, *[]string) {
	var mu sync.Mutex
	var calls []string
	has := map[string]bool{}
	for _, i := range installed {
		has[i] = true
	}
	return Scanner{
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			mu.Lock()
			calls = append(calls, name+" "+strings.Join(args, " "))
			mu.Unlock()
			addr := args[len(args)-1]
			switch name {
			case "arping":
				if mac, ok := answering[addr]; ok {
					return "Unicast reply from " + addr + " [" + strings.ToUpper(mac) + "]  0.7ms\n", nil
				}
				return "Timeout\n", errors.New("exit status 1")
			case "ping":
				if _, ok := answering[addr]; ok {
					return "1 received", nil
				}
				return "", errors.New("exit status 1")
			case "ip":
				return addr + " dev eth0 lladdr " + answering[addr] + " REACHABLE\n", nil
			}
			return "", errors.New("unexpected " + name)
		},
		Has:       func(n string) bool { return has[n] },
		Interface: func() (string, error) { return "eth0", nil },
		Own:       func() map[string]string { return map[string]string{"10.10.10.163": "aa:bb:cc:00:00:03"} },
	}, &calls
}

func TestArpingFindsWhatAnswersAndTheirMACs(t *testing.T) {
	s, calls := fake(map[string]string{"10.10.10.162": "aa:bb:cc:00:00:02", "10.10.10.164": "aa:bb:cc:00:00:04"}, "arping")
	r, err := s.Scan(context.Background(), "10.10.10.161", "10.10.10.165")
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "arping" || r.From != "eth0" || r.Scanned != 4 || len(r.Answered) != 3 || r.Answered[0] != (Answer{Address: "10.10.10.162", MAC: "aa:bb:cc:00:00:02"}) || r.Answered[2].Address != "10.10.10.164" {
		t.Errorf("%+v", r)
	}
	// the machine's own address answers too, with its own MAC, flagged as its own, in order among the others
	if r.Answered[1] != (Answer{Address: "10.10.10.163", MAC: "aa:bb:cc:00:00:03", Self: true}) {
		t.Errorf("own address not reported as itself: %+v", r.Answered[1])
	}
	for _, c := range *calls {
		if strings.Contains(c, "10.10.10.163") {
			t.Errorf("probed its own address: %s", c)
		}
		if !strings.Contains(c, "-I eth0") {
			t.Errorf("did not probe from the default interface: %s", c)
		}
	}
}

func TestWithoutArpingItPingsAndReadsTheNeighbourTable(t *testing.T) {
	s, _ := fake(map[string]string{"10.10.10.170": "aa:bb:cc:00:00:70"})
	r, err := s.Scan(context.Background(), "10.10.10.168", "10.10.10.171")
	if err != nil || r.Method != "ping" || len(r.Answered) != 1 || r.Answered[0].MAC != "aa:bb:cc:00:00:70" {
		t.Errorf("%+v %v", r, err)
	}
}

func TestNothingAnsweringIsAnEmptyListNotNull(t *testing.T) {
	s, _ := fake(nil, "arping")
	r, _ := s.Scan(context.Background(), "10.10.10.168", "10.10.10.169")
	if r.Answered == nil || len(r.Answered) != 0 {
		t.Errorf("%+v", r)
	}
}

func TestOnlyBoundedPrivateIPv4RangesAreScanned(t *testing.T) {
	for _, c := range [][2]string{{"8.8.8.1", "8.8.8.9"}, {"10.0.0.9", "10.0.0.1"}, {"10.0.0.1", "10.0.1.200"}, {"fe80::1", "fe80::9"}, {"nope", "10.0.0.1"}, {"10.0.0.1; reboot", "10.0.0.2"}} {
		if _, err := Addresses(c[0], c[1]); err == nil {
			t.Errorf("%v was accepted", c)
		}
	}
	got, err := Addresses("192.168.1.254", "192.168.2.1")
	if err != nil || len(got) != 4 || got[2] != "192.168.2.0" {
		t.Errorf("%v %v", got, err)
	}
}

func TestTheDefaultInterfaceIsTheRouteTo0000(t *testing.T) {
	table := "Iface\tDestination\tGateway\tFlags\nenp0s1\t00000000\t0101A8C0\t0003\nenp0s1\t0001A8C0\t00000000\t0001\n"
	if got, err := DefaultInterface(table); err != nil || got != "enp0s1" {
		t.Errorf("%q %v", got, err)
	}
	if _, err := DefaultInterface("Iface\tDestination\n"); err == nil {
		t.Error("no default route was accepted")
	}
}

// With the management address on a VLAN (nuit-cloud/nuit-cloud#197) the default route is on <if>.<n>: that is where the scan runs from.
func TestTheDefaultInterfaceMayBeAVLANInterface(t *testing.T) {
	table := "Iface\tDestination\tGateway\tFlags\nenp0s31f6.31\t00000000\t01001F0A\t0003\nenp0s31f6.31\t00001F0A\t00000000\t0001\nenp0s31f6.5\t0000140A\t00000000\t0001\n"
	if got, err := DefaultInterface(table); err != nil || got != "enp0s31f6.31" {
		t.Errorf("%q %v", got, err)
	}
	s, calls := fake(map[string]string{"10.31.0.20": "aa:bb:cc:00:00:20"}, "arping")
	s.Interface = func() (string, error) { return "enp0s31f6.31", nil }
	r, err := s.Scan(context.Background(), "10.31.0.20", "10.31.0.21")
	if err != nil || r.From != "enp0s31f6.31" || len(r.Answered) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, c := range *calls {
		if strings.HasPrefix(c, "arping ") && !strings.Contains(c, "enp0s31f6.31") {
			t.Errorf("probed from elsewhere: %s", c)
		}
	}
}

func TestItsOwnAddressesAreReportedWithTheirMACButNeverProbed(t *testing.T) {
	s, calls := fake(nil, "arping")
	s.Own = func() map[string]string {
		return map[string]string{"10.10.10.163": "aa:bb:cc:00:00:03", "10.10.10.200": "aa:bb:cc:00:00:99", "192.168.1.5": "aa:bb:cc:00:00:05"}
	}
	r, err := s.Scan(context.Background(), "10.10.10.160", "10.10.10.165")
	if err != nil {
		t.Fatal(err)
	}
	// only the own address inside the scanned range is reported, and the scan covered the other five addresses
	if len(r.Answered) != 1 || r.Answered[0] != (Answer{Address: "10.10.10.163", MAC: "aa:bb:cc:00:00:03", Self: true}) || r.Scanned != 5 {
		t.Errorf("%+v", r)
	}
	for _, c := range *calls {
		if strings.Contains(c, "10.10.10.163") {
			t.Errorf("probed its own address: %s", c)
		}
	}
}
