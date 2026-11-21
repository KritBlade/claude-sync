package main

import (
	"reflect"
	"testing"
)

func TestParseWindowsProcessesKeepsHubsAndAppOnly(t *testing.T) {
	output := []byte(`[` +
		`{"ProcessId":50936,"Name":"agy.exe","CommandLine":"C:\\Users\\Someone\\.gemini\\bin\\agy.exe --hub --hub-port=5229 --app_data_dir=antigravity --add-dir=h:\\Github\\Dev\\Project"},` +
		`{"ProcessId":46264,"Name":"agy.exe","CommandLine":"\"C:\\Program Files\\agy\\agy.exe\" --hub --app_data_dir antigravity"},` +
		`{"ProcessId":31000,"Name":"agy.exe","CommandLine":"C:\\Users\\Someone\\.gemini\\bin\\agy.exe"},` +
		`{"ProcessId":32000,"Name":"agy.exe","CommandLine":null},` +
		`{"ProcessId":33000,"Name":"Antigravity.exe","CommandLine":"C:\\Programs\\Antigravity\\Antigravity.exe"},` +
		`{"ProcessId":72944,"Name":"agy.exe","CommandLine":"agy.exe --hub --app_data_dir=antigravity"}` +
		`]` + "\r\n")

	processes, err := parseWindowsProcesses(output, 72944)
	if err != nil {
		t.Fatalf("parseWindowsProcesses: %v", err)
	}
	expected := []antigravityProcess{
		{processID: 50936, description: "agy hub", isHub: true},
		{processID: 46264, description: "agy hub", isHub: true},
		{processID: 33000, description: "Antigravity app"},
	}
	if !reflect.DeepEqual(processes, expected) {
		t.Errorf("processes = %+v, expected %+v", processes, expected)
	}
}

func TestParseWindowsProcessesReadsAnEmptyList(t *testing.T) {
	processes, err := parseWindowsProcesses([]byte("[]\r\n"), 1)
	if err != nil {
		t.Fatalf("parseWindowsProcesses: %v", err)
	}
	if len(processes) != 0 {
		t.Errorf("processes = %+v, expected none", processes)
	}
}

func TestParseWindowsProcessesRejectsOtherOutput(t *testing.T) {
	if _, err := parseWindowsProcesses([]byte("Get-CimInstance : Access denied"), 1); err == nil {
		t.Error("expected an error for output that is not a JSON array")
	}
}
