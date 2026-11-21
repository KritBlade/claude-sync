package main

import "testing"

func windowsConverter(t *testing.T) *pathConverter {
	t.Helper()
	converter, err := newPathConverter(`C:\Users\Goten`, map[string]string{`H:\Github\Dev`: "WORK", `H:\Github\Prod`: "PROD"})
	if err != nil {
		t.Fatal(err)
	}
	return converter
}

func macConverter(t *testing.T) *pathConverter {
	t.Helper()
	converter, err := newPathConverter("/Users/goten", map[string]string{"/Users/goten/Github/Dev": "WORK", "/Users/goten/Github/Prod": "PROD"})
	if err != nil {
		t.Fatal(err)
	}
	return converter
}

func TestWindowsFoldersBecomePortableInEverySpelling(t *testing.T) {
	converter := windowsConverter(t)
	cases := []struct{ name, input, want string }{
		{"raw", `cd H:\Github\Dev\VectFox\src`, `cd ${PORTABLE_PATH:WORK}/VectFox/src`},
		{"lowercase drive", `h:\Github\Dev\VectFox`, `${PORTABLE_PATH:WORK}/VectFox`},
		{"escaped once in JSON", `{"Path":"h:\\Github\\Dev\\Vectfox-Alpha\\GEMINI.md"}`, `{"Path":"${PORTABLE_PATH:WORK}/Vectfox-Alpha/GEMINI.md"}`},
		{"escaped twice in JSON", `"H:\\\\Github\\\\Dev\\\\VectFox\\\" -Force`, `"${PORTABLE_PATH:WORK}/VectFox\\\" -Force`},
		{"JSON newline escape after a path", `"H:\\Github\\Dev\\VectFox\nnext"`, `"${PORTABLE_PATH:WORK}/VectFox\nnext"`},
		{"forward slashes", `H:/Github/Dev/VectFox/a.md`, `${PORTABLE_PATH:WORK}/VectFox/a.md`},
		{"workspace URI", `file:///h%3A/Github/Dev/VectFox`, `file://${PORTABLE_PATH:WORK}/VectFox`},
		{"URI with colon", `[x](file:///h:/Github/Dev/a/b.md)`, `[x](file://${PORTABLE_PATH:WORK}/a/b.md)`},
		{"home with forward slashes", `C:/Users/Goten/.gemini/antigravity/brain/x.jpg`, `${PORTABLE_PATH:HOME}/.gemini/antigravity/brain/x.jpg`},
		{"home URI", `file:///C:/Users/Goten/dev`, `file://${PORTABLE_PATH:HOME}/dev`},
		{"second mapping", `H:\Github\Prod\mvu`, `${PORTABLE_PATH:PROD}/mvu`},
		{"longer folder name is not the folder", `H:\Github\Dev2\x`, `H:\Github\Dev2\x`},
		{"drive letter inside a word", `searcH:\Github\Dev`, `searcH:\Github\Dev`},
		{"unmapped folder", `H:\Apps\SillyTavern`, `H:\Apps\SillyTavern`},
		{"mixed escaping depth", `H:\Github\\Dev\x`, `H:\Github\\Dev\x`},
		{"literal shell variable", `echo ${HOME}`, `echo ${HOME}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, _ := converter.toPortable([]byte(testCase.input))
			if string(got) != testCase.want {
				t.Errorf("toPortable(%s)\n got %s\nwant %s", testCase.input, got, testCase.want)
			}
		})
	}
}

func TestMacFoldersBecomePortable(t *testing.T) {
	converter := macConverter(t)
	cases := []struct{ name, input, want string }{
		{"most specific folder wins", `/Users/goten/Github/Dev/VectFox`, `${PORTABLE_PATH:WORK}/VectFox`},
		{"home", `"cwd":"/Users/goten/dev"`, `"cwd":"${PORTABLE_PATH:HOME}/dev"`},
		{"URI", `file:///Users/goten/Github/Dev/VectFox`, `file://${PORTABLE_PATH:WORK}/VectFox`},
		{"inside a longer path", `/private/Users/goten/x`, `/private/Users/goten/x`},
		{"inside a drive path", `C:/Users/goten/x`, `C:/Users/goten/x`},
		{"longer user name", `/Users/gotenx/a`, `/Users/gotenx/a`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, _ := converter.toPortable([]byte(testCase.input))
			if string(got) != testCase.want {
				t.Errorf("toPortable(%s)\n got %s\nwant %s", testCase.input, got, testCase.want)
			}
		})
	}
}

func TestPortableTokensBecomeEachDevicesFolders(t *testing.T) {
	portable := `${PORTABLE_PATH:WORK}/VectFox file://${PORTABLE_PATH:WORK}/VectFox ${PORTABLE_PATH:HOME}/.gemini ${HOME} ${PORTABLE_PATH:ELSEWHERE}/x`
	mac, unknown := macConverter(t).toLocal([]byte(portable))
	wantMac := `/Users/goten/Github/Dev/VectFox file:///Users/goten/Github/Dev/VectFox /Users/goten/.gemini ${HOME} ${PORTABLE_PATH:ELSEWHERE}/x`
	if string(mac) != wantMac || unknown != 1 {
		t.Errorf("Mac: got %s (unknown %d)\nwant %s (unknown 1)", mac, unknown, wantMac)
	}
	windows, _ := windowsConverter(t).toLocal([]byte(portable))
	wantWindows := `H:/Github/Dev/VectFox file:///h%3A/Github/Dev/VectFox C:/Users/Goten/.gemini ${HOME} ${PORTABLE_PATH:ELSEWHERE}/x`
	if string(windows) != wantWindows {
		t.Errorf("Windows: got %s\nwant %s", windows, wantWindows)
	}
}

func TestWorkspaceURIRoundTripsBetweenDevices(t *testing.T) {
	windows, mac := windowsConverter(t), macConverter(t)
	portable, _ := windows.toPortable([]byte(`file:///h%3A/Github/Dev/VectFox`))
	onMac, _ := mac.toLocal(portable)
	if string(onMac) != `file:///Users/goten/Github/Dev/VectFox` {
		t.Fatalf("on the Mac: %s", onMac)
	}
	portableAgain, _ := mac.toPortable(onMac)
	backOnWindows, _ := windows.toLocal(portableAgain)
	if string(backOnWindows) != `file:///h%3A/Github/Dev/VectFox` {
		t.Fatalf("back on Windows: %s", backOnWindows)
	}
}

func TestHomeTokenIsReserved(t *testing.T) {
	if _, err := newPathConverter("/Users/goten", map[string]string{"/Users/goten/x": "HOME"}); err == nil {
		t.Fatal("mapping a folder to HOME should be refused")
	}
}
