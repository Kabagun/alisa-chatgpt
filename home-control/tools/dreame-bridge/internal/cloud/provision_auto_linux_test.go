//go:build linux

package cloud

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestAutomaticCatalogFailureDoesNotSaveOrReplaceSession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"malformed", 200, `private-unparseable-body`},
		{"unauthenticated_http", 401, `private-vendor-detail`},
		{"unauthenticated_envelope", 200, `{"code":401,"data":{}}`},
		{"empty", 200, `{"code":0,"data":{"page":{"records":[]}}}`},
		{"invalid_records", 200, `{"code":0,"data":{"page":{"records":[{"did":"invalid/id","model":"synthetic.vacuum"}]}}}`},
		{"redirect", 302, `private-vendor-detail`},
	} {
		for _, existing := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "_new", true: "_existing"}[existing], func(t *testing.T) {
				dir := privateTestDirectory(t)
				path := filepath.Join(dir, "session.json")
				const sentinel = `{"synthetic_existing_session":"must_remain_unchanged"}`
				if existing {
					if err := os.WriteFile(path, []byte(sentinel), 0600); err != nil {
						t.Fatal(err)
					}
				}
				var logins, catalogs atomic.Int32
				p := mockProvisioner(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == tokenPath {
						logins.Add(1)
						io.WriteString(w, `{"access_token":"synthetic-access","expires_in":3600,"tenant_id":"synthetic-tenant"}`)
						return
					}
					if r.URL.Path != listPath {
						t.Error("unexpected endpoint")
					}
					catalogs.Add(1)
					w.Header().Set("Location", "https://untrusted.invalid/never-follow")
					w.WriteHeader(tc.status)
					io.WriteString(w, tc.body)
				})
				origin := p.originFor
				p.originFor = func(region string) string {
					if region != "eu" {
						t.Error("other origin attempted")
					}
					return origin(region)
				}
				input := loginFixture()
				input.ServerRegion = ""
				if _, err := p.PasswordLoginAndSave(context.Background(), input, path); err == nil || err.Error() != "authentication_connection_unverified" {
					t.Fatal("unverified connection accepted or error exposed")
				}
				if logins.Load() != 1 || catalogs.Load() != 1 {
					t.Fatal("credential/candidate cycling")
				}
				raw, err := os.ReadFile(path)
				if existing {
					if err != nil || string(raw) != sentinel {
						t.Fatal("unverified login replaced session")
					}
				} else if !os.IsNotExist(err) {
					t.Fatal("unverified login saved session")
				}
			})
		}
	}
}

func TestAutomaticVerifiedSessionSavedWithProof(t *testing.T) {
	dir := privateTestDirectory(t)
	path := filepath.Join(dir, "session.json")
	p := mockProvisioner(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			io.WriteString(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600,"tenant_id":"synthetic-tenant"}`)
			return
		}
		if r.URL.Path != listPath {
			t.Error("unexpected endpoint")
		}
		io.WriteString(w, `{"code":0,"data":{"page":{"records":[{"did":"synthetic-1","model":"synthetic.vacuum"}]}}}`)
	})
	input := loginFixture()
	input.ServerRegion = "auto"
	if _, err := p.PasswordLoginAndSave(context.Background(), input, path); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSessionFile(path, p.now())
	if err != nil || s.ServerRegion != "eu" || s.RoutingProof == nil || s.RoutingProof.Method != routingProofMethod {
		t.Fatal("verified proof not protected/persisted")
	}
}
