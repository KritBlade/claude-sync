package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A pull replaces conversation files, so nothing that holds them open may run meanwhile: Antigravity would
// keep the old file open and later write its WAL into the new one. Two kinds of process hold them:
//   - the Antigravity app, which the user closes;
//   - the agy hub that editor extensions start (agy --hub --app_data_dir=antigravity), which can be stopped
//     with --stop-antigravity; the extension starts it again when its panel is next used.

type antigravityProcess struct {
	processID   int
	description string
	isHub       bool
}

func findAntigravityProcesses() ([]antigravityProcess, error) {
	if runtime.GOOS == "windows" {
		return findWindowsAntigravityProcesses()
	}
	return findUnixAntigravityProcesses()
}

func findUnixAntigravityProcesses() ([]antigravityProcess, error) {
	output, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	var processes []antigravityProcess
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		processID, err := strconv.Atoi(fields[0])
		if err != nil || processID == os.Getpid() {
			continue
		}
		command := strings.Join(fields[1:], " ")
		switch {
		case strings.Contains(command, "Antigravity.app/"):
			processes = append(processes, antigravityProcess{processID: processID, description: "Antigravity app"})
		case filepath.Base(fields[1]) == "agy" && usesAntigravityDataDirectory(fields[2:]):
			processes = append(processes, antigravityProcess{processID: processID, description: "agy hub", isHub: true})
		}
	}
	return processes, nil
}

// usesAntigravityDataDirectory reports whether agy's arguments point it at ~/.gemini/antigravity, the folder
// this tool syncs. A plain agy CLI session uses ~/.gemini/antigravity-cli and is left alone.
func usesAntigravityDataDirectory(arguments []string) bool {
	for i, argument := range arguments {
		if argument == "--app_data_dir=antigravity" || (argument == "--app_data_dir" && i+1 < len(arguments) && arguments[i+1] == "antigravity") {
			return true
		}
	}
	return false
}

// windowsProcessQuery prints the Antigravity app and agy processes, with their command lines, as a JSON array.
// tasklist shows no command lines, and only its arguments tell an agy hub from an agy CLI session. The script
// holds no double quote, so Go's Windows argument quoting passes it to PowerShell unchanged.
const windowsProcessQuery = `[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false; ` +
	`ConvertTo-Json -Compress -InputObject @(Get-CimInstance Win32_Process ` +
	`-Filter 'Name = ''agy.exe'' OR Name = ''Antigravity.exe''' | Select-Object ProcessId, Name, CommandLine)`

type windowsProcess struct {
	ProcessID   int    `json:"ProcessId"`
	Name        string `json:"Name"`
	CommandLine string `json:"CommandLine"`
}

func findWindowsAntigravityProcesses() ([]antigravityProcess, error) {
	output, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", windowsProcessQuery).Output()
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	return parseWindowsProcesses(output, os.Getpid())
}

// parseWindowsProcesses reads windowsProcessQuery's output, leaving out ownProcessID.
func parseWindowsProcesses(output []byte, ownProcessID int) ([]antigravityProcess, error) {
	var listed []windowsProcess
	if err := json.Unmarshal(bytes.TrimSpace(output), &listed); err != nil {
		return nil, fmt.Errorf("read process list: %w", err)
	}
	var processes []antigravityProcess
	for _, process := range listed {
		if process.ProcessID == ownProcessID {
			continue
		}
		switch strings.ToLower(process.Name) {
		case "antigravity.exe":
			processes = append(processes, antigravityProcess{processID: process.ProcessID, description: "Antigravity app"})
		case "agy.exe":
			if usesAntigravityDataDirectory(strings.Fields(process.CommandLine)) {
				processes = append(processes, antigravityProcess{processID: process.ProcessID, description: "agy hub", isHub: true})
			}
		}
	}
	return processes, nil
}

// ensureAntigravityStopped returns nil when nothing holds the conversation files. With shouldStopHub it first
// stops running agy hubs and waits up to ten seconds for them to exit; the Antigravity app is never closed for
// the user.
func ensureAntigravityStopped(shouldStopHub bool) error {
	processes, err := findAntigravityProcesses()
	if err != nil {
		return err
	}
	if shouldStopHub {
		for _, process := range processes {
			if process.isHub {
				if err := stopProcess(process.processID); err != nil {
					warnf("stop %s (pid %d): %v", process.description, process.processID, err)
				}
			}
		}
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
			if processes, err = findAntigravityProcesses(); err != nil {
				return err
			}
			if !hasHub(processes) {
				break
			}
		}
	}
	if len(processes) == 0 {
		return nil
	}
	var running []string
	for _, process := range processes {
		running = append(running, fmt.Sprintf("%s (pid %d)", process.description, process.processID))
	}
	hint := "close Antigravity first"
	if hasHub(processes) && !shouldStopHub {
		hint = "close Antigravity first, or run with --stop-antigravity to stop the agy hub (its editor panel starts it again)"
	}
	return fmt.Errorf("Antigravity is running: %s; %s", strings.Join(running, ", "), hint)
}

func hasHub(processes []antigravityProcess) bool {
	for _, process := range processes {
		if process.isHub {
			return true
		}
	}
	return false
}

// stopProcess sends SIGTERM on Unix. On Windows the hub is a console process with no window, which taskkill
// can end only with /F; SQLite's journal keeps the conversation files consistent when a process ends that way.
func stopProcess(processID int) error {
	if runtime.GOOS == "windows" {
		output, err := exec.Command("taskkill", "/F", "/PID", strconv.Itoa(processID)).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	process, err := os.FindProcess(processID)
	if err != nil {
		return err
	}
	return process.Signal(syscall.SIGTERM)
}
