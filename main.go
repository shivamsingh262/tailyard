// tailyard runs a local dev stack and shows its logs, HTTP traffic and
// Redis queues in one terminal UI.
package main

import (
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/shivamsingh262/tailyard/internal/app"
	"github.com/shivamsingh262/tailyard/internal/config"
	"github.com/shivamsingh262/tailyard/internal/theme"
	"github.com/shivamsingh262/tailyard/internal/ui"
)

//go:embed examples/tailyard.yml
var exampleConfig []byte

// version is set at release time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	configPath := flag.String("config", "", "config file (default: "+config.FileNames[0]+" in this folder or a parent)")
	initConfig := flag.Bool("init", false, "write an example "+config.FileNames[0]+" here and exit")
	check := flag.Bool("check", false, "validate the config and exit")
	build := flag.Bool("build", false, "pass --build to the initial docker compose up")
	noUp := flag.Bool("no-up", false, "don't run docker compose up; follow running containers and leave them running on quit")
	keepRunning := flag.Bool("keep-running", false, "leave the containers running when quitting (d always does)")
	themeName := flag.String("theme", "", "dark or light (overrides the config)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	switch {
	case *showVersion:
		fmt.Println("tailyard", version)
		return
	case *initConfig:
		if err := writeExample(config.FileNames[0]); err != nil {
			fail(err)
		}
		fmt.Println("wrote " + config.FileNames[0] + "; edit it, then run tailyard")
		return
	}

	path := *configPath
	if path == "" {
		wd, err := os.Getwd()
		if err != nil {
			fail(err)
		}
		if path, err = config.Find(wd); err != nil {
			fail(err)
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		fail(err)
	}
	if *check {
		fmt.Printf("%s: OK (%d sources, %d tabs, %d actions)\n", path, len(cfg.Sources), len(cfg.Tabs), len(cfg.Actions))
		return
	}
	if *themeName != "" {
		cfg.Theme = *themeName
	}
	if cfg.Compose != nil {
		if *noUp {
			cfg.Compose.Up = new(bool)
			cfg.Compose.StopOnQuit = new(bool)
		}
		cfg.Compose.Build = cfg.Compose.Build || *build
	}
	stopOnQuit := cfg.ComposeUp() && *cfg.Compose.StopOnQuit && !*keepRunning

	theme.SetLight(cfg.Theme == "light")
	var names []string
	for _, s := range cfg.Sources {
		names = append(names, s.Name)
	}
	theme.RegisterSources(names)

	a, err := app.New(cfg)
	if err != nil {
		fail(err)
	}
	m := ui.New(a, stopOnQuit)
	p := tea.NewProgram(m, tea.WithAltScreen())
	a.Program = p
	go a.Start()

	// Closing the terminal window sends SIGHUP, which Bubble Tea doesn't
	// handle; quit normally so shutdown still stops what tailyard started.
	hangup := make(chan os.Signal, 1)
	signal.Notify(hangup, syscall.SIGHUP)
	go func() {
		<-hangup
		a.HungUp.Store(true)
		p.Quit()
	}()

	if _, err := p.Run(); err != nil && !a.HungUp.Load() {
		fmt.Fprintln(os.Stderr, err)
	}
	a.Shutdown(m.StopOnQuit)
}

func writeExample(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.WriteFile(path, exampleConfig, 0o644)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "tailyard:", err)
	os.Exit(1)
}
