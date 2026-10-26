package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckEnvFileSyntax(t *testing.T) {
	const contents = "\xef\xbb\xbf# comment\r\n" +
		"\r\n" +
		" export PVE_TOKEN_ID = 'pve-csi@pve!csi-dev' # comment\r\n" +
		"UNQUOTED =   value with spaces   # trailing comment\r\n" +
		"SINGLE='  literal $HOME # spaces  '\r\n" +
		"DOUBLE=\"  literal ${HOME} # spaces  \"\r\n" +
		"HASH=value#literal\r\n" +
		"HASH_START=#literal\r\n" +
		"EMPTY_COMMENT= # this is a comment\r\n" +
		"EMPTY=\r\n" +
		"EMPTY_SINGLE=''\r\n" +
		"EMPTY_DOUBLE=\"\"\r\n" +
		"DUPLICATE=first\r\nDUPLICATE=last\r\n" +
		"exported=value\r\n" +
		"export=ordinary-key\r\n" +
		"ESCAPED=\"quote: \\\" slash: \\\\ literal: \\n\"\r\n" +
		"SINGLE_SLASH='C:\\literal\\path'\r\n" +
		"EQUALS=a=b=c\r\n" +
		"UNICODE='中文 token'\r\n" +
		"_KEY2=valid"
	writeCheckEnvTestFile(t, contents)
	get, err := loadCheckEnvFile(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"PVE_TOKEN_ID": "pve-csi@pve!csi-dev",
		"UNQUOTED":     "value with spaces", "SINGLE": "  literal $HOME # spaces  ", "DOUBLE": "  literal ${HOME} # spaces  ",
		"HASH": "value#literal", "HASH_START": "#literal", "EMPTY_COMMENT": "", "EMPTY": "", "EMPTY_SINGLE": "", "EMPTY_DOUBLE": "",
		"DUPLICATE": "last", "exported": "value", "export": "ordinary-key",
		"ESCAPED": "quote: \" slash: \\ literal: \\n", "SINGLE_SLASH": `C:\literal\path`,
		"EQUALS": "a=b=c", "UNICODE": "中文 token", "_KEY2": "valid", "ABSENT": "",
	} {
		if got := get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestCheckEnvFilePrecedenceAndIsolation(t *testing.T) {
	const key = "PVE_CSI_CHECK_ENVFILE_TEST_VALUE"
	t.Setenv(key, "process-value")
	writeCheckEnvTestFile(t, key+"=file-value\nFILE_ONLY=file-only\nEMPTY_PROCESS=file-fallback\nDUP=first\nDUP=\n")
	getenv := func(name string) string {
		if name == "PROCESS_ONLY" {
			return "process-only"
		}
		return os.Getenv(name)
	}
	before := strings.Join(os.Environ(), "\n")
	get, err := loadCheckEnvFile(getenv)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{key: "process-value", "FILE_ONLY": "file-only", "EMPTY_PROCESS": "file-fallback", "PROCESS_ONLY": "process-only", "DUP": ""} {
		if got := get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if after := strings.Join(os.Environ(), "\n"); after != before {
		t.Fatal("loading env file mutated the process environment")
	}
}

func TestCheckEnvFileNoShellEvaluation(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "command-must-not-run")
	command := "$(touch " + marker + ")"
	backtick := "`touch " + marker + "`"
	contents := fmt.Sprintf("PLAIN=%s\nQUOTED=\"%s\"\nBACKTICK='%s'\nVARIABLE=$HOME/${HOME}\n", command, command, backtick)
	writeCheckEnvTestFile(t, contents)
	get, err := loadCheckEnvFile(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"PLAIN": command, "QUOTED": command, "BACKTICK": backtick, "VARIABLE": "$HOME/${HOME}"} {
		if got := get(name); got != want {
			t.Errorf("shell syntax was expanded for %s: %q", name, got)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("command substitution ran or marker check failed: %v", err)
	}
}

func TestCheckEnvFileMalformedInputRedactsValues(t *testing.T) {
	const secret = "test-secret-never-echo"
	for _, line := range []string{
		secret,
		"1KEY=" + secret,
		"BAD-KEY=" + secret,
		"BAD KEY=" + secret,
		"=" + secret,
		"KEY='" + secret,
		"KEY=\"" + secret,
		"KEY='" + secret + "'trailing",
		"KEY=\"" + secret + "\"#no-whitespace",
		"KEY=" + secret + "'quote",
		"KEY=" + secret + "\x00",
		"KEY=" + secret + "\rinside",
		"KEY='" + secret + "\ncontinued'",
	} {
		t.Run(fmt.Sprintf("case-%d", len(line)), func(t *testing.T) {
			path := writeCheckEnvTestFile(t, "# first line\n"+line+"\n")
			get, err := loadCheckEnvFile(func(string) string { return "" })
			if err == nil || get != nil {
				t.Fatal("malformed file accepted")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "line 2") {
				t.Fatalf("error must give only a safe line number and authored detail: %v", err)
			}
		})
	}
}

func TestCheckEnvFileMissingAndUnreadable(t *testing.T) {
	getenv := func(key string) string { return "process:" + key }
	for _, tc := range []struct {
		name      string
		directory bool
	}{
		{"optional missing", false},
		{"unreadable directory", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			if tc.directory {
				if err := os.Mkdir(".env", 0700); err != nil {
					t.Fatal(err)
				}
			}
			get, err := loadCheckEnvFile(getenv)
			if (err != nil) != tc.directory {
				t.Fatalf("error = %v, wantError = %t", err, tc.directory)
			}
			if err != nil {
				if strings.Contains(err.Error(), dir) || err.Error() != "cannot read env file" {
					t.Fatalf("unexpected unsafe read error: %v", err)
				}
			} else if got := get("KEY"); got != "process:KEY" {
				t.Fatalf("fallback getter changed: %q", got)
			}
		})
	}
}

func TestCheckEnvFileSizeLimit(t *testing.T) {
	for _, size := range []int{maxCheckEnvFileBytes, maxCheckEnvFileBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			contents := "KEY=" + strings.Repeat("x", size-len("KEY="))
			writeCheckEnvTestFile(t, contents)
			get, err := loadCheckEnvFile(func(string) string { return "" })
			if size <= maxCheckEnvFileBytes {
				if err != nil || len(get("KEY")) != size-len("KEY=") {
					t.Fatalf("exact-limit file was not read: %v", err)
				}
			} else if err == nil || get != nil || err.Error() != "env file exceeds the 1 MiB size limit" {
				t.Fatalf("oversize file accepted or unsafe error: %v", err)
			}
		})
	}
}

func writeCheckEnvTestFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
