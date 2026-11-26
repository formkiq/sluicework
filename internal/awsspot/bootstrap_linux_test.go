package awsspot

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// This needs a disposable Linux Docker host, never a live Spot machine. The
// container has no external network, no real runner grant and no host services.
func TestUbuntuBootstrapPrivilegesAndMetadataFirewall(t *testing.T) {
	if os.Getenv("SLUICEWORK_TEST_SPOT_BOOTSTRAP") != "1" {
		t.Skip("set SLUICEWORK_TEST_SPOT_BOOTSTRAP=1 on a disposable Linux Docker host")
	}
	if runtime.GOOS != "linux" {
		t.Fatal("bootstrap integration requires Linux")
	}
	dir := t.TempDir()
	runner, e := os.ReadFile("testdata/fake-runner.sh")
	if e != nil {
		t.Fatal(e)
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if e = tw.WriteHeader(&tar.Header{Name: "run.sh", Mode: 0755, Size: int64(len(runner))}); e != nil {
		t.Fatal(e)
	}
	if _, e = tw.Write(runner); e != nil {
		t.Fatal(e)
	}
	if e = tw.Close(); e != nil {
		t.Fatal(e)
	}
	if e = gz.Close(); e != nil {
		t.Fatal(e)
	}
	s := spec()
	digest := sha256.Sum256(archive.Bytes())
	s.Package.SHA256 = hex.EncodeToString(digest[:])
	script, e := RenderBootstrap(s)
	if e != nil {
		t.Fatal(e)
	}
	for name, data := range map[string][]byte{"runner.tar.gz": archive.Bytes(), "bootstrap.sh": []byte(script)} {
		if e = os.WriteFile(filepath.Join(dir, name), data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	image := "sluicework-bootstrap-test:" + strconv.Itoa(os.Getpid())
	if out, e := exec.Command("docker", "build", "--tag", image, "testdata").CombinedOutput(); e != nil {
		t.Fatalf("build disposable Ubuntu fixture: %v\n%s", e, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "--force", image).Run() })
	for _, mode := range []string{"normal", "root-user", "firewall-failure"} {
		t.Run(mode, func(t *testing.T) {
			args := []string{"run", "--rm", "--network", "none", "--cap-add", "NET_ADMIN",
				"--sysctl", "net.ipv6.conf.all.disable_ipv6=0", "--sysctl", "net.ipv6.conf.default.disable_ipv6=0",
				"--mount", "type=bind,src=" + dir + ",dst=/fixture,readonly", image, mode}
			if out, e := exec.Command("docker", args...).CombinedOutput(); e != nil {
				t.Fatalf("isolated bootstrap %s: %v\n%s", mode, e, out)
			} else {
				t.Log(string(out))
			}
		})
	}
}
