package observe

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const (
	goodIngressSettings = "conf-dir=/home/nuitcloud/nginx/conf.d\ncert-root=/home/nuitcloud/nginx/certs\ncontainer=nuit-nginx\n"
	ingressRev          = "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
)

func ingressRecord(settings, revision string) func(string) (string, error) {
	return func(name string) (string, error) {
		switch {
		case name == "settings" && settings != "":
			return settings, nil
		case name == "revision" && revision != "":
			return revision + "\n", nil
		}
		return "", errors.New("no such file")
	}
}

// docker stands in for the container engine: whether the container runs and whether nginx -t passes; it records what it was asked.
func docker(running, nginxOK bool, asked *[]string) func(context.Context, string, ...string) (string, error) {
	return func(_ context.Context, name string, args ...string) (string, error) {
		*asked = append(*asked, name+" "+strings.Join(args, " "))
		switch {
		case name == "docker" && args[0] == "inspect":
			if running {
				return "true\n", nil
			}
			return "false\n", nil
		case name == "docker" && args[0] == "exec" && strings.Join(args[1:], " ") == "nuit-nginx nginx -t":
			if nginxOK {
				return "", nil
			}
			return "", errors.New("exit status 1")
		}
		return "", errors.New("unexpected")
	}
}

func TestTheIngressObservationSaysWhatWasAppliedAndWhetherNginxAcceptsIt(t *testing.T) {
	var asked []string
	c := Collector{
		Run:              docker(true, true, &asked),
		ReadIngressState: ingressRecord(goodIngressSettings, ingressRev),
		ListDir: func(dir string) ([]string, error) {
			if dir != "/home/nuitcloud/nginx/conf.d" {
				return nil, errors.New("wrong directory " + dir)
			}
			return []string{"00-nuit-cloud-default.conf", "nuit-cloud-abc12345-shop.conf", "nuit-cloud-abc12345-pihole.conf", "other.conf", "nuit-cloud-x.conf.bak"}, nil
		},
	}
	if !c.Known(Ingress) {
		t.Fatal("ingress is not a known kind")
	}
	data, v1, err := c.Collect(context.Background(), Ingress)
	if err != nil || !versionOK.MatchString(v1) {
		t.Fatalf("%v %q", err, v1)
	}
	raw, _ := json.Marshal(data)
	if string(raw) != `{"revision":"`+ingressRev+`","containerRunning":true,"sites":3,"nginxOk":true}` {
		t.Errorf("%s", raw)
	}
	if strings.Join(asked, "\n") != "docker inspect --format {{.State.Running}} nuit-nginx\ndocker exec nuit-nginx nginx -t" {
		t.Errorf("asked: %v", asked)
	}
	// nginx refusing it is a different fact, and so a different version
	c.Run = docker(true, false, &asked)
	data, v2, _ := c.Collect(context.Background(), Ingress)
	raw, _ = json.Marshal(data)
	if v2 == v1 || !strings.Contains(string(raw), `"nginxOk":false`) {
		t.Errorf("%s %q", raw, v2)
	}
	// a stopped container: nginx -t is not run, and its answer is null
	asked = nil
	c.Run = docker(false, true, &asked)
	data, _, _ = c.Collect(context.Background(), Ingress)
	raw, _ = json.Marshal(data)
	if !strings.Contains(string(raw), `"containerRunning":false`) || !strings.Contains(string(raw), `"nginxOk":null`) || len(asked) != 1 {
		t.Errorf("%s %v", raw, asked)
	}
	// no revision recorded (or not a revision): null
	c.ReadIngressState = ingressRecord(goodIngressSettings, "HEAD")
	data, _, _ = c.Collect(context.Background(), Ingress)
	raw, _ = json.Marshal(data)
	if !strings.Contains(string(raw), `"revision":null`) {
		t.Errorf("%s", raw)
	}
}

func TestAMachineThatIsNotTheIngressServiceSendsNoIngressObservation(t *testing.T) {
	var asked []string
	for name, settings := range map[string]string{
		"no record":         "",
		"a hostile record":  "conf-dir=/home/x\ncert-root=/home/y\ncontainer=x;reboot\n",
		"a relative record": "conf-dir=home\ncert-root=/home/y\ncontainer=nuit-nginx\n",
	} {
		c := Collector{Run: docker(true, true, &asked), ReadIngressState: ingressRecord(settings, ingressRev)}
		if data, v, err := c.Collect(context.Background(), Ingress); data != nil || v != "" || err != nil {
			t.Errorf("%s: %v %q %v", name, data, v, err)
		}
	}
	if len(asked) != 0 {
		t.Errorf("docker was asked: %v", asked)
	}
	if data, _, _ := (Collector{}).Collect(context.Background(), Ingress); data != nil {
		t.Errorf("%v", data)
	}
}
