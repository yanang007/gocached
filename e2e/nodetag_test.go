package e2e_test

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestNodeTagReachesServerMetrics(t *testing.T) {
	url, _ := runServe(t, gocacheDir(t), "GOCACHED_METRICS_PULL=1")
	s := parseStats(t, goBuild(t, t.TempDir(), filepath.Join(t.TempDir(), "out"),
		"GOCACHED_URL="+url, "GOCACHED_NODE_TAG=ci-linux/backend"))
	if s["upload_ok"] == 0 {
		t.Fatalf("the tagged client should upload, got %v", s)
	}
	resp, err := http.Get(url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `node_tag="ci-linux/backend",op="put",result="stored"`) {
		t.Fatalf("server metrics lack the node tag:\n%s", body)
	}
}

func TestInvalidNodeTagFailsFast(t *testing.T) {
	out := runFailing(t, []string{"prog"}, "GOCACHED_DIR="+t.TempDir(), "GOCACHED_NODE_TAG=has space")
	if !strings.Contains(out, "GOCACHED_NODE_TAG") {
		t.Fatalf("output does not name the variable:\n%s", out)
	}
}
