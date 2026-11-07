// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Command relay is Field Day Commander Relay: it collects contacts from local
// logging software and sends them to a Field Day Commander server.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/app"
	"github.com/ckoval7/fielddaycommander-relay/internal/autostart"
	"github.com/ckoval7/fielddaycommander-relay/internal/filepicker"
	"github.com/ckoval7/fielddaycommander-relay/internal/instance"
	"github.com/ckoval7/fielddaycommander-relay/internal/secret"
	"github.com/ckoval7/fielddaycommander-relay/internal/tray"
	"github.com/ckoval7/fielddaycommander-relay/internal/ui"
)

// version is set when a release is built.
var version = "dev"

// portable is "true" in the portable build, which keeps its data beside the program
// and adds nothing to what starts at sign-in.
var portable = ""

const (
	dataFolder = "Field Day Commander Relay"
	// portableFolder is beside the portable build's program.
	portableFolder = "relay-data"
	// portableIDFile names this portable folder's token in the keychain.
	portableIDFile = "keychain.id"
	// iconResource is the icon group in rsrc_windows_amd64.syso, built from
	// assets/relay.ico.
	iconResource = 1
	logFile      = "relay.log"
	// portFile says where the page is, in case the usual port was taken.
	portFile         = "page.port"
	crashFile        = "crash.log"
	maxLogBytes      = 5 << 20
	preferredPort    = 17311
	findPageAttempts = 5
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Field Day Commander Relay:", err)
		os.Exit(1)
	}
}

func run() error {
	dataDirFlag := flag.String("data-dir", "", "folder for the relay's data, instead of the usual one")
	noBrowser := flag.Bool("no-browser", false, "don't open the status page at startup")
	flag.Parse()

	isPortable := portable == "true"
	program := ""
	if isPortable {
		var err error
		if program, err = os.Executable(); err != nil {
			return fmt.Errorf("find the program's folder: %w", err)
		}
	}
	dataDir, besideProgramErr, err := resolveDataDir(*dataDirFlag, program)
	if err != nil {
		return err
	}
	log, closeLog, err := openLog(dataDir)
	if err != nil {
		return err
	}
	defer closeLog()
	if besideProgramErr != nil {
		log.Warn("couldn't keep data beside the program, so it's in the usual folder", "error", besideProgramErr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The person started it again, most likely looking for the page. A start at
	// sign-in isn't looking for anything.
	alreadyRunning := func() {
		log.Info("another relay is running, so this one is stopping")
		if !*noBrowser {
			openRunningPage(ctx, dataDir, log)
		}
	}

	release, err := instance.Acquire()
	if errors.Is(err, instance.ErrHeld) {
		alreadyRunning()
		return nil
	}
	if err != nil {
		// Collecting contacts matters more than this check, and the port check is left.
		log.Warn("couldn't check for another relay", "error", err)
	} else {
		defer release()
	}

	if err := captureCrashes(dataDir); err != nil {
		log.Warn("couldn't set up the crash log", "error", err)
	}

	listener, err := listen(ctx)
	if errors.Is(err, errAlreadyRunning) {
		alreadyRunning()
		return nil
	}
	if err != nil {
		return err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	url := pageURL(port)
	savePort(dataDir, port, log)

	target := secret.DefaultTarget
	var starter autostart.Starter
	if isPortable {
		if target, err = portableTarget(dataDir); err != nil {
			_ = listener.Close()
			return err
		}
	} else {
		starter = startAtSignIn(*dataDirFlag, log)
	}
	relay, err := app.New(app.Config{
		DataDir:     dataDir,
		Version:     version,
		Store:       secret.New(target),
		Log:         log,
		PickLogFile: pickLogFile,
		Autostart:   starter,
	})
	if err != nil {
		_ = listener.Close()
		return err
	}
	page, err := ui.New(relay, log)
	if err != nil {
		_ = listener.Close()
		return err
	}
	log.Info("relay started", "version", version, "page", url)

	var running sync.WaitGroup
	running.Go(func() {
		if err := relay.Run(ctx); err != nil {
			log.Error("closing the relay's data failed", "error", err)
		}
	})
	running.Go(func() {
		if err := page.Serve(ctx, listener); err != nil {
			log.Error("the status page stopped", "error", err)
			stop()
		}
	})

	if !*noBrowser {
		openBrowser(url, log)
	}

	// If the tray can't start, the relay keeps working without it: collecting
	// contacts matters more than an icon.
	err = tray.Run(ctx, tray.Options{
		IconResource: iconResource,
		Tooltip:      ui.TrayText.Tooltip,
		OpenLabel:    ui.TrayText.Open,
		QuitLabel:    ui.TrayText.Quit,
		OnOpen:       func() { openBrowser(url, log) },
		OnQuit:       stop,
	})
	if err != nil {
		log.Error("the tray icon failed", "error", err)
		<-ctx.Done()
	}
	stop()

	running.Wait()
	log.Info("relay stopped")
	return nil
}

// startAtSignIn returns this program's startup entry, or nil if there can't be one.
// An override has to come along, or the relay would come back with other data.
func startAtSignIn(dataDirOverride string, log *slog.Logger) autostart.Starter {
	program, err := os.Executable()
	if err != nil {
		log.Warn("couldn't find this program for starting at sign-in", "error", err)
		return nil
	}
	if dataDirOverride != "" {
		if dataDirOverride, err = filepath.Abs(dataDirOverride); err != nil {
			log.Warn("couldn't find the data folder for starting at sign-in", "error", err)
			return nil
		}
	}
	return autostart.New(program, dataDirOverride)
}

// resolveDataDir picks the data folder. program is set in the portable build. If its
// folder can't be written, as on a locked drive, the relay still has to collect
// contacts, so it uses the usual folder and besideProgramErr says why.
func resolveDataDir(override, program string) (dir string, besideProgramErr, err error) {
	if override != "" {
		dir = override
	} else if program != "" {
		dir = filepath.Join(filepath.Dir(program), portableFolder)
		if besideProgramErr = checkWritable(dir); besideProgramErr == nil {
			return dir, nil, nil
		}
		dir = ""
	}
	if dir == "" {
		config, err := os.UserConfigDir()
		if err != nil {
			return "", besideProgramErr, fmt.Errorf("find the data folder: %w", err)
		}
		dir = filepath.Join(config, dataFolder)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", besideProgramErr, fmt.Errorf("create the data folder: %w", err)
	}
	return dir, besideProgramErr, nil
}

func checkWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	probe, err := os.CreateTemp(dir, "write-check-*")
	if err != nil {
		return err
	}
	_ = probe.Close()
	return os.Remove(probe.Name())
}

// portableTarget names this folder's token in the keychain, which every copy on a
// computer shares. With one name, connecting a portable copy would replace another
// copy's token, and that copy would then send it to its own server. The ID moves
// with the folder, so a new drive letter doesn't mean connecting again.
func portableTarget(dir string) (string, error) {
	path := filepath.Join(dir, portableIDFile)
	id, err := os.ReadFile(path)
	if err != nil || len(strings.TrimSpace(string(id))) == 0 {
		id = []byte(rand.Text())
		if err := os.WriteFile(path, id, 0o600); err != nil {
			return "", fmt.Errorf("save the keychain name: %w", err)
		}
	}
	return secret.DefaultTarget + "/" + strings.TrimSpace(string(id)), nil
}

// openLog starts a log file, setting the old one aside once it's large. There's
// no console on Windows, and the token never reaches a log.
func openLog(dir string) (*slog.Logger, func(), error) {
	path := filepath.Join(dir, logFile)
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogBytes {
		_ = os.Rename(path, path+".old")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open the log: %w", err)
	}
	var out io.Writer = file
	return slog.New(slog.NewTextHandler(out, nil)), func() { _ = file.Close() }, nil
}

// captureCrashes sends a crash's details to a file. A panic normally prints to the
// console, and the Windows build doesn't have one, so it would vanish. Earlier
// crashes stay in the file, since nobody may look until after the event.
func captureCrashes(dir string) error {
	file, err := os.OpenFile(filepath.Join(dir, crashFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	return debug.SetCrashOutput(file, debug.CrashOptions{})
}

var errAlreadyRunning = errors.New("the relay is already running")

// listen takes the usual port, so the page's address stays the same. If another
// relay holds it there's nothing more to do, and if something else does, any free
// port will serve.
func listen(ctx context.Context) (net.Listener, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(preferredPort)))
	if err == nil {
		return listener, nil
	}
	if relayIsAnswering(ctx, preferredPort) {
		return nil, errAlreadyRunning
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// savePort notes where this relay's page is. It's left behind when the relay stops,
// since whoever reads it asks that port what it is.
func savePort(dir string, port int, log *slog.Logger) {
	if err := os.WriteFile(filepath.Join(dir, portFile), []byte(strconv.Itoa(port)), 0o600); err != nil {
		log.Warn("couldn't save the page's port", "error", err)
	}
}

// savedPort is the port a relay using dir last noted, or 0.
func savedPort(dir string) int {
	data, err := os.ReadFile(filepath.Join(dir, portFile))
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || port < 1 || port > 65535 {
		return 0
	}
	return port
}

// openRunningPage shows the page of the relay already running. That relay may be
// starting at the same moment, so it gets a few seconds to answer.
func openRunningPage(ctx context.Context, dir string, log *slog.Logger) {
	for attempt := range findPageAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
		for _, port := range []int{preferredPort, savedPort(dir)} {
			if port != 0 && relayIsAnswering(ctx, port) {
				openBrowser(pageURL(port), log)
				return
			}
		}
	}
	log.Warn("another relay is running, but its page isn't answering")
}

func relayIsAnswering(ctx context.Context, port int) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL(port)+"health", nil)
	if err != nil {
		return false
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 256))
	return err == nil && strings.TrimSpace(string(body)) == ui.HealthMarker
}

func pageURL(port int) string {
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + "/"
}

func openBrowser(url string, log *slog.Logger) {
	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		command = exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "rundll32.exe"), "url.dll,FileProtocolHandler", url)
	} else {
		command = exec.Command("xdg-open", url)
	}
	if err := command.Start(); err != nil {
		log.Warn("couldn't open a browser", "error", err)
		return
	}
	go func() { _ = command.Wait() }()
}

// pickLogFile starts in the folder where WSJT-X or JTDX keeps its log, if there is
// one: Windows hides that folder, so few people could find it themselves.
func pickLogFile() (string, error) {
	var folders []string
	if local, err := os.UserCacheDir(); err == nil {
		folders = []string{filepath.Join(local, "WSJT-X"), filepath.Join(local, "JTDX")}
	}
	return filepicker.Open(filepicker.Options{
		Title:          ui.PickerText.Title,
		FilterName:     ui.PickerText.LogFiles,
		Patterns:       "*.adi;*.adif",
		AllFilesName:   ui.PickerText.AllFiles,
		DefaultFolders: folders,
	})
}
