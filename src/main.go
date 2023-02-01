// Copyright vinegar-development 2023

//TODO: Add watchdog for unlocker in flatpak? This needs investigation.
package main

import (
	"fmt"
	"github.com/nocrusts/vinegar/src/util"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

// Gluecode for exec library
func Exec(dirs *util.Dirs, prog string, args ...string) { // Thanks, wael.
	cmd := exec.Command(prog, args...)

	stdoutFile, err := os.Create(filepath.Join(dirs.Log, "std.out"))
	log.Println("Forwarding stdout to", filepath.Join(dirs.Log, "std.out"))
	util.Errc(err)
	defer stdoutFile.Close()

	stderrFile, err := os.Create(filepath.Join(dirs.Log, "std.err"))
	log.Println("Forwarding stderr to", filepath.Join(dirs.Log, "std.err"))
	util.Errc(err)
	defer stderrFile.Close()

	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile

	err = cmd.Run()

	// FIXME: make error comprehensible.
	util.Errc(err)
}

// Handle requests for launch
func launch(args ...string) {
	dirs := util.InitDirs() // pointer to instance of Dirs
	util.InitEnvironment(dirs)
	util.CheckExecutables(dirs)
	launcherexe := filepath.Join(dirs.Exe, "RobloxPlayerLauncher.exe")
	studioexe := filepath.Join(dirs.Exe, "RobloxStudioLauncherBeta.exe")
	fpsunlockerexe := filepath.Join(dirs.Cache, "rbxfpsunlocker.exe")
	// Wine is initialized on first launch automatically.
	switch len(args) {
	case 1:
		if args[0] == "app" {
			Exec(dirs, "wine", launcherexe, " -app", " -fast")
			Exec(dirs, "wine", fpsunlockerexe)
		} else if args[0] == "reset" {
			util.ResetPrefix(dirs)
			log.Println("Prefix erased. A new one will be created the next time you launch any game mode.")
		} else if args[0] == "studio" {
			Exec(dirs, "wine", studioexe)
		}
	case 2:
		if args[0] == "player" {
			Exec(dirs, "wine", launcherexe, " -fast " , args[1])
			Exec(dirs, "wine", fpsunlockerexe)
		} else if args[0] == "studio" {
			Exec(dirs, "wine", studioexe, " ", args[1])
		}
	}
}

// Command argument processing
func main() {
	// Handle arguments here
	// Ex: vinegar MODE [optional launcharg]

	args := os.Args[1:]
	argsLen := len(args) // First argument is MODE

	switch argsLen {
	case 0:
		fmt.Println("usage: vinegar <MODE> [launch string] \n")
		fmt.Println("Available game modes are: app, player, studio\n")
		fmt.Println("Available maintenance modes are: reset\n")
	case 1:
		if args[0] == "app" {
			fmt.Println("Launching app")
			launch(args[0])
		} else if args[0] == "player" {
			fmt.Println("Can't run this mode without launch argument!")
			os.Exit(2)
		} else if args[0] == "reset" {
			fmt.Println("Regenerating prefix")
			launch(args[0])
		} else if args[0] == "studio" {
			fmt.Println("Launching studio without specific game.")
			launch(args[0])
		} else {
			fmt.Println("Unrecognized mode!")
		}
	case 2:
		if args[0] == "studio" || args[0] == "player" {
			fmt.Println("Starting " + args[0] + " with argument " + args[1])
			launch(args[0], args[1])
		} else if args[0] == "app" {
			fmt.Println("app cannot be launched with additional arguments!")
		} else {
			fmt.Println("Unrecognized mode!")
		}
	}
}
