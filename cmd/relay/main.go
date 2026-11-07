// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Command relay is Field Day Commander Relay: it collects contacts from local
// logging software and sends them to a Field Day Commander server.
package main

import (
	"context"
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
	"github.com/ckoval7/fielddaycommander-relay/internal/filepicker"
	"github.com/ckoval7/fielddaycommander-relay/internal/secret"
	"github.com/ckoval7/fielddaycommander-relay/internal/tray"
	"github.com/ckoval7/fielddaycommander-relay/internal/ui"
)

// version is set when a release is built.
var version = "dev"

const (
	dataFolder = "Field Day Commander Relay"
	// iconResource is the icon group in rsrc_windows_amd64.syso, built from
	// assets/relay.ico.
	iconResource  = 1
	logFile       = "relay.log"
	crashFile     = "crash.log"
	maxLogBytes   = 5 << 20
	preferredPort = 17311
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

	dataDir, err := resolveDataDir(*dataDirFlag)
	if err != nil {
		return err
	}
	log, closeLog, err := openLog(dataDir)
	if err != nil {
		return err
	}
	defer closeLog()
	if err := captureCrashes(dataDir); err != nil {
		log.Warn("couldn't set up the crash log", "error", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	listener, err := listen(ctx)
	if errors.Is(err, errAlreadyRunning) {
		// The person started it again, most likely looking for the page.
		openBrowser(pageURL(preferredPort), log)
		return nil
	}
	if err != nil {
		return err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	url := pageURL(port)

	relay, err := app.New(app.Config{DataDir: dataDir, Version: version, Store: secret.New(secret.DefaultTarget), Log: log, PickLogFile: pickLogFile})
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

func resolveDataDir(override string) (string, error) {
	dir := override
	if dir == "" {
		config, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("find the data folder: %w", err)
		}
		dir = filepath.Join(config, dataFolder)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create the data folder: %w", err)
	}
	return dir, nil
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
	if relayIsAnswering(ctx) {
		return nil, errAlreadyRunning
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

func relayIsAnswering(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL(preferredPort)+"health", nil)
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
