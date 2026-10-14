package cli

import (
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestValidateDaemonTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		tunnelType string
		port       int
		wantErr    bool
	}{
		{name: "http", tunnelType: "http", port: 3000},
		{name: "https", tunnelType: "https", port: 443},
		{name: "tcp", tunnelType: "tcp", port: 22},
		{name: "invalid type", tunnelType: "../http", port: 3000, wantErr: true},
		{name: "invalid port low", tunnelType: "http", port: 0, wantErr: true},
		{name: "invalid port high", tunnelType: "http", port: 70000, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateDaemonTarget(tt.tunnelType, tt.port)
			if tt.wantErr && err == nil {
				t.Fatalf("validateDaemonTarget(%q, %d) expected error", tt.tunnelType, tt.port)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateDaemonTarget(%q, %d) unexpected error: %v", tt.tunnelType, tt.port, err)
			}
		})
	}
}

func TestSanitizeDaemonArgs(t *testing.T) {
	t.Parallel()

	args := []string{
		"http", "3000",
		"--daemon",
		"-d",
		"--daemon=true",
		"--daemon-child",
		"--verbose",
		"--transport", "wss",
	}

	got := sanitizeDaemonArgs(args)
	want := []string{
		"http", "3000",
		"--daemon-child",
		"--verbose",
		"--transport", "wss",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitizeDaemonArgs() = %#v, want %#v", got, want)
	}
}

func TestBuildDaemonArgsWritesSecretsToFileInsteadOfArgv(t *testing.T) {
	resetDaemonSecretTestGlobals(t)
	t.Setenv("HOME", t.TempDir())

	authToken = "server-token-secret"
	authPass = "proxy-password-secret"
	authBearer = "proxy-bearer-secret"
	serverURL = "drip.example:443"

	args, err := buildDaemonArgs("http", []string{"3000"}, "demo", "127.0.0.1")
	if err != nil {
		t.Fatalf("buildDaemonArgs() error = %v", err)
	}

	joined := strings.Join(args, "\x00")
	for _, secret := range []string{authToken, authPass, authBearer} {
		if strings.Contains(joined, secret) {
			t.Fatalf("daemon argv contains secret %q: %#v", secret, args)
		}
	}
	for _, forbiddenFlag := range []string{"--token", "--auth", "--auth-bearer"} {
		if containsArg(args, forbiddenFlag) {
			t.Fatalf("daemon argv contains forbidden secret flag %q: %#v", forbiddenFlag, args)
		}
	}

	secretPath := parseFlagValue(args, daemonSecretFileFlag, "", "")
	if secretPath == "" {
		t.Fatalf("daemon argv missing %s: %#v", daemonSecretFileFlag, args)
	}

	info, err := os.Stat(secretPath)
	if err != nil {
		t.Fatalf("stat daemon secret file: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("daemon secret file mode = %o, want 0600", info.Mode().Perm())
	}

	payload, err := readDaemonSecretFile(secretPath)
	if err != nil {
		t.Fatalf("readDaemonSecretFile() error = %v", err)
	}
	if payload.Token != authToken || payload.AuthPass != authPass || payload.AuthBearer != authBearer {
		t.Fatalf("daemon secret payload = %#v, want token/auth/auth-bearer secrets", payload)
	}
}

func TestApplyDaemonSecretFileLoadsAndRemovesSecrets(t *testing.T) {
	resetDaemonSecretTestGlobals(t)
	t.Setenv("HOME", t.TempDir())

	path, err := writeDaemonSecretFile(daemonSecretPayload{
		Token:      "child-token",
		AuthPass:   "child-auth",
		AuthBearer: "child-bearer",
	})
	if err != nil {
		t.Fatalf("writeDaemonSecretFile() error = %v", err)
	}

	daemonSecretFile = path
	authToken = "old-token"
	authPass = "old-auth"
	authBearer = "old-bearer"

	if err := applyDaemonSecretFile(); err != nil {
		t.Fatalf("applyDaemonSecretFile() error = %v", err)
	}

	if authToken != "child-token" || authPass != "child-auth" || authBearer != "child-bearer" {
		t.Fatalf("loaded secrets = token:%q auth:%q bearer:%q", authToken, authPass, authBearer)
	}
	if daemonSecretFile != "" {
		t.Fatalf("daemonSecretFile = %q, want cleared", daemonSecretFile)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("daemon secret file still exists or stat failed: %v", err)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func resetDaemonSecretTestGlobals(t *testing.T) {
	t.Helper()

	oldServerURL := serverURL
	oldAuthToken := authToken
	oldAuthPass := authPass
	oldAuthBearer := authBearer
	oldDaemonSecretFile := daemonSecretFile
	oldAllowIPs := append([]string(nil), allowIPs...)
	oldDenyIPs := append([]string(nil), denyIPs...)
	oldTransport := transport
	oldBandwidth := bandwidth
	oldSkipLocalTLSVerify := skipLocalTLSVerify
	oldInsecure := insecure
	oldVerbose := verbose

	t.Cleanup(func() {
		serverURL = oldServerURL
		authToken = oldAuthToken
		authPass = oldAuthPass
		authBearer = oldAuthBearer
		daemonSecretFile = oldDaemonSecretFile
		allowIPs = oldAllowIPs
		denyIPs = oldDenyIPs
		transport = oldTransport
		bandwidth = oldBandwidth
		skipLocalTLSVerify = oldSkipLocalTLSVerify
		insecure = oldInsecure
		verbose = oldVerbose
	})
}
