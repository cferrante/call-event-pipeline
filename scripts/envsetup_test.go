package scripts_test

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cferrante/call-event-pipeline/internal/sendgrid"
)

// runSetup copies env-setup.sh and .env.example into a fresh directory, runs the script
// there, and returns the .env it wrote as key/value pairs.
func runSetup(t *testing.T, dir string) map[string]string {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(dir, "scripts", "env-setup.sh"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("env-setup.sh: %v\n%s", err, out)
	} else if strings.Contains(string(out), "MII") || strings.Contains(string(out), "MFk") {
		t.Errorf("env-setup.sh printed key material:\n%s", out)
	}
	f, err := os.Open(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	vals := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok && !strings.HasPrefix(k, "#") {
			vals[k] = v
		}
	}
	return vals
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The simulator's SendGrid key pair is generated into .env: sendhook signs with the private
// key and ingest verifies with the public one, so the two must match. Nothing is printed,
// the file stays private, and re-running never replaces a value.
func TestEnvSetupGeneratesAMatchingSendGridKeyPair(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "env-setup.sh", filepath.Join(dir, "scripts", "env-setup.sh"))
	copyFile(t, "../.env.example", filepath.Join(dir, ".env.example"))

	first := runSetup(t, dir)
	signer, err := sendgrid.ParsePrivateKey(first["SENDGRID_SIMULATOR_PRIVATE_KEY"])
	if err != nil {
		t.Fatalf("private key: %v", err)
	}
	if _, err := sendgrid.ParsePublicKey(first["SENDGRID_PUBLIC_KEY"]); err != nil {
		t.Fatalf("public key: %v", err)
	}
	if signer.PublicKeyBase64() != first["SENDGRID_PUBLIC_KEY"] {
		t.Error("SENDGRID_PUBLIC_KEY doesn't match the simulator's private key")
	}
	if strings.Contains(first["TWILIO_AUTH_TOKEN"], "change-me") {
		t.Error("existing placeholders are no longer replaced")
	}
	info, err := os.Stat(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf(".env mode = %o, want 600", info.Mode().Perm())
	}

	second := runSetup(t, dir)
	for _, k := range []string{"SENDGRID_SIMULATOR_PRIVATE_KEY", "SENDGRID_PUBLIC_KEY", "TWILIO_AUTH_TOKEN"} {
		if second[k] != first[k] {
			t.Errorf("re-running changed %s", k)
		}
	}
}
