package upf

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTLSInsecureSkipVerify：自签证书的 HTTPS mock UPF；
// insecureSkipVerify=true 能拿到 token，默认校验拿不到（x509 报错）。
func TestTLSInsecureSkipVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/upf/v2/user/token" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`"test-token"`))
	}))
	defer srv.Close()

	insecure, err := New(srv.URL, "ehgw", "04f5349605824dec612bd1493efdc923", 5*time.Second, 30, 300, TLSConfig{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("New with insecureSkipVerify: %v", err)
	}
	tok, err := insecure.GetToken(context.Background())
	if err != nil {
		t.Fatalf("GetToken with insecureSkipVerify=true: %v", err)
	}
	if tok != "test-token" {
		t.Fatalf("token = %q, want %q", tok, "test-token")
	}

	strict, err := New(srv.URL, "ehgw", "04f5349605824dec612bd1493efdc923", 5*time.Second, 30, 300, TLSConfig{})
	if err != nil {
		t.Fatalf("New strict: %v", err)
	}
	if _, err := strict.GetToken(context.Background()); err == nil {
		t.Fatalf("GetToken with verify on self-signed cert: got nil error, want x509 error")
	}

	// CA 文件打不开要在 New 里就报错，不静默回退到系统根证书。
	if _, err := New(srv.URL, "ehgw", "x", 5*time.Second, 30, 300, TLSConfig{CAFile: "/nonexistent-ca.pem"}); err == nil {
		t.Fatalf("New with missing caFile: got nil error, want error")
	}
}

// TestTLSKeyLogFile：配了 keyLogFile 时，一次 HTTPS 请求后文件里应有会话密钥
// （TLS 1.3 写 *_TRAFFIC_SECRET，TLS 1.2 写 CLIENT_RANDOM）；路径打不开时
// New 直接报错，不静默丢弃。
func TestTLSKeyLogFile(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`"test-token"`))
	}))
	defer srv.Close()

	keyLog := filepath.Join(t.TempDir(), "sslkey.log")
	cli, err := New(srv.URL, "ehgw", "04f5349605824dec612bd1493efdc923", 5*time.Second, 30, 300,
		TLSConfig{InsecureSkipVerify: true, KeyLogFile: keyLog})
	if err != nil {
		t.Fatalf("New with keyLogFile: %v", err)
	}
	if _, err := cli.GetToken(context.Background()); err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	data, err := os.ReadFile(keyLog)
	if err != nil {
		t.Fatalf("read key log: %v", err)
	}
	if !strings.Contains(string(data), "CLIENT_RANDOM") && !strings.Contains(string(data), "TRAFFIC_SECRET") {
		t.Fatalf("key log has no session secrets: %q", string(data))
	}

	// keyLogFile 打不开要在 New 里就报错，不静默丢弃。
	badPath := filepath.Join(t.TempDir(), "no-such-dir", "sslkey.log")
	if _, err := New(srv.URL, "ehgw", "x", 5*time.Second, 30, 300,
		TLSConfig{InsecureSkipVerify: true, KeyLogFile: badPath}); err == nil {
		t.Fatalf("New with bad keyLogFile: got nil error, want error")
	}
}
