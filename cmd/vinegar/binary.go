package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/godbus/dbus/v5"
	"github.com/lmittmann/tint"
	"github.com/nxadm/tail"
	slogmulti "github.com/samber/slog-multi"
	bsrpc "github.com/vinegarhq/vinegar/bloxstraprpc"
	"github.com/vinegarhq/vinegar/config"
	"github.com/vinegarhq/vinegar/internal/dirs"
	"github.com/vinegarhq/vinegar/internal/state"
	"github.com/vinegarhq/vinegar/roblox"
	boot "github.com/vinegarhq/vinegar/roblox/bootstrapper"
	"github.com/vinegarhq/vinegar/splash"
	"github.com/vinegarhq/vinegar/sysinfo"
	"github.com/vinegarhq/vinegar/wine"
	"golang.org/x/term"
)

const (
	LogTimeout = 6 * time.Second
	DieTimeout = 3 * time.Second
)

const (
	DialogUseBrowser = "WebView/InternalBrowser is broken, please use the browser for the action that you were doing."
	DialogQuickLogin = "WebView/InternalBrowser is broken, use Quick Log In to authenticate ('Log In With Another Device' button)"
	DialogFailure    = "Vinegar experienced an error:\n%s"
	DialogNoAVX      = "Warning: Your CPU does not support AVX. While some people may be able to run without it, most are not able to. VinegarHQ cannot provide support for your installation. Continue?"
)

type Binary struct {
	// Only initialized in Main
	Splash *splash.Splash

	GlobalState *state.State
	State       *state.Binary

	GlobalConfig *config.Config
	Config       *config.Binary

	Alias  string
	Name   string
	Dir    string
	Prefix *wine.Prefix
	Type   roblox.BinaryType
	Deploy *boot.Deployment

	// Logging
	Auth     bool
	Activity bsrpc.Activity
}

func BinaryPrefixDir(bt roblox.BinaryType) string {
	return filepath.Join(dirs.Prefixes, strings.ToLower(bt.String()))
}

func NewBinary(bt roblox.BinaryType, cfg *config.Config) (*Binary, error) {
	var bcfg *config.Binary
	var bstate *state.Binary

	s, err := state.Load()
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	switch bt {
	case roblox.Player:
		bcfg = &cfg.Player
		bstate = &s.Player
	case roblox.Studio:
		bcfg = &cfg.Studio
		bstate = &s.Studio
	}

	pfx, err := wine.New(BinaryPrefixDir(bt), bcfg.WineRoot)
	if err != nil {
		return nil, fmt.Errorf("new prefix %s: %w", bt, err)
	}

	os.Setenv("GAMEID", "ulwgl-roblox")

	return &Binary{
		Activity: bsrpc.New(),

		GlobalState: &s,
		State:       bstate,

		GlobalConfig: cfg,
		Config:       bcfg,

		Alias:  bt.String(),
		Name:   bt.BinaryName(),
		Type:   bt,
		Prefix: pfx,
	}, nil
}

func (b *Binary) Main(args ...string) int {
	logFile, err := LogFile(b.Type.String())
	if err != nil {
		slog.Error(fmt.Sprintf("create log file: %s", err))
		return 1
	}
	defer logFile.Close()

	slog.SetDefault(slog.New(slogmulti.Fanout(
		tint.NewHandler(os.Stderr, nil),
		tint.NewHandler(logFile, &tint.Options{NoColor: true}),
	)))

	b.Splash = splash.New(&b.GlobalConfig.Splash)
	b.Config.Env.Setenv()

	go func() {
		err := b.Splash.Run()
		if errors.Is(splash.ErrClosed, err) {
			slog.Warn("Splash window closed!")

			// Will tell Run() to immediately kill Roblox, as it handles INT/TERM.
			// Otherwise, it will just with the same appropiate signal.
			syscall.Kill(syscall.Getpid(), syscall.SIGINT)
			return
		}

		// The splash window didn't close cleanly (ErrClosed), an
		// internal error occured, and vinegar cannot continue.
		if err != nil {
			slog.Error(fmt.Sprintf("splash: %s", err))
			logFile.Close()
			os.Exit(1)
		}
	}()

	err = b.Run(args...)
	if err != nil {
		slog.Error(err.Error())

		if b.GlobalConfig.Splash.Enabled && !term.IsTerminal(int(os.Stderr.Fd())) {
			b.Splash.LogPath = logFile.Name()
			b.Splash.SetMessage("Oops!")
			b.Splash.Dialog(fmt.Sprintf(DialogFailure, err), false) // blocks
		}

		return 1
	}

	return 0
}

func (b *Binary) Run(args ...string) error {
	if err := b.Init(); err != nil {
		return fmt.Errorf("init %s: %w", b.Type, err)
	}

	if len(args) == 1 && args[0] == "roblox-" {
		b.HandleProtocolURI(args[0])
	}

	b.Splash.SetDesc(b.Config.Channel)

	if err := b.Setup(); err != nil {
		return fmt.Errorf("failed to setup roblox: %w", err)
	}

	if err := b.Execute(args...); err != nil {
		return fmt.Errorf("failed to run roblox: %w", err)
	}

	return nil
}

func (b *Binary) Init() error {
	firstRun := false
	if _, err := os.Stat(filepath.Join(b.Prefix.Dir(), "drive_c", "windows")); err != nil {
		firstRun = true
	}

	if firstRun && !sysinfo.CPU.AVX {
		b.Splash.Dialog(DialogNoAVX, false)
		slog.Warn("Running roblox without AVX, Roblox will most likely fail to run!")
	}

	// Command-line flag vs wineprefix initialized
	if firstRun || FirstRun {
		slog.Info("Initializing wineprefix", "dir", b.Prefix.Dir())
		b.Splash.SetMessage("Initializing wineprefix")

		var err error
		switch b.Type {
		case roblox.Player:
			err = b.Prefix.Init()
		case roblox.Studio:
			// Studio accepts all DPIs except the default, which is 96.
			// Technically this is 'initializing wineprefix', as SetDPI calls Wine which
			// automatically create the Wineprefix.
			err = b.Prefix.SetDPI(97)
		}

		if err != nil {
			return fmt.Errorf("failed to init %s prefix: %w", b.Type, err)
		}

		if err := b.InstallWebView(); err != nil {
			return fmt.Errorf("failed to install webview: %w", err)
		}
	}

	return nil
}

func (b *Binary) HandleProtocolURI(mime string) {
	uris := strings.Split(mime, "+")
	for _, uri := range uris {
		kv := strings.Split(uri, ":")

		if len(kv) == 2 && kv[0] == "channel" {
			c := kv[1]
			if c == "" {
				continue
			}

			slog.Warn("Roblox has requested a user channel, changing...", "channel", c)
			b.Config.Channel = c
		}
	}
}

func (b *Binary) Execute(args ...string) error {
	if b.Config.DiscordRPC {
		if err := b.Activity.Connect(); err != nil {
			slog.Error("Could not connect to Discord RPC", "error", err)
			b.Config.DiscordRPC = false
		} else {
			defer b.Activity.Close()
		}
	}

	// Studio can run in multiple instances, not Player
	if b.GlobalConfig.MultipleInstances && b.Type == roblox.Player {
		slog.Info("Running robloxmutexer")

		mutexer := b.Prefix.Wine(filepath.Join(BinPrefix, "robloxmutexer.exe"))
		if err := mutexer.Start(); err != nil {
			return fmt.Errorf("start robloxmutexer: %w", err)
		}
		go func() {
			if err := mutexer.Wait(); err != nil {
				slog.Error("robloxmutexer returned too early", "error", err)
			}
		}()
	}

	cmd, err := b.Command(args...)
	if err != nil {
		return fmt.Errorf("%s command: %w", b.Type, err)
	}

	// Roblox will keep running if it was sent SIGINT; requiring acting as the signal holder.
	// SIGUSR1 is used in Tail() to force kill roblox, used to differenciate between
	// a user-sent signal and a self sent signal.
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGUSR1)
	go func() {
		s := <-c

		slog.Warn("Recieved signal", "signal", s)

		// Only kill Roblox if it hasn't exited
		if cmd.ProcessState == nil {
			slog.Warn("Killing Roblox", "pid", cmd.Process.Pid)
			// This way, cmd.Run() will return and vinegar (should) exit.
			cmd.Process.Kill()
		}

		// Don't handle INT after it was recieved, this way if another signal was sent,
		// Vinegar will immediately exit.
		signal.Stop(c)
	}()

	slog.Info("Running Binary", "name", b.Name, "cmd", cmd)
	b.Splash.SetMessage("Launching " + b.Alias)

	go func() {
		// Wait for process to start
		for {
			if cmd.Process != nil {
				break
			}
		}

		// If the log file wasn't found, assume failure
		// and don't perform post-launch roblox functions.
		lf, err := RobloxLogFile(b.Prefix)
		if err != nil {
			slog.Error("Failed to find Roblox log file", "error", err.Error())
			return
		}

		b.Splash.Close()

		if b.Config.GameMode {
			b.RegisterGameMode(int32(cmd.Process.Pid))
		}

		// Blocks and tails file forever until roblox is dead, unless
		// if finding the log file had failed.
		b.Tail(lf)
	}()

	if err := cmd.Run(); err != nil {
		// thanks for your time, fizzie on #go-nuts
		// Killed, not an error (in most cases)
		if cmd.ProcessState.ExitCode() == -1 {
			slog.Warn("Roblox was killed!")
			return nil
		}
		return fmt.Errorf("roblox process: %w", err)
	}

	return nil
}

func RobloxLogFile(pfx *wine.Prefix) (string, error) {
	ad, err := pfx.AppDataDir()
	if err != nil {
		return "", fmt.Errorf("get appdata: %w", err)
	}

	dir := filepath.Join(ad, "Local", "Roblox", "logs")

	// This is required due to fsnotify requiring the directory
	// to watch to exist before adding it.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create roblox log dir: %w", err)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return "", fmt.Errorf("make fsnotify watcher: %w", err)
	}
	defer w.Close()

	if err := w.Add(dir); err != nil {
		return "", fmt.Errorf("watch roblox log dir: %w", err)
	}

	t := time.NewTimer(LogTimeout)

	for {
		select {
		case <-t.C:
			return "", fmt.Errorf("roblox log file not found after %s", LogTimeout)
		case e := <-w.Events:
			if e.Has(fsnotify.Create) {
				return e.Name, nil
			}
		case err := <-w.Errors:
			slog.Error("Recieved fsnotify watcher error", "error", err)
		}
	}
}

func (b *Binary) Tail(name string) {
	t, err := tail.TailFile(name, tail.Config{Follow: true})
	if err != nil {
		slog.Error("Could not tail Roblox log file", "error", err)
		return
	}

	for line := range t.Lines {
		fmt.Fprintln(b.Prefix.Stderr, line.Text)

		// Roblox shut down, give it atleast a few seconds, and then send an
		// internal signal to kill it.
		// This is due to Roblox occasionally refusing to die. We must kill it.
		if strings.Contains(line.Text, "[FLog::SingleSurfaceApp] shutDown:") {
			go func() {
				time.Sleep(DieTimeout)
				syscall.Kill(syscall.Getpid(), syscall.SIGUSR1)
			}()
		}

		if b.Config.DiscordRPC {
			if err := b.Activity.HandleRobloxLog(line.Text); err != nil {
				slog.Error("Activity Roblox log handle failed", "error", err)
			}
		}
	}
}

func (b *Binary) Command(args ...string) (*wine.Cmd, error) {
	if strings.HasPrefix(strings.Join(args, " "), "roblox-studio:1") {
		args = []string{"-protocolString", args[0]}
	}

	cmd := b.Prefix.Wine(filepath.Join(b.Dir, b.Type.Executable()), args...)

	launcher := strings.Fields(b.Config.Launcher)
	if len(launcher) >= 1 {
		cmd.Args = append(launcher, cmd.Args...)
		p, err := b.Config.LauncherPath()
		if err != nil {
			return nil, fmt.Errorf("bad launcher: %w", err)
		}
		cmd.Path = p
	}

	return cmd, nil
}

func (b *Binary) RegisterGameMode(pid int32) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		slog.Error("Failed to connect to D-Bus", "error", err)
		return
	}

	desktop := conn.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop")

	call := desktop.Call("org.freedesktop.portal.GameMode.RegisterGame", 0, pid)
	if call.Err != nil && !errors.Is(call.Err, dbus.ErrMsgNoObject) {
		slog.Error("Failed to register to GameMode", "error", err)
		return
	}
}

func LogFile(name string) (*os.File, error) {
	if err := dirs.Mkdirs(dirs.Logs); err != nil {
		return nil, err
	}

	// name-2006-01-02T15:04:05Z07:00.log
	path := filepath.Join(dirs.Logs, name+"-"+time.Now().Format(time.RFC3339)+".log")

	file, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("failed to create %s log file: %w", name, err)
	}

	slog.Info("Logging to file", "path", path)

	return file, nil
}
