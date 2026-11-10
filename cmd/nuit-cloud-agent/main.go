// nuit-cloud-agent is the program a Nuit Cloud Server runs so the platform can commission and look after it.
//
// It only ever calls out to the platform; it listens on nothing. It does only what the platform signs and its own catalog
// allows (see ADR 0001 in the nuit-cloud repository).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/client"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/controlplane"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/loop"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/observe"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/profiles"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/runner"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/runtimeinstall"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/state"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/survey"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/update"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

const defaultStateDir = "/var/lib/nuit-cloud-agent"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "enroll":
		err = enroll(args)
	case "run":
		err = run(args)
	case "status":
		err = status(args)
	case "survey":
		err = surveyCmd()
	case "profile-check":
		err = profileCheck(args)
	case "selftest":
		err = selftest(args)
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "nuit-cloud-agent:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `nuit-cloud-agent `+version+`

  enroll --url https://platform --code nce_... [--hostname name] [--state-dir dir] [--insecure-http]
        Join the platform with an enrollment code from the admin portal. Run once, as root.
  run [--state-dir dir] [--insecure-http]
        Call the platform every 30 seconds and perform the tasks it signs. This is what the service runs.
  status [--state-dir dir]
        Show who this machine is on the platform (never the credential).
  survey
        Read this machine and print what the platform would be told. Changes nothing.
  profile-check <profile>
        Compare this machine with a profile and print the result as JSON. Changes nothing.
  selftest [--state-dir dir] [--insecure-http]
        Load this machine's identity and make one real check-in, then print ok. Used before an update replaces the agent.
  version
`)
}

// insecureFlag is --insecure-http, which can also be set for the whole service with NUIT_CLOUD_AGENT_INSECURE_HTTP=1 (so a
// unit file for a trusted test network needs no edit to its command).
func insecureFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("insecure-http", os.Getenv("NUIT_CLOUD_AGENT_INSECURE_HTTP") == "1", "allow a plain http:// platform address (a trusted test network only)")
}

func stateDirFlag(fs *flag.FlagSet) *string {
	def := os.Getenv("NUIT_CLOUD_AGENT_STATE_DIR")
	if def == "" {
		def = defaultStateDir
	}
	return fs.String("state-dir", def, "where the agent keeps its identity (readable by root only)")
}

func enroll(args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	url := fs.String("url", os.Getenv("NUIT_CLOUD_PLATFORM_URL"), "the platform's address, https://...")
	code := fs.String("code", os.Getenv("NUIT_CLOUD_ENROLLMENT_CODE"), "an enrollment code from the admin portal")
	hostname := fs.String("hostname", "", "the name to enrol under (default: this machine's hostname)")
	insecure := insecureFlag(fs)
	dir := stateDirFlag(fs)
	_ = fs.Parse(args)
	if *url == "" || *code == "" {
		return fmt.Errorf("--url and --code are needed")
	}
	if _, err := state.Load(*dir); err == nil {
		return fmt.Errorf("this machine is already enrolled (state in %s); remove that directory first to enrol again", *dir)
	}
	name := *hostname
	if name == "" {
		h, err := os.Hostname()
		if err != nil {
			return err
		}
		name = strings.ToLower(h)
	}
	c, err := client.New(*url, version, *insecure)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := c.Enroll(ctx, *code, name)
	if err != nil {
		return err
	}
	st := state.New(*dir)
	st.PlatformURL, st.MachineID, st.Name, st.Credential = c.BaseURL, res.MachineID, res.Name, res.Credential
	st.TrustedKeys, st.EnrolledAt = res.TaskSigning.Keys, time.Now().UTC()
	st.Fallbacks = c.SetFallbacks(res.FallbackAddresses)
	if err := st.Save(); err != nil {
		return fmt.Errorf("enrolled, but could not save the identity (the code is now used up): %w", err)
	}
	fmt.Printf("enrolled as %s (machine %s); it will check in every %d seconds\n", res.Name, res.MachineID, res.PollIntervalSeconds)
	return nil
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	insecure := insecureFlag(fs)
	dir := stateDirFlag(fs)
	_ = fs.Parse(args)
	st, err := state.Load(*dir)
	if err != nil {
		return err
	}
	c, err := client.New(st.PlatformURL, version, *insecure)
	if err != nil {
		return err
	}
	c.Credential = st.Credential
	c.SetFallbacks(st.Fallbacks)
	facts := observe.Real()
	facts.Clock = c.ClockOffset
	if u, err := url.Parse(c.BaseURL); err == nil {
		facts.PlatformHost = u.Hostname()
	}
	facts.TrustedKeyIDs = st.KeyIDs
	c.Observer = facts
	logger := log.New(os.Stderr, "", log.LstdFlags|log.LUTC)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Updating itself: the running binary, and a guard that confirms a freshly installed version or puts the old one back.
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	restart := &atomic.Bool{}
	l := &loop.Loop{
		State:    st,
		Platform: c,
		FallbackSink: func(list []string) {
			if err := st.SetFallbacks(c.SetFallbacks(list)); err != nil {
				logger.Printf("could not save the platform's fallback addresses: %v", err)
			}
		},
		Route:       c.Route,
		ClockOffset: c.ClockOffset,
		Runner: runner.Runner{
			Version:      version,
			Survey:       func(ctx context.Context) survey.Report { return survey.Collect(ctx, survey.Real()) },
			Profiles:     profiles.Runner{},
			PlatformURL:  c.BaseURL,
			Runtime:      runtimeinstall.Stager{BaseURL: c.BaseURL, HTTP: c, Dir: filepath.Join(*dir, "stage")},
			ControlPlane: controlplane.Provisioner{Source: c, Fetcher: runtimeinstall.Stager{BaseURL: c.BaseURL, HTTP: c, Dir: filepath.Join(*dir, "stage")}, StageDir: filepath.Join(*dir, "stage"), PlatformURL: c.BaseURL},
			Updater:      update.Updater{BaseURL: c.BaseURL, HTTP: c, ExePath: exe, StateDir: *dir, Version: version, Arch: runtime.GOARCH, State: st},
			Keys:         st,
			Restart:      restart,
		},
		Guard:   update.Guard{ExePath: exe, Version: version, State: st},
		Restart: stop, // the service manager starts the agent again; the new binary is what runs
		Log:     logger.Printf,
	}
	logger.Printf("nuit-cloud-agent %s: %s (%s) calling %s", version, st.Name, st.MachineID, st.PlatformURL)
	l.Run(ctx)
	logger.Printf("stopping")
	return nil
}

func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dir := stateDirFlag(fs)
	_ = fs.Parse(args)
	st, err := state.Load(*dir)
	if err != nil {
		return err
	}
	keys := st.KeyIDs()
	fmt.Printf("name:        %s\nmachine id:  %s\nplatform:    %s\nenrolled at: %s\ntrusted keys: %s\nagent:       %s\n", st.Name, st.MachineID, st.PlatformURL, st.EnrolledAt.Format(time.RFC3339), strings.Join(keys, ", "), version)
	return nil
}

func surveyCmd() error {
	out, err := json.MarshalIndent(survey.Collect(context.Background(), survey.Real()), "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

func profileCheck(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: nuit-cloud-agent profile-check <profile> (this agent carries: %s)", strings.Join(profiles.Runner{}.Names(), ", "))
	}
	res, _, err := profiles.Runner{}.Run(context.Background(), args[0], false)
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

// selftest proves this build can start on this machine: it loads the identity, reaches the platform and is accepted. An update
// runs it on the new build before replacing the running one. The check-in it makes is a normal one (tasks it is handed are
// not run or reported, so the platform offers them again).
func selftest(args []string) error {
	fs := flag.NewFlagSet("selftest", flag.ExitOnError)
	insecure := insecureFlag(fs)
	dir := stateDirFlag(fs)
	_ = fs.Parse(args)
	st, err := state.Load(*dir)
	if err != nil {
		return err
	}
	c, err := client.New(st.PlatformURL, version, *insecure)
	if err != nil {
		return err
	}
	c.Credential = st.Credential
	c.SetFallbacks(st.Fallbacks)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.CheckIn(ctx); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}
