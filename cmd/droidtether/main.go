package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/princePal/droidtether/config"
	"github.com/princePal/droidtether/internal/daemon"
)

// Injected by make build: -ldflags="-X main.version=..."
var version = "dev"

// lockPath guards against two daemons claiming the same phone and fighting
// over the utun routes (e.g. a foreground run while launchd also runs one).
const lockPath = "/var/run/droidtether.pid"

// lockFile stays open for the life of the process; the kernel drops the
// flock when the process exits, so a crash never leaves a stale lock.
var lockFile *os.File

func displayVersion() string {
	return "v" + strings.TrimPrefix(version, "v")
}

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("config", "", "path to config file")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: sudo droidtether [--config FILE]\n"+
			"       droidtether --version\n\n"+
			"droidtether is a daemon with no subcommands; it normally runs under launchd.\n"+
			"Use the tether controller instead:\n"+
			"  tether status            show tether status\n"+
			"  sudo tether droid on     start the daemon (launchd, as root)\n"+
			"  sudo tether droid off    stop it\n"+
			"  tether log -f            follow the daemon log\n")
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("droidtether %s\n", displayVersion())
		return
	}

	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "droidtether: unknown argument %q\n\n", flag.Arg(0))
		flag.Usage()
		os.Exit(2)
	}

	// USB access to an Android phone (macOS marks it NeedsDeviceAccessEntitlement,
	// so non-root libusb cannot even enumerate it), utun creation, routes and
	// DNS all need root. Without this check a non-root run waits silently forever.
	if os.Geteuid() != 0 {
		fmt.Fprintf(os.Stderr, "droidtether: must run as root: non-root processes cannot see the phone over USB\n"+
			"or create the utun interface. Start it with:\n"+
			"  sudo tether droid on                      (launchd daemon, recommended)\n"+
			"  sudo droidtether --config /etc/droidtether/droidtether.toml   (foreground)\n")
		os.Exit(1)
	}

	if err := acquireLock(); err != nil {
		fmt.Fprintf(os.Stderr, "droidtether: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("droidtether %s starting...\n", displayVersion())

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "droidtether: config not found or unreadable, trying local default.toml...\n")
		cfg, err = config.Load("config/default.toml")
		if err != nil {
			fmt.Fprintf(os.Stderr, "droidtether: failed to load any configuration: %v\n", err)
			os.Exit(1)
		}
	}

	d := daemon.New(cfg)
	if err := d.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "droidtether: daemon encountered an error: %v\n", err)
		os.Exit(1)
	}
}

// acquireLock takes an exclusive, non-blocking flock on lockPath and records our pid.
func acquireLock() error {
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("cannot open lock file %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		buf := make([]byte, 32)
		n, _ := f.ReadAt(buf, 0)
		f.Close()
		owner := strings.TrimSpace(string(buf[:n]))
		if _, convErr := strconv.Atoi(owner); convErr != nil {
			owner = "unknown"
		}
		return fmt.Errorf("another droidtether is already running (pid %s); "+
			"stop it first with: sudo tether droid off", owner)
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	lockFile = f
	return nil
}
